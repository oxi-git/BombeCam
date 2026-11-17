package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Fever-r/BombeCam/pkg/bridge"
	"github.com/Fever-r/BombeCam/pkg/profile"
)

func defaultProfilePaths() (string, string) {
	if runtime.GOOS == "windows" {
		localAppData := os.Getenv("LOCALAPPDATA")
		if localAppData == "" {
			userProfile := os.Getenv("USERPROFILE")
			if userProfile == "" {
				userProfile = "."
			}
			localAppData = filepath.Join(userProfile, "AppData", "Local")
		}
		dir := filepath.Join(localAppData, "Bombecam")
		_ = os.MkdirAll(dir, 0700)
		return filepath.Join(dir, "profile.enc"), filepath.Join(dir, "profile.key")
	}
	// Service/Docker installs use /var/lib/bombecam; a normal user account
	// cannot create that, so fall back to the XDG data directory.
	dir := "/var/lib/bombecam"
	if err := os.MkdirAll(dir, 0700); err != nil || !dirWritable(dir) {
		base := os.Getenv("XDG_DATA_HOME")
		if base == "" {
			if home, herr := os.UserHomeDir(); herr == nil {
				base = filepath.Join(home, ".local", "share")
			}
		}
		if base != "" {
			dir = filepath.Join(base, "bombecam")
			_ = os.MkdirAll(dir, 0700)
		}
	}
	return filepath.Join(dir, "profile.enc"), filepath.Join(dir, "profile.key")
}

func dirWritable(dir string) bool {
	f, err := os.CreateTemp(dir, ".probe-*")
	if err != nil {
		return false
	}
	name := f.Name()
	_ = f.Close()
	_ = os.Remove(name)
	return true
}

var (
	defaultProfPath, defaultKeyFilePath = defaultProfilePaths()
	profilePathFlag                     = flag.String("profile-path", getEnvOrDefault("BOMBECAM_PROFILE_PATH", defaultProfPath), "Path to encrypted user profile (or env BOMBECAM_PROFILE_PATH)")
	profileKeyFlag                      = flag.String("profile-key", getEnvOrDefault("BOMBECAM_PROFILE_KEY", ""), "Profile master key or passphrase (or env BOMBECAM_PROFILE_KEY)")
	profileKeyFileFlag                  = flag.String("profile-key-file", getEnvOrDefault("BOMBECAM_PROFILE_KEY_FILE", defaultKeyFilePath), "Path to master encryption keyfile (or env BOMBECAM_PROFILE_KEY_FILE)")
)

var (
	activeProfileMu sync.RWMutex
	activeProfilePM profile.ProfileManager
)

// ActiveProfileManager returns the currently active ProfileManager instance.
func ActiveProfileManager() profile.ProfileManager {
	activeProfileMu.RLock()
	defer activeProfileMu.RUnlock()
	return activeProfilePM
}

// SetActiveProfileManager sets the global ProfileManager instance.
func SetActiveProfileManager(pm profile.ProfileManager) {
	activeProfileMu.Lock()
	defer activeProfileMu.Unlock()
	activeProfilePM = pm
}

// runGatewayStartup coordinates startup profile detection, first-run bootstrap, and restart restoration.
func runGatewayStartup(sm *SessionManager, stm *StreamManager, email, password, country, enrolledF, deviceSel, httpAddr string) {
	ctx := context.Background()
	profilePath := *profilePathFlag
	keyFlag := *profileKeyFlag
	keyFileFlag := *profileKeyFileFlag

	if dir := filepath.Dir(profilePath); dir != "" && dir != "." {
		_ = os.MkdirAll(dir, 0700)
	}
	if dir := filepath.Dir(keyFileFlag); dir != "" && dir != "." {
		_ = os.MkdirAll(dir, 0700)
	}

	// The manager main already installed on stm is the one to use; otherwise
	// build it here, as only this path knows where the profile lives.
	pm := stm.ProfileManager()
	if pm == nil {
		var err error
		pm, err = profile.NewDefaultManager(profilePath, keyFlag, keyFileFlag)
		if err != nil || pm == nil {
			fmt.Printf("[profile] error: failed to initialize key provider: %v\n", err)
			sm.SetStatus(SessionStatusUnavailable, fmt.Sprintf("profile key initialization failed: %v", err))
			return
		}
		stm.SetProfileManager(pm)
	}
	SetActiveProfileManager(pm)

	hasProfile, err := pm.HasProfile(ctx)
	if err != nil {
		fmt.Printf("[profile] error checking profile existence: %v\n", err)
	}

	resume := func() {
		runGatewayStartupSafely(sm, stm, email, password, country, enrolledF, deviceSel, httpAddr)
	}

	if !hasProfile {
		// First-run mode
		fmt.Printf("[profile] no encrypted profile found at %s (first-run mode active)\n", profilePath)
		if email != "" && password != "" {
			if strings.TrimSpace(country) == "" {
				sm.SetStatus(SessionStatusUnavailable, "OSAIO_COUNTRY is required for this account; no country was inferred")
				return
			}
			if waitForOsaioValues(sm, resume) {
				return
			}
			// If Osaio refuses the login because the server key or app ID
			// is outdated, saving a new one on the page tries it again
			// (unless a login was added on the page meanwhile).
			gen := osaioChanges.Load()
			retry := func() {
				if has, _ := pm.HasProfile(context.Background()); !has {
					resume()
				}
			}
			fmt.Printf("[profile] bootstrapping initial encrypted profile from CLI/env credentials for %s...\n", email)
			if !bootstrapProfileFromCredentials(ctx, sm, stm, pm, email, password, country, enrolledF, deviceSel) {
				retryStartupAfterChange(gen, retry)
			}
		} else {
			fmt.Printf("[profile] first-run: no credentials configured (HTTP server active on %s, awaiting onboarding API)\n", httpAddr)
			sm.SetStatus(SessionStatusCredentialsMissing, "first-run: no profile configured; complete onboarding via API")
		}
		return
	}

	// Profile exists: restore gateway state across restart
	fmt.Printf("[profile] loading encrypted profile from %s...\n", profilePath)
	prof, err := pm.Load(ctx)
	if err != nil {
		fmt.Printf("[profile] error: failed to decrypt/load profile: %v\n", err)
		sm.SetStatus(SessionStatusUnavailable, "profile decryption failed: invalid key or corrupted file")
		return
	}

	fmt.Printf("[profile] successfully loaded profile (schema v%d, logins=%d, cameras=%d)\n",
		prof.Version, len(prof.Accounts), len(prof.Cameras))

	if waitForOsaioValues(sm, resume) {
		return
	}
	restoreGatewayState(ctx, sm, stm, pm, prof)
}

// bootstrapProfileFromCredentials adds the login given on the command line
// (or in OSAIO_EMAIL / OSAIO_PASSWORD) to the pool, like a first sign-in on
// the page, and adds the cameras named by -enrolled or -device. It reports
// false when the login was not signed in, which a new server key or app ID
// may fix.
func bootstrapProfileFromCredentials(ctx context.Context, sm *SessionManager, stm *StreamManager, pm profile.ProfileManager, email, password, country, enrolledF, deviceSel string) bool {
	fmt.Printf("[session] authenticating account %s...\n", email)
	reg := stm.Registry()
	session, fail := signInToPool(ctx, reg, pm, email, password, country)
	if fail != nil {
		fmt.Printf("[session] login failed: %s\n", fail.Message)
		if session != nil {
			return true // signed in; only saving it failed
		}
		status := SessionStatusInvalidCredentials
		switch fail.Code {
		case "app_id_missing":
			status = SessionStatusAppIDMissing
		case "server_key_missing":
			status = SessionStatusServerKeyMissing
		}
		sm.SetStatus(status, fail.Message)
		return false
	}
	fmt.Printf("[session] authenticated successfully. UID=%s\n", session.GetStatus().VendorUID)

	devs, _, err := session.DeviceList(true)
	if err != nil {
		fmt.Printf("[session] initial device discovery failed: %v\n", err)
		return true
	}
	fmt.Printf("\n=== %d DISCOVERED CAMERAS ===\n", len(devs))
	for _, d := range devs {
		fmt.Printf("  - %-20s | %-15s | rtsp path=/%s online=%d\n", d.Name, d.Type, d.UUID, d.Online)
	}

	var initialEnroll []string
	if enrolledF != "" {
		for _, id := range strings.Split(enrolledF, ",") {
			if trimmed := strings.TrimSpace(id); trimmed != "" {
				initialEnroll = append(initialEnroll, trimmed)
			}
		}
	} else if deviceSel != "" {
		initialEnroll = append(initialEnroll, deviceSel)
	}

	camerasMap := make(map[string]profile.CameraProfile)
	if len(initialEnroll) > 0 {
		enrolled, err := stm.Enroll(initialEnroll, devs)
		if err != nil {
			fmt.Printf("[stream] startup enrollment error: %v\n", err)
		} else {
			fmt.Printf("[stream] enrolled %d camera(s) at startup: %v\n", len(enrolled), enrolled)
			for _, id := range enrolled {
				for _, d := range devs {
					if d.UUID == id {
						camerasMap[id] = profile.CameraProfile{
							UUID:         d.UUID,
							AccountEmail: profile.NormalizeEmail(email),
							Name:         d.Name,
							Model:        d.Type,
							EnrolledAt:   time.Now().UTC(),
							Online:       d.Online == 1,
						}
						break
					}
				}
			}
		}
	}

	var allowedSubnets []string
	if stm != nil {
		for _, sn := range stm.AllowedSubnets() {
			if sn != nil {
				allowedSubnets = append(allowedSubnets, sn.String())
			}
		}
	}
	cloud := session.Cloud()
	if _, err := pm.Update(ctx, func(p *profile.Profile) error {
		if p.Cameras == nil {
			p.Cameras = make(map[string]profile.CameraProfile)
		}
		for id, cam := range camerasMap {
			p.Cameras[id] = cam
		}
		p.Connection.AllowedSubnets = allowedSubnets
		p.Connection.Timezone = cloud.TimezoneName
		p.Connection.TimezoneOffset = cloud.ZoneOffset
		return nil
	}); err != nil {
		fmt.Printf("[profile] error saving initial profile: %v\n", err)
	} else {
		syncStreamNames(ctx, pm, stm)
		fmt.Printf("[profile] initial encrypted profile saved successfully for %s\n", email)
	}
	return true
}

// restoreGatewayState brings back every saved Osaio login and the cameras
// that were added, and nothing else. Every login is restored the same way:
// from its saved sign-in if Osaio still accepts it, else with its saved
// password. A login that fails only affects its own cameras. Cameras a login
// can see but that were never added stay out. The camera firewall lives on
// the router and keeps itself in force across reboots, so there is nothing
// to re-apply here.
func restoreGatewayState(ctx context.Context, sm *SessionManager, stm *StreamManager, pm profile.ProfileManager, prof *profile.Profile) {
	if b := prof.Privacy.BlockCloudVideo; b != nil {
		fmt.Printf("[privacy] Block cloud video was last applied as %s on router %s\n", yesNoWord(*b), prof.Privacy.RouterAddress)
	}

	// Subnets and time zone are this PC's settings. The time zone goes on
	// the template that every login's session is made from.
	if len(prof.Connection.AllowedSubnets) > 0 {
		var subnets []*net.IPNet
		for _, cidr := range prof.Connection.AllowedSubnets {
			if _, ipNet, err := net.ParseCIDR(cidr); err == nil {
				subnets = append(subnets, ipNet)
			}
		}
		if len(subnets) > 0 {
			stm.SetAllowedSubnets(subnets)
		}
	}
	if prof.Connection.Timezone != "" {
		applyTimezoneConfig(sm.Cloud(), prof.Connection.Timezone, fmt.Sprintf("%.2f", prof.Connection.TimezoneOffset))
	}

	reg := stm.Registry()
	if len(prof.Accounts) == 0 {
		fmt.Println("[session] no Osaio login saved; awaiting sign-in on the web page")
		sm.SetStatus(SessionStatusCredentialsMissing, "first-run: no profile configured; complete onboarding via API")
		return
	}

	// Sign every saved login in, side by side. Give them a while before
	// setting up cameras (live names and models are better than saved
	// ones); a login still waiting for Osaio carries on in the background
	// and its cameras start as soon as it is signed in.
	reg.SetRestoring(true)
	var wg sync.WaitGroup
	for _, acct := range prof.Accounts {
		wg.Add(1)
		go func(acct profile.CloudCredentials) {
			defer wg.Done()
			restoreLogin(ctx, reg, pm, acct)
		}(acct)
	}
	done := make(chan struct{})
	go func() {
		wg.Wait()
		reg.SetRestoring(false)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(restoreLoginWait):
		fmt.Println("[session] some Osaio logins are still signing in; their cameras start when they are")
	}

	// The saved cameras, and only those. Live details from each camera's
	// own login replace the saved ones where available.
	live := map[string]bridge.Device{}
	for _, acct := range prof.Accounts {
		session, ok := reg.Get(acct.AccountEmail)
		if !ok || !session.IsAuthenticated() {
			continue
		}
		devs, _, err := session.DeviceList(false)
		if err != nil {
			continue
		}
		for _, msg := range missingFromAccount(prof, acct.AccountEmail, devs) {
			fmt.Println("[cameras] " + msg)
		}
		for _, d := range devs {
			if cam, saved := prof.Cameras[d.UUID]; saved && profile.NormalizeEmail(cam.AccountEmail) == profile.NormalizeEmail(acct.AccountEmail) {
				live[d.UUID] = d
			}
		}
	}
	var enrolledIDs []string
	var inventory []bridge.Device
	for uuid, cam := range prof.Cameras {
		enrolledIDs = append(enrolledIDs, uuid)
		if d, ok := live[uuid]; ok {
			inventory = append(inventory, d)
		} else {
			inventory = append(inventory, bridge.Device{UUID: cam.UUID, Name: cam.Name, Type: cam.Model, Model: 1, Online: 1})
		}
	}

	if len(enrolledIDs) > 0 {
		enrolled, err := stm.Enroll(enrolledIDs, inventory)
		if err != nil {
			fmt.Printf("[stream] restart enrollment error: %v\n", err)
		} else {
			fmt.Printf("[stream] restored %d enrolled camera(s) from profile: %v\n", len(enrolled), enrolled)
			// Pre-seed known LAN IPs
			for _, id := range enrolled {
				if cam, ok := prof.Cameras[id]; ok && cam.IPAddress != "" {
					if mc, found := stm.GetCamera(id); found {
						mc.mu.Lock()
						mc.IP = cam.IPAddress
						mc.mu.Unlock()
					}
				}
			}
		}
	} else {
		fmt.Println("[stream] zero cameras enrolled in profile (awaiting enrollment via API)")
	}
	syncStreamNames(ctx, pm, stm)
	kickIntegrations()
}

// restoreLoginWait is how long start-up waits for the saved logins to sign
// in before setting up cameras.
var restoreLoginWait = 45 * time.Second

// restoreLogin signs one saved login in: with its saved sign-in if Osaio
// still accepts it, else with its saved password. While Osaio can't be
// reached (the PC just booted) it keeps trying; only a refused password or
// a missing server key or app ID needs the user.
func restoreLogin(ctx context.Context, reg *SessionRegistry, pm profile.ProfileManager, acct profile.CloudCredentials) {
	email := profile.NormalizeEmail(acct.AccountEmail)
	session := reg.Ensure(email)
	if acct.Country != "" {
		session.SetCountry(acct.Country)
	}
	if acct.PhoneCode != "" {
		session.SetPhoneCode(acct.PhoneCode)
	}
	tokenValid := !acct.AuthToken.IsEmpty() && (acct.TokenExpiresAt.IsZero() || time.Now().Before(acct.TokenExpiresAt))
	if tokenValid {
		c := session.Cloud()
		c.UID = acct.VendorUID
		c.APIToken = acct.AuthToken.Expose()
		if acct.Region != "" {
			c.Web = acct.Region
		}
		c.GetBaseURL(email)
		session.Adopt(email, acct.Country, c)
		if _, _, err := session.DeviceList(false); err == nil {
			fmt.Printf("[session] %s: saved sign-in still valid\n", email)
			return
		} else {
			fmt.Printf("[session] %s: saved sign-in no longer accepted: %v\n", email, err)
		}
	}
	if acct.Password.IsEmpty() {
		fmt.Printf("[session] %s: no saved password; enter it on the Osaio logins page\n", email)
		session.SetStatus(SessionStatusInvalidCredentials, "No password is saved for this login. Enter it on the Osaio logins page.")
		return
	}
	delay := 5 * time.Second
	for attempt := 1; ; attempt++ {
		fmt.Printf("[session] %s: signing in with the saved password...\n", email)
		err := session.Login(email, acct.Password.Expose())
		if err == nil {
			break
		}
		st := session.GetStatus()
		if st.Status == SessionStatusInvalidCredentials || st.Status == SessionStatusCredentialsMissing || st.Status == SessionStatusServerKeyMissing || st.Status == SessionStatusAppIDMissing {
			fmt.Printf("[session] %s: sign-in failed: %v\n", email, err)
			return
		}
		fmt.Printf("[session] %s: Osaio not reachable (%v); retrying in %v\n", email, err, delay)
		session.SetStatus(SessionStatusAuthenticating, fmt.Sprintf("waiting for the vendor cloud (attempt %d): %s", attempt, st.Error))
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return
		}
		if delay < time.Minute {
			delay *= 2
		}
		if cur, ok := reg.Get(email); !ok || cur != session || cur.GetStatus().Status != SessionStatusAuthenticating {
			return // signed in again (or removed) from the page meanwhile
		}
	}
	fmt.Printf("[session] %s: signed in (UID: %s)\n", email, session.GetStatus().VendorUID)
	saveLoginToken(email, session.Cloud(), currentStreamManager())
}

// missingFromAccount explains each saved camera of one login that the login
// no longer lists. A camera that is reset and paired again only comes back on
// the account it was paired to (a share from another account ends with the
// reset), and until then every stream attempt fails with a bare vendor code.
func missingFromAccount(prof *profile.Profile, email string, live []bridge.Device) []string {
	if prof == nil {
		return nil
	}
	email = profile.NormalizeEmail(email)
	have := make(map[string]bool, len(live))
	for _, d := range live {
		have[d.UUID] = true
	}
	var out []string
	for uuid, cam := range prof.Cameras {
		if have[uuid] || profile.NormalizeEmail(cam.AccountEmail) != email {
			continue
		}
		name := cam.Name
		if name == "" {
			name = uuid
		}
		out = append(out, fmt.Sprintf("%s is not on the Osaio account %s right now, so it can't stream. "+
			"If the camera was reset and paired again, pair it to this account (or share it to this account) in the Osaio app, then restart BombeCam.",
			name, email))
	}
	sort.Strings(out)
	return out
}

// runGatewayStartupSafely runs the startup sequence and keeps BombeCam open
// if it fails unexpectedly: the reason and stack go to gateway.log and the
// web page shows the problem instead of the window closing.
func runGatewayStartupSafely(sm *SessionManager, stm *StreamManager, email, password, country, enrolledF, deviceSel, httpAddr string) {
	guardStartup(sm, func() {
		runGatewayStartup(sm, stm, email, password, country, enrolledF, deviceSel, httpAddr)
	})
}

func guardStartup(sm *SessionManager, run func()) {
	defer func() {
		if r := recover(); r != nil {
			fmt.Printf("[startup] unexpected error while restoring the saved session: %v\n%s", r, debug.Stack())
			sm.SetStatus(SessionStatusUnavailable, fmt.Sprintf("BombeCam hit an unexpected error while starting (%v). Details are in gateway.log; sign in again to continue.", r))
		}
	}()
	run()
}
