package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/Fever-r/BombeCam/pkg/policy"
	"github.com/Fever-r/BombeCam/pkg/profile"
)

// Osaio logins. BombeCam has no account of its own: it keeps a pool of Osaio
// logins, stored encrypted on this computer, and uses each one only to find
// and reach the cameras it can see. Every login is equal: each can be signed
// in again, searched for more cameras, or removed with its cameras. The first
// sign-in page is only a quick start that adds the first login to the pool.

// accountView is one stored login as the Osaio logins page shows it.
type accountView struct {
	Email       string `json:"email"`
	Country     string `json:"country,omitempty"`
	Cameras     int    `json:"cameras"`
	HasPassword bool   `json:"has_password"`
	// Status: connected, connecting, needs_password, server_key, app_id,
	// unreachable or not_connected.
	Status string `json:"status"`
	Detail string `json:"detail,omitempty"`
}

type accountRequest struct {
	Email    string `json:"email"`
	Password string `json:"password,omitempty"`
	Country  string `json:"country,omitempty"`
}

// accountStatus turns a login's session state into the page's status.
func accountStatus(sm *SessionManager, found, hasPassword bool) (string, string) {
	if !found {
		if !hasPassword {
			return "needs_password", "No password is saved for this login."
		}
		return "not_connected", "Not signed in yet."
	}
	st := sm.GetStatus()
	switch st.Status {
	case SessionStatusAuthenticated:
		return "connected", ""
	case SessionStatusAuthenticating:
		return "connecting", st.Error
	case SessionStatusInvalidCredentials, SessionStatusCredentialsMissing:
		return "needs_password", st.Error
	case SessionStatusServerKeyMissing:
		return "server_key", st.Error
	case SessionStatusAppIDMissing:
		return "app_id", st.Error
	case SessionStatusNetworkTimeout, SessionStatusUnavailable:
		return "unreachable", st.Error
	}
	return "not_connected", st.Error
}

// handleAccountsList handles GET /api/v1/accounts.
func handleAccountsList(w http.ResponseWriter, r *http.Request, streamMgr *StreamManager) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET required", http.StatusMethodNotAllowed)
		return
	}
	out := []accountView{}
	attention := 0
	pm := streamMgr.ProfileManager()
	var prof *profile.Profile
	if pm != nil {
		prof = pm.GetProfile()
	}
	if prof != nil {
		reg := streamMgr.Registry()
		for _, a := range prof.Accounts {
			var sm *SessionManager
			found := false
			if reg != nil {
				sm, found = reg.Get(a.AccountEmail)
			}
			status, detail := accountStatus(sm, found, !a.Password.IsEmpty())
			if status != "connected" && status != "connecting" {
				attention++
			}
			out = append(out, accountView{
				Email:       a.AccountEmail,
				Country:     a.Country,
				Cameras:     len(prof.CamerasForAccount(a.AccountEmail)),
				HasPassword: !a.Password.IsEmpty(),
				Status:      status,
				Detail:      detail,
			})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"accounts": out, "needs_attention": attention})
}

// signInFailure is a sign-in Osaio refused or BombeCam could not save.
type signInFailure struct {
	HTTP    int
	Code    string
	Message string
}

func (f *signInFailure) write(w http.ResponseWriter) {
	writeJSON(w, f.HTTP, map[string]any{"error": f.Code, "message": f.Message})
}

// signInFailureFor maps a failed sign-in's session state to the reply.
func signInFailureFor(st SessionStatusInfo, err error) *signInFailure {
	f := &signInFailure{HTTP: http.StatusUnauthorized, Code: "invalid_credentials", Message: st.Error}
	switch st.Status {
	case SessionStatusServerKeyMissing:
		f.HTTP, f.Code = http.StatusPreconditionRequired, "server_key_missing"
	case SessionStatusAppIDMissing:
		f.HTTP, f.Code = http.StatusPreconditionRequired, "app_id_missing"
	case SessionStatusNetworkTimeout:
		f.HTTP, f.Code = http.StatusServiceUnavailable, "network_timeout"
	case SessionStatusUnavailable:
		f.HTTP, f.Code = http.StatusBadGateway, "upstream_unavailable"
	case SessionStatusCredentialsMissing:
		f.HTTP, f.Code = http.StatusBadRequest, "country_required"
	}
	if f.Message == "" && err != nil {
		f.Message = err.Error()
	}
	return f
}

// signInToPool checks a login's password with Osaio on a separate connection
// and, if it works, makes that the login's session and stores the login
// (encrypted) in the profile. Every login is handled the same way, whether it
// is the first one or the tenth, and no other login is touched. A refused
// password leaves the login's working session and stored password as they
// were.
func signInToPool(ctx context.Context, reg *SessionRegistry, pm profile.ProfileManager, email, password, country string) (*SessionManager, *signInFailure) {
	email = profile.NormalizeEmail(email)
	trial := reg.NewTrialSession(country)
	if err := trial.Login(email, password); err != nil {
		return nil, signInFailureFor(trial.GetStatus(), err)
	}
	// A refused start-up login waiting to be retried after a server key or
	// app ID change is moot now, and would hold up re-signing failed logins.
	takePendingStartup()
	session := reg.Ensure(email)
	session.Adopt(email, country, trial.Cloud())
	forgetRenewAttempts()
	if pm != nil {
		c := session.Cloud()
		creds := profile.CloudCredentials{
			AccountEmail:   email,
			Password:       profile.SecretString(password),
			Country:        country,
			PhoneCode:      session.PhoneCode(),
			VendorUID:      c.UID,
			AuthToken:      profile.SecretString(c.APIToken),
			Region:         c.Web,
			TokenExpiresAt: time.Now().UTC().Add(24 * time.Hour),
		}
		if _, err := pm.Update(ctx, func(p *profile.Profile) error {
			p.UpsertAccount(creds)
			return nil
		}); err != nil {
			return session, &signInFailure{HTTP: http.StatusInternalServerError, Code: "save_failed",
				Message: "Signed in, but the login could not be saved: " + err.Error()}
		}
	}
	return session, nil
}

// handleAccountSignIn handles POST /api/v1/accounts/signin {email, password,
// country}: adds a login to the pool, or saves a new password for one already
// in it.
func handleAccountSignIn(w http.ResponseWriter, r *http.Request, sessionMgr *SessionManager, streamMgr *StreamManager) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	var req accountRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_request", "message": "Send the login's email and password."})
		return
	}
	email := profile.NormalizeEmail(req.Email)
	if email == "" || req.Password == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_request", "message": "Enter the login's email and password."})
		return
	}
	reg := streamMgr.Registry()
	pm := streamMgr.ProfileManager()
	var saved *profile.Profile
	if pm != nil {
		saved = pm.GetProfile()
	}
	_, known := reg.Get(email)
	country, err := resolveAccountCountry(req.Country, email, saved, countrySession(reg, email))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "country_required", "message": err.Error()})
		return
	}
	if _, fail := signInToPool(r.Context(), reg, pm, email, req.Password, country); fail != nil {
		fail.write(w)
		return
	}
	// Cameras of this login that gave up waiting for it try again now.
	if saved != nil {
		for _, id := range saved.CamerasForAccount(email) {
			_, _ = streamMgr.Unlock(id)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "email": email, "added": !known})
}

// countrySession is the session whose country a sign-in may reuse: the
// login's own, or (for a login BombeCam has not seen) the start-up default.
func countrySession(reg *SessionRegistry, email string) *SessionManager {
	if sm, ok := reg.Get(email); ok {
		return sm
	}
	return reg.Template()
}

// handleAccountCameras handles POST /api/v1/accounts/cameras {email}: the
// cameras a stored login can see, so more of them can be added without
// typing its password again.
func handleAccountCameras(w http.ResponseWriter, r *http.Request, streamMgr *StreamManager) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	var req accountRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_request"})
		return
	}
	email := profile.NormalizeEmail(req.Email)
	reg := streamMgr.Registry()
	var sm *SessionManager
	ok := false
	if reg != nil && email != "" {
		sm, ok = reg.Get(email)
	}
	if !ok || !sm.IsAuthenticated() {
		writeJSON(w, http.StatusConflict, map[string]any{"error": "not_signed_in", "message": "This login isn't signed in. Enter its password first."})
		return
	}
	devs, _, err := sm.DeviceList(true)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "discovery_failed", "message": fmt.Sprintf("Osaio didn't return this login's cameras: %v", err)})
		return
	}
	enrolled := map[string]bool{}
	if pm := streamMgr.ProfileManager(); pm != nil {
		if prof := pm.GetProfile(); prof != nil {
			for id := range prof.Cameras {
				enrolled[id] = true
			}
		}
	}
	items := make([]cameraListItem, 0, len(devs))
	for _, d := range devs {
		camIP := ""
		if mc, ok := streamMgr.GetCamera(d.UUID); ok {
			camIP = mc.LANIP()
		}
		items = append(items, cameraListItem{ID: d.UUID, AccountEmail: email, Name: d.Name, Model: d.Type,
			Online: d.Online != 0, IP: camIP, Enrolled: enrolled[d.UUID]})
	}
	writeJSON(w, http.StatusOK, map[string]any{"account_email": email, "cameras": items})
}

// handleAccountRemove handles POST /api/v1/accounts/remove {email}: forgets a
// login and removes the cameras added through it (their streams stop, and
// the router stops limiting them if it did). Other logins and their cameras
// are not touched.
func handleAccountRemove(w http.ResponseWriter, r *http.Request, sessionMgr *SessionManager, streamMgr *StreamManager) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	var req accountRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_request"})
		return
	}
	email := profile.NormalizeEmail(req.Email)
	pm := streamMgr.ProfileManager()
	if pm == nil || pm.GetProfile() == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "not_found", "message": "That login isn't stored on this computer."})
		return
	}
	prof := pm.GetProfile()
	if _, ok := prof.FindAccount(email); !ok || email == "" {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "not_found", "message": "That login isn't stored on this computer."})
		return
	}
	camIDs := prof.CamerasForAccount(email)
	onRouter := false
	applied := map[string]bool{}
	for _, m := range prof.Privacy.AppliedCameras {
		applied[m] = true
	}
	for _, id := range camIDs {
		if mac := policy.CanonicalMAC(prof.Cameras[id].MACAddress); mac != "" && applied[mac] {
			onRouter = true
		}
		streamMgr.Unenroll(id)
	}
	updated, err := pm.Update(r.Context(), func(p *profile.Profile) error {
		for _, id := range camIDs {
			delete(p.Cameras, id)
			delete(p.Privacy.Blocked, id)
		}
		p.RemoveAccount(email)
		return nil
	})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "save_failed", "message": err.Error()})
		return
	}

	reg := streamMgr.Registry()
	reg.Remove(email)
	if len(updated.Accounts) == 0 {
		sessionMgr.Reset() // nothing left to sign in: back to first run
	}

	note := ""
	if onRouter {
		ctx, cancel := context.WithTimeout(context.Background(), changeTimeout)
		o := syncRouter(ctx, streamMgr, pm, false)
		cancel()
		if o.OK {
			note = "Your router no longer limits the removed cameras."
		} else {
			note = "Your router still limits the removed cameras. Connect the router in the Firewall tab to update it."
		}
	}
	kickIntegrations()
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":              true,
		"removed":         email,
		"removed_cameras": camIDs,
		"logins_left":     len(updated.Accounts),
		"firewall_note":   note,
	})
}
