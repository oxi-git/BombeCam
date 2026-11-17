package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"rsc.io/qr"

	"github.com/Fever-r/BombeCam/pkg/auth"
	"github.com/Fever-r/BombeCam/pkg/profile"
)

// BombeCam's administrator. It is created on this PC at first start, before
// any Osaio login, and is the only way into BombeCam from other devices.
// The administrator chooses whether this PC asks for the sign-in too.
// Osaio logins never sign anyone in to BombeCam.

// signInRequired reports whether a request needs a signed-in administrator:
// always from other devices, and on this PC when the administrator chose so.
// Until an administrator exists, this PC needs no sign-in (it is where the
// administrator is created) and other devices cannot get in.
func signInRequired(r *http.Request, streamMgr *StreamManager) bool {
	if !isLoopbackRequest(r) {
		return true
	}
	prof := activeProfile(streamMgr)
	if prof == nil {
		return false
	}
	_, hasAdmin := prof.Admin()
	return hasAdmin && prof.Security.RequireLocalSignIn
}

func activeProfile(streamMgr *StreamManager) *profile.Profile {
	if streamMgr == nil {
		return nil
	}
	if pm := streamMgr.ProfileManager(); pm != nil {
		return pm.GetProfile()
	}
	return nil
}

// requestSignedIn reports whether the request carries a signed-in session or token.
func requestSignedIn(r *http.Request) bool {
	if om := GatewayOperatorManager(); om != nil {
		ok, _, _ := om.AuthenticateRequest(r)
		return ok
	}
	return false
}

type adminRequest struct {
	Name               string  `json:"name"`
	Password           string  `json:"password"`
	NewPassword        string  `json:"new_password"`
	Code               string  `json:"code"`
	Hint               *string `json:"hint"`
	RequireLocalSignIn *bool   `json:"require_local_sign_in"`
}

// maxHintLength keeps the hint a short reminder.
const maxHintLength = 120

// checkHint cleans up a password hint and refuses one that gives the
// password away.
func checkHint(hint, password string) (string, string) {
	hint = strings.TrimSpace(hint)
	if len([]rune(hint)) > maxHintLength {
		return "", fmt.Sprintf("Keep the hint under %d characters.", maxHintLength)
	}
	if hint != "" && password != "" && strings.Contains(strings.ToLower(hint), strings.ToLower(password)) {
		return "", "The hint must not contain the password."
	}
	return hint, ""
}

func decodeAdminRequest(w http.ResponseWriter, r *http.Request) (adminRequest, bool) {
	var req adminRequest
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return req, false
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_request"})
		return req, false
	}
	req.Name = strings.TrimSpace(req.Name)
	return req, true
}

// startAdminSession signs the browser in: a new session (never the one used
// before signing in) with its own cookie and CSRF token.
func startAdminSession(w http.ResponseWriter, r *http.Request) (string, bool) {
	om := GatewayOperatorManager()
	if om == nil {
		return "", true
	}
	if c, err := r.Cookie(auth.SessionCookieName); err == nil && c.Value != "" {
		om.RevokeSession(c.Value)
	}
	sess, err := om.CreateSession()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "session_failed"})
		return "", false
	}
	om.IssueSessionCookie(w, sess)
	return sess.CSRFToken, true
}

func adminProfileManager(w http.ResponseWriter, streamMgr *StreamManager) (profile.ProfileManager, bool) {
	pm := streamMgr.ProfileManager()
	if pm == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "profile_unavailable", "message": "BombeCam's saved data can't be opened."})
		return nil, false
	}
	return pm, true
}

// handleAdminStatus handles GET /api/v1/admin/status: what the page needs
// to decide between creating the administrator, signing in, or carrying on.
func handleAdminStatus(w http.ResponseWriter, r *http.Request, streamMgr *StreamManager) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET required", http.StatusMethodNotAllowed)
		return
	}
	prof := activeProfile(streamMgr)
	admin, exists := prof.Admin()
	in := requestSignedIn(r)
	resp := map[string]any{
		"admin_exists":       exists,
		"this_pc":            isLoopbackRequest(r),
		"signed_in":          in,
		"sign_in_required":   signInRequired(r, streamMgr),
		"can_create":         !exists && isLoopbackRequest(r),
		"min_password_chars": auth.MinPasswordLength,
	}
	// A saved profile that can't be opened (lost key, damaged file): this PC
	// can only start again with Delete all.
	if prof == nil && isLoopbackRequest(r) {
		if pm := streamMgr.ProfileManager(); pm != nil {
			if has, _ := pm.HasProfile(r.Context()); has {
				if _, err := pm.Load(r.Context()); err != nil {
					resp["profile_unreadable"] = true
				}
			}
		}
	}
	// The hint is for whoever sits at the PC running BombeCam, not for
	// other devices on the network.
	if exists && isLoopbackRequest(r) && admin.PasswordHint != "" {
		resp["password_hint"] = admin.PasswordHint
	}
	if exists && (in || !signInRequired(r, streamMgr)) {
		resp["has_password_hint"] = admin.PasswordHint != ""
		resp["name"] = admin.Name
		resp["require_local_sign_in"] = prof.Security.RequireLocalSignIn
		resp["two_step"] = admin.TwoStep()
		resp["recovery_codes_left"] = len(admin.RecoveryCodes)
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleAdminSetup handles POST /api/v1/admin/setup {name, password,
// require_local_sign_in}: creates the administrator. Only on this PC, and
// only while there is none.
func handleAdminSetup(w http.ResponseWriter, r *http.Request, streamMgr *StreamManager) {
	req, ok := decodeAdminRequest(w, r)
	if !ok {
		return
	}
	if !isLoopbackRequest(r) {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": "this_pc_only", "message": "Create the administrator on the PC running BombeCam."})
		return
	}
	pm, ok := adminProfileManager(w, streamMgr)
	if !ok {
		return
	}
	if req.Name == "" {
		req.Name = "admin"
	}
	hash, err := auth.HashPassword(req.Password)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "weak_password", "message": "Password: " + err.Error() + "."})
		return
	}
	hint := ""
	if req.Hint != nil {
		var msg string
		if hint, msg = checkHint(*req.Hint, req.Password); msg != "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "bad_hint", "message": msg})
			return
		}
	}
	id := make([]byte, 8)
	_, _ = rand.Read(id)
	exists := false
	_, err = pm.Update(r.Context(), func(p *profile.Profile) error {
		if _, has := p.Admin(); has {
			exists = true
			return nil
		}
		p.Users = append(p.Users, profile.UserProfile{ID: hex.EncodeToString(id), Name: req.Name, Role: profile.RoleAdmin,
			PasswordHash: profile.SecretString(hash), PasswordHint: hint, CreatedAt: time.Now().UTC()})
		p.Security.RequireLocalSignIn = req.RequireLocalSignIn != nil && *req.RequireLocalSignIn
		return nil
	})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "save_failed", "message": err.Error()})
		return
	}
	if exists {
		writeJSON(w, http.StatusConflict, map[string]any{"error": "admin_exists", "message": "BombeCam already has an administrator. Sign in instead."})
		return
	}
	fmt.Printf("[admin] administrator %q created (sign-in on this PC: %v)\n", req.Name, req.RequireLocalSignIn != nil && *req.RequireLocalSignIn)
	csrf, ok := startAdminSession(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "csrf_token": csrf})
}

// checkAdminCode verifies a two-step code: an authenticator code (each works
// once) or a one-time recovery code. On success the change is saved.
func checkAdminCode(r *http.Request, pm profile.ProfileManager, code string) (usedRecovery bool, ok bool) {
	admin, _ := pm.GetProfile().Admin()
	if step, good := auth.VerifyTOTP(admin.TOTPSecret.Expose(), code, time.Now(), admin.TOTPLastStep); good {
		_, err := pm.Update(r.Context(), func(p *profile.Profile) error {
			p.UpdateAdmin(func(u *profile.UserProfile) {
				if step > u.TOTPLastStep {
					u.TOTPLastStep = step
				}
			})
			return nil
		})
		return false, err == nil
	}
	if left, good := auth.UseRecoveryCode(admin.RecoveryCodes, code); good {
		_, err := pm.Update(r.Context(), func(p *profile.Profile) error {
			p.UpdateAdmin(func(u *profile.UserProfile) { u.RecoveryCodes = left })
			return nil
		})
		return true, err == nil
	}
	return false, false
}

// verifyAdmin checks a name (optional) and password, rate-limited per
// address with the other sign-ins. It writes the refusal itself.
func verifyAdmin(w http.ResponseWriter, r *http.Request, pm profile.ProfileManager, name, password string) (profile.UserProfile, bool) {
	addr := requestAddress(r)
	if operatorSignIns.blocked(addr) {
		w.Header().Set("Retry-After", strconv.Itoa(int(signInWindow/time.Second)))
		writeJSON(w, http.StatusTooManyRequests, map[string]any{"error": "too_many_attempts", "message": "Too many wrong passwords from this address. Try again in 10 minutes."})
		return profile.UserProfile{}, false
	}
	admin, exists := pm.GetProfile().Admin()
	nameOK := name == "" || strings.EqualFold(name, admin.Name)
	if !exists || !auth.VerifyPassword(admin.PasswordHash.Expose(), password) || !nameOK {
		operatorSignIns.failed(addr)
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "wrong_password", "message": "Wrong name or password."})
		return profile.UserProfile{}, false
	}
	return admin, true
}

// handleAdminSignIn handles POST /api/v1/admin/signin {name, password, code}.
func handleAdminSignIn(w http.ResponseWriter, r *http.Request, streamMgr *StreamManager) {
	req, ok := decodeAdminRequest(w, r)
	if !ok {
		return
	}
	pm, ok := adminProfileManager(w, streamMgr)
	if !ok {
		return
	}
	admin, ok := verifyAdmin(w, r, pm, req.Name, req.Password)
	if !ok {
		return
	}
	addr := requestAddress(r)
	usedRecovery := false
	if admin.TwoStep() {
		if strings.TrimSpace(req.Code) == "" {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "code_required", "message": "Enter the code from your authenticator app."})
			return
		}
		var good bool
		if usedRecovery, good = checkAdminCode(r, pm, req.Code); !good {
			operatorSignIns.failed(addr)
			writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "wrong_code", "message": "That code didn't work. Codes change every 30 seconds and each works once."})
			return
		}
	}
	operatorSignIns.succeeded(addr)
	csrf, ok := startAdminSession(w, r)
	if !ok {
		return
	}
	left := 0
	if a, _ := pm.GetProfile().Admin(); a.TwoStep() {
		left = len(a.RecoveryCodes)
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "csrf_token": csrf, "used_recovery_code": usedRecovery, "recovery_codes_left": left})
}

// handleAdminSignOut handles POST /api/v1/admin/signout.
func handleAdminSignOut(w http.ResponseWriter, r *http.Request, streamMgr *StreamManager) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	if om := GatewayOperatorManager(); om != nil {
		if c, err := r.Cookie(auth.SessionCookieName); err == nil && c.Value != "" {
			om.RevokeSession(c.Value)
		}
		om.ClearSessionCookie(w)
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleAdminPassword handles POST /api/v1/admin/password {password,
// new_password}. Every other signed-in browser is signed out.
func handleAdminPassword(w http.ResponseWriter, r *http.Request, streamMgr *StreamManager) {
	req, ok := decodeAdminRequest(w, r)
	if !ok {
		return
	}
	pm, ok := adminProfileManager(w, streamMgr)
	if !ok {
		return
	}
	if _, ok := verifyAdmin(w, r, pm, "", req.Password); !ok {
		return
	}
	hash, err := auth.HashPassword(req.NewPassword)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "weak_password", "message": "New password: " + err.Error() + "."})
		return
	}
	if admin, _ := pm.GetProfile().Admin(); admin.PasswordHint != "" {
		if _, msg := checkHint(admin.PasswordHint, req.NewPassword); msg != "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "bad_hint", "message": "Your password hint contains the new password. Change the hint first, or pick another password."})
			return
		}
	}
	if _, err := pm.Update(r.Context(), func(p *profile.Profile) error {
		p.UpdateAdmin(func(u *profile.UserProfile) { u.PasswordHash = profile.SecretString(hash) })
		return nil
	}); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "save_failed", "message": err.Error()})
		return
	}
	operatorSignIns.succeeded(requestAddress(r))
	if om := GatewayOperatorManager(); om != nil {
		om.RevokeOperatorSessions()
	}
	csrf, ok := startAdminSession(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "csrf_token": csrf})
}

// handleAdminLocalSignIn handles POST /api/v1/admin/local-sign-in
// {password, require_local_sign_in}.
func handleAdminLocalSignIn(w http.ResponseWriter, r *http.Request, streamMgr *StreamManager) {
	req, ok := decodeAdminRequest(w, r)
	if !ok {
		return
	}
	pm, ok := adminProfileManager(w, streamMgr)
	if !ok {
		return
	}
	if req.RequireLocalSignIn == nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_request"})
		return
	}
	if _, ok := verifyAdmin(w, r, pm, "", req.Password); !ok {
		return
	}
	operatorSignIns.succeeded(requestAddress(r))
	if _, err := pm.Update(r.Context(), func(p *profile.Profile) error {
		p.Security.RequireLocalSignIn = *req.RequireLocalSignIn
		return nil
	}); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "save_failed", "message": err.Error()})
		return
	}
	// Turning it on from this PC: this browser stays signed in.
	csrf, ok := startAdminSession(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "require_local_sign_in": *req.RequireLocalSignIn, "csrf_token": csrf})
}

// handleAdminHint handles POST /api/v1/admin/hint {password, hint}: sets or
// clears (empty) the password hint.
func handleAdminHint(w http.ResponseWriter, r *http.Request, streamMgr *StreamManager) {
	req, ok := decodeAdminRequest(w, r)
	if !ok {
		return
	}
	pm, ok := adminProfileManager(w, streamMgr)
	if !ok {
		return
	}
	if req.Hint == nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_request"})
		return
	}
	if _, ok := verifyAdmin(w, r, pm, "", req.Password); !ok {
		return
	}
	operatorSignIns.succeeded(requestAddress(r))
	hint, msg := checkHint(*req.Hint, req.Password)
	if msg != "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "bad_hint", "message": msg})
		return
	}
	if _, err := pm.Update(r.Context(), func(p *profile.Profile) error {
		p.UpdateAdmin(func(u *profile.UserProfile) { u.PasswordHint = hint })
		return nil
	}); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "save_failed", "message": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "has_password_hint": hint != ""})
}

// A two-step secret waits here between "start" and "confirm"; it is saved
// only once a code from the app has proved the app has it.
var pendingTOTP struct {
	sync.Mutex
	secret  string
	expires time.Time
}

// handleAdminTwoStepStart handles POST /api/v1/admin/two-step/start
// {password}: a new secret, as text and as a QR code for the app.
func handleAdminTwoStepStart(w http.ResponseWriter, r *http.Request, streamMgr *StreamManager) {
	req, ok := decodeAdminRequest(w, r)
	if !ok {
		return
	}
	pm, ok := adminProfileManager(w, streamMgr)
	if !ok {
		return
	}
	admin, ok := verifyAdmin(w, r, pm, "", req.Password)
	if !ok {
		return
	}
	operatorSignIns.succeeded(requestAddress(r))
	secret, err := auth.NewTOTPSecret()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "internal_error"})
		return
	}
	uri := auth.TOTPURI(secret, admin.Name, "BombeCam")
	img := ""
	if code, err := qr.Encode(uri, qr.M); err == nil {
		code.Scale = 6
		img = "data:image/png;base64," + base64.StdEncoding.EncodeToString(code.PNG())
	}
	pendingTOTP.Lock()
	pendingTOTP.secret, pendingTOTP.expires = secret, time.Now().Add(10*time.Minute)
	pendingTOTP.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "secret": secret, "uri": uri, "qr": img})
}

// handleAdminTwoStepConfirm handles POST /api/v1/admin/two-step/confirm
// {code}: turns two-step on and returns the recovery codes (shown once).
func handleAdminTwoStepConfirm(w http.ResponseWriter, r *http.Request, streamMgr *StreamManager) {
	req, ok := decodeAdminRequest(w, r)
	if !ok {
		return
	}
	pm, ok := adminProfileManager(w, streamMgr)
	if !ok {
		return
	}
	pendingTOTP.Lock()
	secret := pendingTOTP.secret
	if time.Now().After(pendingTOTP.expires) {
		secret = ""
	}
	pendingTOTP.Unlock()
	if secret == "" {
		writeJSON(w, http.StatusConflict, map[string]any{"error": "not_started", "message": "Start again: the setup expired."})
		return
	}
	step, good := auth.VerifyTOTP(secret, req.Code, time.Now(), 0)
	if !good {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "wrong_code", "message": "That code didn't match. Check the app shows BombeCam and try the current code."})
		return
	}
	codes, hashes, err := auth.NewRecoveryCodes()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "internal_error"})
		return
	}
	if _, err := pm.Update(r.Context(), func(p *profile.Profile) error {
		p.UpdateAdmin(func(u *profile.UserProfile) {
			u.TOTPSecret = profile.SecretString(secret)
			u.TOTPLastStep = step
			u.RecoveryCodes = hashes
		})
		return nil
	}); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "save_failed", "message": err.Error()})
		return
	}
	pendingTOTP.Lock()
	pendingTOTP.secret = ""
	pendingTOTP.Unlock()
	fmt.Println("[admin] two-step sign-in turned on")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "recovery_codes": codes})
}

// handleAdminTwoStepDisable handles POST /api/v1/admin/two-step/disable
// {password, code}.
func handleAdminTwoStepDisable(w http.ResponseWriter, r *http.Request, streamMgr *StreamManager) {
	req, ok := decodeAdminRequest(w, r)
	if !ok {
		return
	}
	pm, ok := adminProfileManager(w, streamMgr)
	if !ok {
		return
	}
	admin, ok := verifyAdmin(w, r, pm, "", req.Password)
	if !ok {
		return
	}
	if admin.TwoStep() {
		if _, good := checkAdminCode(r, pm, req.Code); !good {
			operatorSignIns.failed(requestAddress(r))
			writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "wrong_code", "message": "That code didn't work."})
			return
		}
	}
	operatorSignIns.succeeded(requestAddress(r))
	if _, err := pm.Update(r.Context(), func(p *profile.Profile) error {
		p.UpdateAdmin(func(u *profile.UserProfile) {
			u.TOTPSecret, u.TOTPLastStep, u.RecoveryCodes = "", 0, nil
		})
		return nil
	}); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "save_failed", "message": err.Error()})
		return
	}
	fmt.Println("[admin] two-step sign-in turned off")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleAdminRecoveryCodes handles POST /api/v1/admin/recovery-codes
// {password}: a new set, replacing the old one.
func handleAdminRecoveryCodes(w http.ResponseWriter, r *http.Request, streamMgr *StreamManager) {
	req, ok := decodeAdminRequest(w, r)
	if !ok {
		return
	}
	pm, ok := adminProfileManager(w, streamMgr)
	if !ok {
		return
	}
	admin, ok := verifyAdmin(w, r, pm, "", req.Password)
	if !ok {
		return
	}
	operatorSignIns.succeeded(requestAddress(r))
	if !admin.TwoStep() {
		writeJSON(w, http.StatusConflict, map[string]any{"error": "two_step_off", "message": "Turn on two-step sign-in first."})
		return
	}
	codes, hashes, err := auth.NewRecoveryCodes()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "internal_error"})
		return
	}
	if _, err := pm.Update(r.Context(), func(p *profile.Profile) error {
		p.UpdateAdmin(func(u *profile.UserProfile) { u.RecoveryCodes = hashes })
		return nil
	}); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "save_failed", "message": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "recovery_codes": codes})
}

// ensureAdminFromEnv creates the administrator from BOMBECAM_ADMIN_NAME and
// BOMBECAM_ADMIN_PASSWORD when there is none yet, for a machine whose page
// is only opened from other devices. An existing administrator is never
// changed this way.
func ensureAdminFromEnv(pm profile.ProfileManager, getenv func(string) string) {
	password := getenv("BOMBECAM_ADMIN_PASSWORD")
	if pm == nil || password == "" {
		return
	}
	if prof, err := pm.Load(context.Background()); err == nil && prof != nil {
		if _, has := prof.Admin(); has {
			return
		}
	}
	name := strings.TrimSpace(getenv("BOMBECAM_ADMIN_NAME"))
	if name == "" {
		name = "admin"
	}
	hash, err := auth.HashPassword(password)
	if err != nil {
		fmt.Printf("[admin] BOMBECAM_ADMIN_PASSWORD not used: %v\n", err)
		return
	}
	id := make([]byte, 8)
	_, _ = rand.Read(id)
	if _, err := pm.Update(context.Background(), func(p *profile.Profile) error {
		if _, has := p.Admin(); !has {
			p.Users = append(p.Users, profile.UserProfile{ID: hex.EncodeToString(id), Name: name, Role: profile.RoleAdmin,
				PasswordHash: profile.SecretString(hash), CreatedAt: time.Now().UTC()})
		}
		return nil
	}); err != nil {
		fmt.Printf("[admin] could not create the administrator from the environment: %v\n", err)
		return
	}
	fmt.Printf("[admin] administrator %q created from BOMBECAM_ADMIN_PASSWORD\n", name)
}

// startOverShutdown shuts BombeCam down after starting over (replaced in tests).
var startOverShutdown = func() {
	time.Sleep(shutdownDelay)
	requestShutdown()
}

// handleAdminStartOver handles POST /api/v1/admin/start-over: after a
// forgotten password, deletes all saved data, the administrator included
// (taking BombeCam's rules and key off the router first), then shuts
// BombeCam down. The next start begins from scratch. Only on the PC running
// BombeCam, which needs no password for it: whoever sits there could delete
// the file anyway. Other devices have no way to do this.
func handleAdminStartOver(w http.ResponseWriter, r *http.Request, sessionMgr *SessionManager, streamMgr *StreamManager) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	if !isLoopbackRequest(r) {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": "this_pc_only", "message": "Only the PC running BombeCam can start it over."})
		return
	}
	resp := resetStoredInformation(r.Context(), sessionMgr, streamMgr, streamMgr.ProfileManager(), false)
	if om := GatewayOperatorManager(); om != nil {
		om.RevokeOperatorSessions()
		om.ClearSessionCookie(w)
	}
	fmt.Println("[admin] started over after a forgotten password: all saved data deleted; shutting down")
	resp["status"] = "shutting_down"
	writeJSON(w, http.StatusOK, resp)
	go startOverShutdown()
}

// registerAdminRoutes adds the administrator endpoints.
func registerAdminRoutes(mux *http.ServeMux, streamMgr *StreamManager) {
	mux.HandleFunc("/api/v1/admin/status", func(w http.ResponseWriter, r *http.Request) {
		handleAdminStatus(w, r, streamMgr)
	})
	mux.HandleFunc("/api/v1/admin/setup", func(w http.ResponseWriter, r *http.Request) {
		handleAdminSetup(w, r, streamMgr)
	})
	mux.HandleFunc("/api/v1/admin/signin", func(w http.ResponseWriter, r *http.Request) {
		handleAdminSignIn(w, r, streamMgr)
	})
	mux.HandleFunc("/api/v1/admin/signout", func(w http.ResponseWriter, r *http.Request) {
		handleAdminSignOut(w, r, streamMgr)
	})
	mux.HandleFunc("/api/v1/admin/password", func(w http.ResponseWriter, r *http.Request) {
		handleAdminPassword(w, r, streamMgr)
	})
	mux.HandleFunc("/api/v1/admin/local-sign-in", func(w http.ResponseWriter, r *http.Request) {
		handleAdminLocalSignIn(w, r, streamMgr)
	})
	mux.HandleFunc("/api/v1/admin/hint", func(w http.ResponseWriter, r *http.Request) {
		handleAdminHint(w, r, streamMgr)
	})
	mux.HandleFunc("/api/v1/admin/two-step/start", func(w http.ResponseWriter, r *http.Request) {
		handleAdminTwoStepStart(w, r, streamMgr)
	})
	mux.HandleFunc("/api/v1/admin/two-step/confirm", func(w http.ResponseWriter, r *http.Request) {
		handleAdminTwoStepConfirm(w, r, streamMgr)
	})
	mux.HandleFunc("/api/v1/admin/two-step/disable", func(w http.ResponseWriter, r *http.Request) {
		handleAdminTwoStepDisable(w, r, streamMgr)
	})
	mux.HandleFunc("/api/v1/admin/recovery-codes", func(w http.ResponseWriter, r *http.Request) {
		handleAdminRecoveryCodes(w, r, streamMgr)
	})
}
