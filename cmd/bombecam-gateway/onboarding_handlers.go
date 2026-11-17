package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/Fever-r/BombeCam/internal/mediamtx"
	"github.com/Fever-r/BombeCam/pkg/bridge"
	"github.com/Fever-r/BombeCam/pkg/profile"
)

// ----------------------------------------------------------------------------
// Onboarding API Endpoints
// ----------------------------------------------------------------------------

// handleOnboardingStatus handles GET /api/v1/onboarding/status.
func handleOnboardingStatus(w http.ResponseWriter, r *http.Request, sessionMgr *SessionManager, streamMgr *StreamManager) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET required", http.StatusMethodNotAllowed)
		return
	}

	pm := streamMgr.ProfileManager()
	hasProfile := false
	var prof *profile.Profile
	if pm != nil {
		if hp, err := pm.HasProfile(r.Context()); err == nil && hp {
			hasProfile = true
			prof = pm.GetProfile()
			if prof == nil {
				if loaded, err := pm.Load(r.Context()); err == nil {
					prof = loaded
				}
			}
		}
	}

	reg := streamMgr.Registry()
	sessionInfo := reg.Status()

	loginsCount := 0
	if prof != nil {
		loginsCount = len(prof.Accounts)
	}

	enrolledCount := streamMgr.CameraCount()
	if enrolledCount == 0 && prof != nil {
		enrolledCount = len(prof.Cameras)
	}

	activeStreams := streamMgr.ActiveStreamCount()
	readyForDiscovery := reg.AnyAuthenticated()

	errStr := sessionInfo.Error
	if !hasProfile && errStr == "" {
		errStr = "first-run: no profile configured; complete onboarding via API"
	}

	resp := map[string]any{
		"has_profile":            hasProfile,
		"first_run":              !hasProfile,
		"session_status":         sessionInfo.Status,
		"block_cloud_video":      privacyChoice(prof),
		"enrolled_cameras_count": enrolledCount,
		"logins_count":           loginsCount,
		"active_stream_count":    activeStreams,
		"ready_for_discovery":    readyForDiscovery,
		"server_key":             serverKeyStatus(),
		"app_id":                 appIDStatus(),
		"error":                  errStr,
	}

	writeJSON(w, http.StatusOK, resp)
}

// detectHostCountry returns a validated environment value or known locale region.
// Unknown locale and language-only values stay unknown.
func detectHostCountry() string {
	if envC := strings.TrimSpace(os.Getenv("OSAIO_COUNTRY")); envC != "" {
		if country, ok := normalizedCountry(envC); ok {
			return country
		}
		return ""
	}
	for _, envKey := range []string{"LC_ALL", "LC_MESSAGES", "LANG"} {
		v := strings.TrimSpace(os.Getenv(envKey))
		if v == "" {
			continue
		}
		var region string
		if idx := strings.IndexAny(v, "_-"); idx != -1 {
			rest := v[idx+1:]
			if dotIdx := strings.Index(rest, "."); dotIdx != -1 {
				region = rest[:dotIdx]
			} else if atIdx := strings.Index(rest, "@"); atIdx != -1 {
				region = rest[:atIdx]
			} else {
				region = rest
			}
		}
		region = strings.ToUpper(strings.TrimSpace(region))
		switch region {
		case "US", "CA":
			return "1"
		case "GB", "UK":
			return "44"
		case "FR":
			return "33"
		case "DE":
			return "49"
		case "ES":
			return "34"
		case "IT":
			return "39"
		case "JP":
			return "81"
		case "CN":
			return "86"
		case "AU":
			return "61"
		}
	}
	return ""
}

// detectHostTimezone resolves local timezone name from request, environment, existing profile, or host OS.
func detectHostTimezone(existingProf *profile.Profile) string {
	if tz := strings.TrimSpace(os.Getenv("OSAIO_TIMEZONE")); tz != "" {
		return tz
	}
	if tz := strings.TrimSpace(os.Getenv("TZ")); tz != "" {
		return tz
	}
	if existingProf != nil && strings.TrimSpace(existingProf.Connection.Timezone) != "" {
		return strings.TrimSpace(existingProf.Connection.Timezone)
	}
	tz := time.Now().Location().String()
	if tz == "" || tz == "Local" {
		name, _ := time.Now().Zone()
		if name != "" {
			tz = name
		} else {
			tz = "UTC"
		}
	}
	return tz
}

// detectHostTimezoneOffset resolves local timezone offset hours from environment or host OS.
func detectHostTimezoneOffset() float64 {
	if v := strings.TrimSpace(os.Getenv("OSAIO_ZONE_OFFSET")); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f >= -12.0 && f <= 14.0 {
			return f
		}
	}
	_, offsetSec := time.Now().Zone()
	return float64(offsetSec) / 3600.0
}

// OnboardingSetupRequest represents the request payload for POST /api/v1/onboarding/setup.
type OnboardingSetupRequest struct {
	AccountEmail   string   `json:"account_email"`
	Password       string   `json:"password"`
	Country        string   `json:"country"`
	AllowedSubnets []string `json:"allowed_subnets"`
	Timezone       string   `json:"timezone"`
	TimezoneOffset *float64 `json:"timezone_offset"`
	Force          bool     `json:"force"`
}

// handleOnboardingSetup handles POST /api/v1/onboarding/setup: the first-run
// quick start. It adds a login to the pool exactly like Osaio logins > Add a
// login does (other stored logins are not touched) and records this PC's
// time zone and camera subnets.
func handleOnboardingSetup(w http.ResponseWriter, r *http.Request, sessionMgr *SessionManager, streamMgr *StreamManager) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}

	var req OnboardingSetupRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid json payload"})
		return
	}

	pm := streamMgr.ProfileManager()
	if pm == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "profile_unavailable", "message": "Profile storage is unavailable. Restore the saved key before signing in."})
		return
	}
	var existingProf *profile.Profile
	if pm != nil {
		if p, err := pm.Load(r.Context()); err == nil && p != nil {
			existingProf = p
		} else if err != nil && !errors.Is(err, profile.ErrProfileNotFound) {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "profile_unavailable", "message": "The saved profile could not be read. Restore it before changing accounts."})
			return
		}
	}

	req.AccountEmail = strings.TrimSpace(req.AccountEmail)
	req.Password = strings.TrimSpace(req.Password)
	if req.AccountEmail == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "account_email is required"})
		return
	}
	if req.Password == "" {
		var stored profile.CloudCredentials
		found := false
		if existingProf != nil {
			stored, found = existingProf.FindAccount(req.AccountEmail)
		}
		if found && !stored.Password.IsEmpty() {
			req.Password = stored.Password.Expose()
		} else {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "password is required"})
			return
		}
	}

	if pm != nil {
		if hp, err := pm.HasProfile(r.Context()); err == nil && hp && !req.Force {
			writeJSON(w, http.StatusConflict, map[string]any{"error": "profile already exists; pass force: true to overwrite"})
			return
		}
	}

	reg := streamMgr.Registry()
	country, countryErr := resolveAccountCountry(req.Country, req.AccountEmail, existingProf, countrySession(reg, req.AccountEmail))
	if countryErr != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "country_required", "message": countryErr.Error()})
		return
	}

	// The time zone goes on the template, so every login's session uses it.
	tz := strings.TrimSpace(req.Timezone)
	if tz == "" {
		tz = detectHostTimezone(existingProf)
	}
	var offsetVal float64
	if req.TimezoneOffset != nil {
		offsetVal = *req.TimezoneOffset
	} else {
		offsetVal = detectHostTimezoneOffset()
	}
	if cloud := sessionMgr.Cloud(); cloud != nil {
		applyTimezoneConfig(cloud, tz, fmt.Sprintf("%.2f", offsetVal))
	}

	session, fail := signInToPool(r.Context(), reg, pm, req.AccountEmail, req.Password, country)
	if fail != nil {
		fail.write(w)
		return
	}
	cloud := session.Cloud()

	// Apply subnets if provided
	if len(req.AllowedSubnets) > 0 {
		var subnets []*net.IPNet
		for _, cidr := range req.AllowedSubnets {
			if _, ipNet, err := net.ParseCIDR(strings.TrimSpace(cidr)); err == nil {
				subnets = append(subnets, ipNet)
			}
		}
		if len(subnets) > 0 {
			streamMgr.SetAllowedSubnets(subnets)
		}
	}
	subnetsList := req.AllowedSubnets
	if len(subnetsList) == 0 {
		for _, sn := range streamMgr.AllowedSubnets() {
			if sn != nil {
				subnetsList = append(subnetsList, sn.String())
			}
		}
	}
	tzName, tzOffset := tz, offsetVal
	if cloud != nil {
		if cloud.TimezoneName != "" {
			tzName = cloud.TimezoneName
		}
		tzOffset = cloud.ZoneOffset
	}
	if _, err := pm.Update(r.Context(), func(p *profile.Profile) error {
		p.Connection.AllowedSubnets = subnetsList
		p.Connection.Timezone = tzName
		p.Connection.TimezoneOffset = tzOffset
		return nil
	}); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": fmt.Sprintf("failed to save profile: %v", err)})
		return
	}

	// Fetch this login's camera list in the background.
	go func() {
		if _, _, err := session.DeviceList(true); err != nil {
			fmt.Printf("[onboarding] could not refresh the camera list: %v\n", err)
		}
	}()

	respMap := map[string]any{
		"ok":            true,
		"status":        "configured",
		"account_email": profile.NormalizeEmail(req.AccountEmail),
		"message":       "login added; proceed to camera discovery",
	}

	// Respond with sanitized status (strictly zero secrets leaked)
	writeJSON(w, http.StatusOK, respMap)
}

// handleOnboardingProfile handles GET /api/v1/onboarding/profile.
func handleOnboardingProfile(w http.ResponseWriter, r *http.Request, streamMgr *StreamManager) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET required", http.StatusMethodNotAllowed)
		return
	}

	pm := streamMgr.ProfileManager()
	if pm == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "profile not found: complete first-run setup first"})
		return
	}

	hasProfile, err := pm.HasProfile(r.Context())
	if err != nil || !hasProfile {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "profile not found: complete first-run setup first"})
		return
	}

	view := pm.GetPublicView()
	writeJSON(w, http.StatusOK, view)
}

// clearAllButAdmin empties the profile except BombeCam's administrator
// (sign-in, two-step, hint, the sign-in-on-this-PC choice). It reports false
// when there is no administrator to keep or the profile can't be read; the
// caller then deletes the file.
func clearAllButAdmin(ctx context.Context, pm profile.ProfileManager) bool {
	prof := pm.GetProfile()
	if prof == nil {
		return false
	}
	if _, has := prof.Admin(); !has {
		return false
	}
	_, err := pm.Update(ctx, func(p *profile.Profile) error {
		*p = profile.Profile{
			Version:   profile.CurrentSchemaVersion,
			CreatedAt: p.CreatedAt,
			Cameras:   map[string]profile.CameraProfile{},
			Users:     p.Users,
			Security:  p.Security,
		}
		return nil
	})
	return err == nil
}

// handleOnboardingReset handles DELETE /api/v1/onboarding/profile and POST /api/v1/onboarding/reset:
// Delete all stored information. Osaio logins, cameras, router rules and key,
// and settings go; the administrator's sign-in stays (it changes only under
// Sign-in settings).
func handleOnboardingReset(w http.ResponseWriter, r *http.Request, sessionMgr *SessionManager, streamMgr *StreamManager) {
	if r.Method != http.MethodDelete && r.Method != http.MethodPost {
		http.Error(w, "DELETE or POST required", http.StatusMethodNotAllowed)
		return
	}

	pm := streamMgr.ProfileManager()
	writeJSON(w, http.StatusOK, resetStoredInformation(r.Context(), sessionMgr, streamMgr, pm, true))
}

// resetStoredInformation stops every stream, takes BombeCam's rules and key
// off the router, and empties the saved data. With keepAdmin the
// administrator's sign-in stays (Delete all stored information); without it
// the saved data is deleted entirely (starting over after a forgotten
// password).
func resetStoredInformation(ctx context.Context, sessionMgr *SessionManager, streamMgr *StreamManager, pm profile.ProfileManager, keepAdmin bool) map[string]any {
	streamMgr.CloseAll()

	// Every camera is being removed, so BombeCam's rules and key come off the
	// router first (while BombeCam still has the key to do it).
	routerMsg := "No router was set up, so nothing on a router changed."
	routerUpdated := true
	if pm != nil && pm.GetProfile() != nil {
		rctx, cancel := context.WithTimeout(context.Background(), changeTimeout)
		o := uninstallFromRouter(rctx, pm)
		cancel()
		switch {
		case o.OK && o.Changed:
			routerMsg = "BombeCam's rules and key were removed from your router."
		case !o.OK:
			routerUpdated = false
			routerMsg = "Your router still has BombeCam's rules: " + o.Message
		}
	}
	var oldInteg profile.IntegrationSettings
	keptAdmin := false
	if pm != nil {
		if prof := pm.GetProfile(); prof != nil {
			oldInteg = prof.Integrations
		}
		if keepAdmin {
			keptAdmin = clearAllButAdmin(ctx, pm)
		}
		if !keptAdmin {
			_ = pm.Delete(ctx)
		}
	}
	// Frigate / Home Assistant settings go with the profile: back to defaults
	// (MQTT devices are removed from Home Assistant, the snapshot listener
	// closes, and MediaMTX returns to its default ports without a password).
	applyIntegrationRuntime(streamMgr, profile.IntegrationSettings{})
	if mediaSettingsDiffer(oldInteg, profile.IntegrationSettings{}) {
		if locked, _ := portsLocked(); !locked {
			go func() {
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				if err := reconfigureMedia(ctx, streamMgr, profile.IntegrationSettings{}); err != nil {
					fmt.Printf("[mediamtx] could not return to the default settings: %v\n", err)
				}
			}()
		}
	}

	sessionMgr.Reset()
	if reg := streamMgr.Registry(); reg != nil {
		reg.Clear()
	}
	// Without an administrator to keep, nobody stays signed in.
	if om := GatewayOperatorManager(); om != nil && !keptAdmin {
		om.RevokeOperatorSessions()
	}

	return map[string]any{
		"ok":             true,
		"status":         "reset",
		"message":        "stored information deleted. " + routerMsg,
		"admin_kept":     keptAdmin,
		"router_updated": routerUpdated,
		"router_message": routerMsg,
	}
}

// ----------------------------------------------------------------------------
// Camera Discovery & Selective Enrollment
// ----------------------------------------------------------------------------

// handleCameraDiscover handles GET /api/v1/onboarding/cameras/discover and GET /api/v1/inventory/discover.
func handleCameraDiscover(w http.ResponseWriter, r *http.Request, sessionMgr *SessionManager, streamMgr *StreamManager) {

	if r.Method != http.MethodGet {
		http.Error(w, "GET required", http.StatusMethodNotAllowed)
		return
	}

	reg := streamMgr.Registry()
	if !reg.AnyAuthenticated() {
		writeJSON(w, http.StatusUnauthorized, map[string]any{
			"error": "unauthenticated: no Osaio login is signed in",
		})
		return
	}

	// Every signed-in login's cameras, each with the login it came from.
	refresh := r.URL.Query().Get("refresh") == "true"
	devs, owners, ts, err := reg.DeviceList(refresh)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"error": fmt.Sprintf("discovery failed: %v", err),
		})
		return
	}

	pm := streamMgr.ProfileManager()
	var prof *profile.Profile
	if pm != nil {
		prof = pm.GetProfile()
	}

	type cameraItem struct {
		ID           string `json:"id"`
		AccountEmail string `json:"account_email"`
		Name         string `json:"name"`
		Model        string `json:"model"`
		Online       bool   `json:"online"`
		IP           string `json:"ip"`
		MAC          string `json:"mac,omitempty"`
	}

	items := make([]cameraItem, 0, len(devs))
	for idx, d := range devs {
		camIP := ""
		if mc, ok := streamMgr.GetCamera(d.UUID); ok {
			mc.mu.RLock()
			camIP = mc.IP
			mc.mu.RUnlock()
		}
		if camIP != "" {
			// found on the live session
		} else if prof != nil && prof.Cameras != nil {
			if cp, found := prof.Cameras[d.UUID]; found && cp.IPAddress != "" {
				camIP = cp.IPAddress
			}
		}
		if camIP == "" && idx < len(streamMgr.cameraIPs) && strings.TrimSpace(streamMgr.cameraIPs[idx]) != "" {
			camIP = strings.TrimSpace(streamMgr.cameraIPs[idx])
		}

		items = append(items, cameraItem{
			ID:           d.UUID,
			AccountEmail: owners[d.UUID],
			Name:         d.Name,
			Model:        d.Type,
			Online:       d.Online != 0,
			IP:           camIP,
			MAC:          lookupMAC(camIP),
		})
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"cameras":      items,
		"freshness_ts": ts.Format(time.RFC3339),
	})
}

// handleCameraEnroll handles POST /api/v1/onboarding/cameras/enroll and POST /api/v1/inventory/enroll.
func handleCameraEnroll(w http.ResponseWriter, r *http.Request, sessionMgr *SessionManager, streamMgr *StreamManager) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		CameraIDs []string `json:"camera_ids"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": "invalid json payload",
		})
		return
	}

	if len(req.CameraIDs) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": "no camera IDs provided: camera_ids array must not be empty",
		})
		return
	}

	// The cameras the signed-in logins listed last, each with its login.
	var cachedDevs []bridge.Device
	owners := map[string]string{}
	for _, sm := range streamMgr.Registry().Sessions() {
		for _, d := range sm.GetCachedDevices() {
			if _, dup := owners[d.UUID]; !dup {
				owners[d.UUID] = profile.NormalizeEmail(sm.Email())
				cachedDevs = append(cachedDevs, d)
			}
		}
	}
	for _, id := range req.CameraIDs {
		found := false
		for _, d := range cachedDevs {
			if d.UUID == id {
				found = true
				break
			}
		}
		if !found {
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"error": fmt.Sprintf("camera ID '%s' not found in authenticated account inventory", id),
			})
			return
		}
	}

	enrolled, err := streamMgr.Enroll(req.CameraIDs, cachedDevs)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": err.Error(),
		})
		return
	}

	// Persist enrolled cameras into encrypted profile so daemon restart restores them
	pm := streamMgr.ProfileManager()
	if pm != nil {
		ctx := r.Context()
		devMap := make(map[string]bridge.Device)
		for _, d := range cachedDevs {
			devMap[d.UUID] = d
		}

		_, saveErr := pm.Update(ctx, func(p *profile.Profile) error {
			if p.Cameras == nil {
				p.Cameras = make(map[string]profile.CameraProfile)
			}
			desired := make(map[string]bool)
			for _, id := range enrolled {
				desired[id] = true
				existing := p.Cameras[id]
				dev := devMap[id]
				existing.UUID = id
				existing.AccountEmail = owners[id]
				existing.Name = dev.Name
				existing.Model = dev.Type
				existing.Online = dev.Online == 1
				if existing.EnrolledAt.IsZero() {
					existing.EnrolledAt = time.Now().UTC()
				}
				if mc, ok := streamMgr.GetCamera(id); ok {
					if ip := mc.LANIP(); ip != "" {
						existing.IPAddress = ip
					}
				}
				p.Cameras[id] = existing
			}
			for id := range p.Cameras {
				if !desired[id] {
					delete(p.Cameras, id)
				}
			}
			return nil
		})
		if saveErr != nil {
			// The streams are already running; say the profile disagrees
			// instead of reporting a clean enrollment.
			writeJSON(w, http.StatusInternalServerError, map[string]any{
				"error":   "cameras_not_saved",
				"message": "The cameras are streaming but could not be saved: " + saveErr.Error(),
			})
			return
		}
	}

	if pm != nil {
		syncStreamNames(r.Context(), pm, streamMgr)
	}
	kickIntegrations()
	writeJSON(w, http.StatusOK, map[string]any{
		"enrolled":       enrolled,
		"active_streams": streamMgr.ActiveStreamCount(),
	})
}

// ----------------------------------------------------------------------------
// Dual-Mode Firewall Orchestration & Stream/Control Activation
// ----------------------------------------------------------------------------

// handleDedicatedPTZ handles POST /api/v1/cameras/{id}/ptz.
func handleDedicatedPTZ(w http.ResponseWriter, r *http.Request, camID string, streamMgr *StreamManager) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}

	mc, ok := streamMgr.GetCamera(camID)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "camera not found"})
		return
	}

	cc := cameraControl(streamMgr, camID)
	if cc == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "control transport unavailable"})
		return
	}

	var req struct {
		Direction  any `json:"direction"`
		DurationMs int `json:"duration_ms"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid json payload"})
		return
	}

	dir := 0
	dirName := "stop"
	valid := false

	switch v := req.Direction.(type) {
	case float64:
		dir = int(v)
		if dir >= 0 && dir <= 4 {
			valid = true
		}
	case int:
		dir = v
		if dir >= 0 && dir <= 4 {
			valid = true
		}
	case string:
		switch strings.ToLower(v) {
		case "0", "stop":
			dir = 0
			valid = true
		case "1", "left":
			dir = 1
			valid = true
		case "2", "right":
			dir = 2
			valid = true
		case "3", "up", "top":
			dir = 3
			valid = true
		case "4", "down", "bottom":
			dir = 4
			valid = true
		default:
			if i, err := strconv.Atoi(v); err == nil && i >= 0 && i <= 4 {
				dir = i
				valid = true
			}
		}
	}

	if !valid {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": "invalid direction: use 0=stop, 1=left, 2=right, 3=up, 4=down",
		})
		return
	}

	switch dir {
	case 1:
		dirName = "left"
	case 2:
		dirName = "right"
	case 3:
		dirName = "up"
	case 4:
		dirName = "down"
	default:
		dirName = "stop"
	}

	dur := req.DurationMs
	if dur <= 0 {
		dur = 400
	}

	if err := cc.MovePTZ(r.Context(), camID, dir, dur); err != nil {
		controlFailed(w, "ptz move", err)
		return
	}

	mc.mu.RLock()
	camName := mc.Name
	camTransport := mc.Transport
	mc.mu.RUnlock()

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":             true,
		"camera":         camName,
		"uuid":           mc.UUID,
		"direction":      dir,
		"direction_name": dirName,
		"duration_ms":    dur,
		"transport":      camTransport,
	})
}

// handleCameraStream handles GET /api/v1/cameras/{id}/stream.
func handleCameraStream(w http.ResponseWriter, r *http.Request, camID string, streamMgr *StreamManager) {

	if r.Method != http.MethodGet {
		http.Error(w, "GET required", http.StatusMethodNotAllowed)
		return
	}

	mc, ok := streamMgr.GetCamera(camID)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "camera not found"})
		return
	}

	mc.mu.RLock()
	transport := mc.Transport
	if transport == "" {
		transport = "unknown"
	}
	freshness := ""
	if !mc.Freshness.IsZero() {
		freshness = mc.Freshness.UTC().Format(time.RFC3339)
	}
	streaming := mc.Streaming
	uuid, name := mc.UUID, mc.Name
	session := mc.Session
	liveViewer := mc.Viewer
	mc.mu.RUnlock()
	viewerCopy := liveViewer != nil && liveViewer.ViewerCopyReady()

	// "streaming" only means a camera session exists; the viewer needs to know
	// whether MediaMTX actually has the stream, and if not, why not.
	state, detail, ready, tracks := describeStreamState(uuid, streaming, session)
	if streamMgr.IsStopped(uuid) {
		state, detail, ready = "stopped", "Cameras are stopped. Press Start cameras under Settings to resume.", false
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"id":        uuid,
		"name":      name,
		"streaming": streaming,
		// rtsp_url / rtsp_url_lan / hls_url_lan: what other devices use (this
		// PC's LAN address). hls_url: the viewer's own copy, relayed by the
		// gateway so it is same-origin.
		"rtsp_url":          streamMgr.ConsumerRTSPURL(uuid, r),
		"rtsp_url_lan":      streamMgr.ConsumerRTSPURL(uuid, r),
		"rtsp_url_by_id":    streamMgr.ConsumerRTSPURLByID(uuid, r),
		"stream_name":       streamMgr.StreamName(uuid),
		"hls_url":           streamMgr.ViewerHLSURL(uuid),
		"hls_url_lan":       streamMgr.ConsumerHLSURL(uuid, r),
		"snapshot_url":      snapshotURLFor(streamMgr, r, uuid),
		"transport":         transport,
		"publication_ready": ready,
		"state":             state,
		"state_detail":      detail,
		"tracks":            tracks,
		"publisher":         streamMgr.Publisher(),
		"stopped":           streamMgr.IsStopped(uuid),
		"video_codec":       "h264",
		"audio_codec":       "aac",
		"freshness_ts":      freshness,
		// the video-only copy for the page's muted view (no alignment delay)
		"video_only_copy": viewerCopy,
	})
}

// describeStreamState classifies a camera's live-video state for the viewer:
// media_server_down, locked_out, connecting, waiting_for_video or ready.
func describeStreamState(uuid string, streaming bool, session *CameraSession) (state, detail string, ready bool, tracks []string) {
	tracks = []string{}
	if ok, why := mediaServerHealth(); !ok {
		return "media_server_down", "Video server problem: " + why, false, tracks
	}
	_, apiBase := currentMediaRuntime()
	if info, found, err := mediamtx.GetPath(apiBase, uuid); err == nil && found && info.Ready {
		if info.Tracks != nil {
			tracks = info.Tracks
		}
		return "ready", "Live", true, tracks
	}
	if session != nil {
		session.mu.Lock()
		locked := session.State == SessionStateLockout
		session.mu.Unlock()
		if locked {
			return "locked_out", "The camera failed to connect 3 times in a row. BombeCam will try again automatically, or press Retry now.", false, tracks
		}
	}
	if sameLoginViewerRecently(uuid) {
		return "busy_same_login", sameLoginDetail, false, tracks
	}
	if !streaming {
		return "connecting", "Connecting to the camera...", false, tracks
	}
	return "waiting_for_video", "Connected. Waiting for the first video frames...", false, tracks
}

// ----------------------------------------------------------------------------
// Lifecycle Safety & Owner Restoration
// ----------------------------------------------------------------------------

// handleGatewayStart handles POST /api/v1/gateway/start: resumes cameras
// stopped with /api/v1/gateway/stop.
func handleGatewayStart(w http.ResponseWriter, r *http.Request, streamMgr *StreamManager) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	started := streamMgr.StartStreaming()
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"status":  "started",
		"started": started,
	})
}

// handleGatewayStop handles POST /api/v1/gateway/stop.
// The route asks for authOperatorOnly, so the middleware has already checked it.
// Calls StreamManager.StopStreaming() to stop active MediaMTX RTSP streams and camera control polling.
// Does not change the router's "Block cloud video" rules.
func handleGatewayStop(w http.ResponseWriter, r *http.Request, streamMgr *StreamManager) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}

	streamMgr.StopStreaming()

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":               true,
		"status":           "stopped",
		"streaming":        false,
		"active_streams":   streamMgr.ActiveStreamCount(),
		"router_unchanged": true,
		"message":          "gateway streaming and control polling stopped; router rules unchanged",
	})
}
