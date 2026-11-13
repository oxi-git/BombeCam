package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/Fever-r/BombeCam/internal/ui"
	"github.com/Fever-r/BombeCam/pkg/auth"
	"github.com/Fever-r/BombeCam/pkg/bridge"
	shadowshim "github.com/Fever-r/BombeCam/pkg/bridge/shadow_shim"
	"github.com/Fever-r/BombeCam/pkg/profile"
)

// SetupAPIMux registers both the modern REST API (/api/v1/...) and legacy endpoints.
func SetupAPIMux(sessionMgr *SessionManager, streamMgr *StreamManager, optPM ...profile.ProfileManager) *http.ServeMux {
	// Every handler reaches the Osaio logins through the pool; sessionMgr is
	// only its template.
	if reg := GatewaySessions(); sessionMgr != nil && (reg == nil || reg.Template() != sessionMgr) {
		SetGatewaySessions(NewSessionRegistry(sessionMgr, sessionMgr.Country(), sessionMgr.PhoneCode(), nil))
	}
	mux := http.NewServeMux()

	// -------------------------------------------------------------
	// 0. Operator Auth & CSRF API
	// -------------------------------------------------------------
	csrfHandler := func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "GET required", http.StatusMethodNotAllowed)
			return
		}
		om := GatewayOperatorManager()
		if om == nil {
			om = auth.NewOperatorManager(auth.OperatorConfig{})
			SetGatewayOperatorManager(om)
		}
		if !om.ValidateHost(r.Host) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"error":   "forbidden_host",
				"message": "Host header rejected by local security boundary",
			})
			return
		}

		cookie, err := r.Cookie(auth.SessionCookieName)
		var sess *auth.Session
		if err == nil && cookie != nil && cookie.Value != "" {
			sess, _ = om.VerifySession(cookie.Value)
		}
		if sess == nil {
			sess, err = om.CreateVisitorSession()
			if err != nil {
				http.Error(w, "failed to create session", http.StatusInternalServerError)
				return
			}
			om.IssueSessionCookie(w, sess)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok":         true,
			"csrf_token": sess.CSRFToken,
		})
	}
	mux.HandleFunc("/api/v1/auth/csrf", csrfHandler)
	mux.HandleFunc("/api/v1/operator/csrf", csrfHandler)

	tokenHandler := func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST required", http.StatusMethodNotAllowed)
			return
		}
		om := GatewayOperatorManager()
		if om == nil {
			om = auth.NewOperatorManager(auth.OperatorConfig{})
			SetGatewayOperatorManager(om)
		}

		// 1. Host & Origin validation (DNS rebinding and cross-origin defense)
		if !om.ValidateHost(r.Host) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"error":   "forbidden_host",
				"message": "Host header rejected by local security boundary",
			})
			return
		}
		origin := r.Header.Get("Origin")
		if origin != "" && !om.ValidateRequestOrigin(r) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"error":   "forbidden_origin",
				"message": "Origin rejected by cross-origin security boundary",
			})
			return
		}

		// 2. Determine Caller Authorization
		callerAuthorized := false

		// Check Bearer Token (existing API token or operator session token)
		authHeader := r.Header.Get("Authorization")
		if authHeader != "" && strings.HasPrefix(authHeader, "Bearer ") {
			tokStr := strings.TrimSpace(strings.TrimPrefix(authHeader, "Bearer "))
			if tokStr != "" {
				if om.VerifyAPIToken(tokStr) {
					callerAuthorized = true
				} else if s, ok := om.VerifySession(tokStr); ok && s != nil && s.IsOperator {
					callerAuthorized = true
				}
			}
		}

		// Check Session Cookie
		cookie, cErr := r.Cookie(auth.SessionCookieName)
		var callerSess *auth.Session
		if cErr == nil && cookie != nil && cookie.Value != "" {
			callerSess, _ = om.VerifySession(cookie.Value)
		}

		if !callerAuthorized && callerSess != nil && callerSess.IsOperator {
			// Cookie-based operator session requires valid CSRF token
			csrf := r.Header.Get(auth.CSRFHeaderName)
			if csrf == "" || !om.VerifyCSRFToken(callerSess.Token, csrf) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusForbidden)
				_ = json.NewEncoder(w).Encode(map[string]any{
					"error":   "invalid_csrf_token",
					"message": "Missing or invalid CSRF token on state-mutating request",
				})
				return
			}
			callerAuthorized = true
		}

		// If not authorized via verified session, check local bootstrap credentials
		addr := requestAddress(r)
		if !callerAuthorized && operatorSignIns.blocked(addr) {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Retry-After", strconv.Itoa(int(signInWindow/time.Second)))
			w.WriteHeader(http.StatusTooManyRequests)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"error":   "too_many_attempts",
				"message": "Too many wrong passwords from this address. Try again in 10 minutes.",
			})
			return
		}
		if !callerAuthorized {
			// The administrator's name and password (and two-step code, if
			// on): as Basic auth, or {"name","password","code"}. Osaio
			// logins never grant access to BombeCam.
			var creds struct {
				Name     string `json:"name"`
				Password string `json:"password"`
				Code     string `json:"code"`
			}
			if user, pass, ok := r.BasicAuth(); ok {
				creds.Name, creds.Password = user, pass
				creds.Code = r.Header.Get("X-BombeCam-Code")
			} else if r.Body != nil {
				bodyBytes, _ := io.ReadAll(http.MaxBytesReader(w, r.Body, 16<<10))
				if len(bodyBytes) > 0 {
					_ = json.Unmarshal(bodyBytes, &creds)
				}
			}
			if creds.Password != "" {
				if pm := getEffectivePM(optPM...); pm != nil && pm.GetProfile() != nil {
					admin, exists := pm.GetProfile().Admin()
					nameOK := strings.TrimSpace(creds.Name) == "" || strings.EqualFold(strings.TrimSpace(creds.Name), admin.Name)
					if exists && nameOK && auth.VerifyPassword(admin.PasswordHash.Expose(), creds.Password) {
						callerAuthorized = !admin.TwoStep()
						if admin.TwoStep() && creds.Code != "" {
							_, callerAuthorized = checkAdminCode(r, pm, creds.Code)
						}
					}
				}
				if callerAuthorized {
					operatorSignIns.succeeded(addr)
				} else {
					operatorSignIns.failed(addr)
				}
			}
		}

		if !callerAuthorized {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"error":   "unauthorized",
				"message": "Verified caller session or local bootstrap credentials required",
			})
			return
		}

		// If authenticated via bootstrap credentials and holds a visitor session cookie, elevate it
		if callerSess != nil && !callerSess.IsOperator {
			om.ElevateSession(callerSess.Token)
		}

		tok, err := om.GenerateAPIToken()
		if err != nil {
			http.Error(w, "failed to generate token", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok":    true,
			"token": tok,
		})
	}
	mux.HandleFunc("/api/v1/operator/token", tokenHandler)
	registerAdminRoutes(mux)
	mux.HandleFunc("/api/v1/admin/start-over", func(w http.ResponseWriter, r *http.Request) {
		handleAdminStartOver(w, r, sessionMgr, streamMgr, optPM...)
	})

	// -------------------------------------------------------------
	// 1. Session Status API
	// -------------------------------------------------------------
	mux.HandleFunc("/api/v1/session/status", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "GET required", http.StatusMethodNotAllowed)
			return
		}
		statusInfo := sessionsFor(sessionMgr).Status()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(statusInfo)
	})

	// -------------------------------------------------------------
	// 2. Onboarding Lifecycle API
	// -------------------------------------------------------------
	mux.HandleFunc("/api/v1/onboarding/status", func(w http.ResponseWriter, r *http.Request) {
		handleOnboardingStatus(w, r, sessionMgr, streamMgr, optPM...)
	})

	// Osaio app ID and server key: status (never the value) and saving a new one.
	mux.HandleFunc("/api/v1/app-id", func(w http.ResponseWriter, r *http.Request) {
		handleAppID(w, r, sessionMgr, streamMgr)
	})
	mux.HandleFunc("/api/v1/server-key", func(w http.ResponseWriter, r *http.Request) {
		handleServerKey(w, r, sessionMgr, streamMgr)
	})

	mux.HandleFunc("/api/v1/onboarding/setup", func(w http.ResponseWriter, r *http.Request) {
		handleOnboardingSetup(w, r, sessionMgr, streamMgr, optPM...)
	})

	mux.HandleFunc("/api/v1/onboarding/profile", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			handleOnboardingProfile(w, r, optPM...)
			return
		}
		if r.Method == http.MethodDelete {
			handleOnboardingReset(w, r, sessionMgr, streamMgr, optPM...)
			return
		}
		http.Error(w, "GET or DELETE required", http.StatusMethodNotAllowed)
	})

	mux.HandleFunc("/api/v1/onboarding/reset", func(w http.ResponseWriter, r *http.Request) {
		handleOnboardingReset(w, r, sessionMgr, streamMgr, optPM...)
	})

	// Onboarding aliases
	mux.HandleFunc("/api/v1/setup", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			handleOnboardingStatus(w, r, sessionMgr, streamMgr, optPM...)
			return
		}
		if r.Method == http.MethodPost {
			handleOnboardingSetup(w, r, sessionMgr, streamMgr, optPM...)
			return
		}
		http.Error(w, "GET or POST required", http.StatusMethodNotAllowed)
	})

	// -------------------------------------------------------------
	// 3. Authoritative Discovery & Caching API (Aliased)
	// -------------------------------------------------------------
	discoverHandler := func(w http.ResponseWriter, r *http.Request) {
		handleCameraDiscover(w, r, sessionMgr, streamMgr, optPM...)
	}
	mux.HandleFunc("/api/v1/onboarding/cameras/discover", discoverHandler)
	mux.HandleFunc("/api/v1/inventory/discover", discoverHandler)
	mux.HandleFunc("/api/v1/devices/discover", discoverHandler)

	// -------------------------------------------------------------
	// 4. Selective Camera Streaming Enrollment API (Aliased)
	// -------------------------------------------------------------
	enrollHandler := func(w http.ResponseWriter, r *http.Request) {
		handleCameraEnroll(w, r, sessionMgr, streamMgr, optPM...)
	}
	mux.HandleFunc("/api/v1/onboarding/cameras/enroll", enrollHandler)
	mux.HandleFunc("/api/v1/inventory/enroll", enrollHandler)
	mux.HandleFunc("/api/v1/devices/enroll", enrollHandler)

	// Add-cameras lifecycle: reusable per-login search, additive add, and
	// per-camera remove. These coexist with the legacy discover/enroll routes.
	mux.HandleFunc("/api/v1/cameras/search", func(w http.ResponseWriter, r *http.Request) {
		handleCameraSearch(w, r, streamMgr, optPM...)
	})
	mux.HandleFunc("/api/v1/cameras/add", func(w http.ResponseWriter, r *http.Request) {
		handleCameraAdd(w, r, streamMgr, optPM...)
	})
	// Osaio logins: the pool of logins used to find and reach cameras.
	mux.HandleFunc("/api/v1/accounts", func(w http.ResponseWriter, r *http.Request) {
		handleAccountsList(w, r, optPM...)
	})
	mux.HandleFunc("/api/v1/accounts/signin", func(w http.ResponseWriter, r *http.Request) {
		handleAccountSignIn(w, r, sessionMgr, streamMgr, optPM...)
	})
	mux.HandleFunc("/api/v1/accounts/cameras", func(w http.ResponseWriter, r *http.Request) {
		handleAccountCameras(w, r, streamMgr, optPM...)
	})
	mux.HandleFunc("/api/v1/accounts/remove", func(w http.ResponseWriter, r *http.Request) {
		handleAccountRemove(w, r, sessionMgr, streamMgr, optPM...)
	})
	// Delete all stored information (logins + cameras); returns to first-run.
	mux.HandleFunc("/api/v1/profile/forget", func(w http.ResponseWriter, r *http.Request) {
		handleOnboardingReset(w, r, sessionMgr, streamMgr, optPM...)
	})

	// -------------------------------------------------------------
	// 5. "Block cloud video?" (router allowlist) and stream start/stop
	// -------------------------------------------------------------
	mux.HandleFunc("/api/v1/control/status", handleControlStatus)
	mux.HandleFunc("/api/v1/privacy", func(w http.ResponseWriter, r *http.Request) {
		handlePrivacyStatus(w, r, streamMgr, optPM...)
	})
	mux.HandleFunc("/api/v1/privacy/apply", func(w http.ResponseWriter, r *http.Request) {
		handlePrivacyApply(w, r, streamMgr, optPM...)
	})
	mux.HandleFunc("/api/v1/privacy/camera", func(w http.ResponseWriter, r *http.Request) {
		handlePrivacyCamera(w, r, streamMgr, optPM...)
	})
	mux.HandleFunc("/api/v1/privacy/block-all", func(w http.ResponseWriter, r *http.Request) {
		handlePrivacyBlockAll(w, r, streamMgr, optPM...)
	})
	mux.HandleFunc("/api/v1/privacy/router/connect", func(w http.ResponseWriter, r *http.Request) {
		handleRouterConnect(w, r, streamMgr, optPM...)
	})
	mux.HandleFunc("/api/v1/privacy/router/disconnect", func(w http.ResponseWriter, r *http.Request) {
		handleRouterDisconnect(w, r, streamMgr, optPM...)
	})
	mux.HandleFunc("/api/v1/privacy/router/check", func(w http.ResponseWriter, r *http.Request) {
		handleRouterCheck(w, r, streamMgr, optPM...)
	})
	mux.HandleFunc("/api/v1/privacy/forget-router", func(w http.ResponseWriter, r *http.Request) {
		handlePrivacyForgetRouter(w, r, optPM...)
	})
	mux.HandleFunc("/api/v1/privacy/router-script", handlePrivacyRouterScript)

	mux.HandleFunc("/api/v1/gateway/start", func(w http.ResponseWriter, r *http.Request) {
		handleGatewayStart(w, r, streamMgr)
	})
	mux.HandleFunc("/api/v1/gateway/stop", func(w http.ResponseWriter, r *http.Request) {
		handleGatewayStop(w, r, streamMgr)
	})
	mux.HandleFunc("/api/v1/gateway/shutdown", handleGatewayShutdown)
	mux.HandleFunc("/api/v1/gateway/info", handleGatewayInfo)
	mux.HandleFunc("/api/v1/gateway/autostart", handleAutostart)
	mux.HandleFunc("/favicon.ico", handleFavicon)

	// -------------------------------------------------------------
	// 6. Third-Party Integrations API
	// -------------------------------------------------------------
	mux.HandleFunc("/api/v1/integrations/frigate", func(w http.ResponseWriter, r *http.Request) {
		handleFrigateIntegration(w, r, streamMgr, optPM...)
	})

	mux.HandleFunc("/api/v1/integrations/homeassistant", func(w http.ResponseWriter, r *http.Request) {
		handleHomeAssistantIntegration(w, r, streamMgr, optPM...)
	})

	mux.HandleFunc("/api/v1/integrations/streams", func(w http.ResponseWriter, r *http.Request) {
		handleStreamsIntegration(w, r, streamMgr, optPM...)
	})

	// "Use with Frigate / Home Assistant" settings (this PC only, like all of /api)
	mux.HandleFunc("/api/v1/integrations/settings", func(w http.ResponseWriter, r *http.Request) {
		handleIntegrationSettings(w, r, streamMgr, optPM...)
	})

	// -------------------------------------------------------------
	// Recordings
	// -------------------------------------------------------------
	// GET /api/v1/recordings          list recorded segments
	// GET /api/v1/recordings/download?path=...  download one segment
	mux.HandleFunc("/api/v1/recordings", func(w http.ResponseWriter, r *http.Request) {
		handleRecordings(w, r)
	})
	mux.HandleFunc("/api/v1/recordings/download", func(w http.ResponseWriter, r *http.Request) {
		handleRecordingDownload(w, r)
	})

	// -------------------------------------------------------------
	// 7. RESTful Camera Routes (/api/v1/cameras/{id}/...)
	// -------------------------------------------------------------
	mux.HandleFunc("/api/v1/cameras/", func(w http.ResponseWriter, r *http.Request) {
		if !validateOperatorRequest(w, r) {
			return
		}
		relPath := strings.TrimPrefix(r.URL.Path, "/api/v1/cameras/")
		parts := strings.Split(relPath, "/")

		if len(parts) == 0 || parts[0] == "" {
			http.NotFound(w, r)
			return
		}

		camID := parts[0]

		// Route: DELETE /api/v1/cameras/{id}  (remove a camera from BombeCam)
		if len(parts) == 1 && r.Method == http.MethodDelete {
			handleCameraRemove(w, r, camID, streamMgr, optPM...)
			return
		}

		// Route: GET /api/v1/cameras/{id}/status
		if len(parts) == 2 && parts[1] == "status" && r.Method == http.MethodGet {
			mc, ok := streamMgr.GetCamera(camID)
			if !ok {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusNotFound)
				_ = json.NewEncoder(w).Encode(map[string]any{"error": "camera not found"})
				return
			}
			resp := mc.GetStatus(GatewayControlChannel(), r)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(resp)
			return
		}

		// Route: GET /api/v1/cameras/{id}/shadow
		if len(parts) == 2 && parts[1] == "shadow" && r.Method == http.MethodGet {
			handleCameraShadow(w, r, camID, streamMgr)
			return
		}

		// Route: POST /api/v1/cameras/{id}/control
		if len(parts) == 2 && parts[1] == "control" && r.Method == http.MethodPost {
			handleCameraControl(w, r, camID, streamMgr)
			return
		}

		// Route: POST /api/v1/cameras/{id}/ptz
		if len(parts) == 2 && parts[1] == "ptz" && r.Method == http.MethodPost {
			handleDedicatedPTZ(w, r, camID, streamMgr)
			return
		}

		// Route: GET /api/v1/cameras/{id}/stream
		if len(parts) == 2 && parts[1] == "stream" && r.Method == http.MethodGet {
			handleCameraStream(w, r, camID, streamMgr)
			return
		}

		// Route: POST /api/v1/cameras/{id}/unlock
		if len(parts) == 2 && parts[1] == "unlock" && r.Method == http.MethodPost {
			mc, err := streamMgr.Unlock(camID)
			if err != nil {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusNotFound)
				_ = json.NewEncoder(w).Encode(map[string]any{"error": "camera not found"})
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"status":           "unlocked",
				"camera":           mc.Name,
				"uuid":             mc.UUID,
				"remaining_budget": 3,
				"ceiling_seconds":  30,
			})
			return
		}

		// Route: POST /api/v1/cameras/{id}/mode
		if len(parts) == 2 && parts[1] == "mode" && r.Method == http.MethodPost {
			var body struct {
				StrictManual bool `json:"strict_manual"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(map[string]any{"error": "invalid json payload"})
				return
			}
			if err := streamMgr.SetStrictManual(camID, body.StrictManual); err != nil {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusNotFound)
				_ = json.NewEncoder(w).Encode(map[string]any{"error": "camera not found"})
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok":            true,
				"strict_manual": body.StrictManual,
			})
			return
		}

		// Route: POST /api/v1/cameras/{id}/talk/start
		if len(parts) == 3 && parts[1] == "talk" && parts[2] == "start" && r.Method == http.MethodPost {
			handleTalkStart(w, r, camID, streamMgr)
			return
		}

		// Route: POST /api/v1/cameras/{id}/talk/stop
		if len(parts) == 3 && parts[1] == "talk" && parts[2] == "stop" && r.Method == http.MethodPost {
			handleTalkStop(w, r, camID, streamMgr)
			return
		}

		// Route: POST /api/v1/cameras/{id}/talk/stream
		if len(parts) == 3 && parts[1] == "talk" && parts[2] == "stream" && r.Method == http.MethodPost {
			handleTalkStream(w, r, camID, streamMgr)
			return
		}

		// Route: /api/v1/cameras/{id}/whep (POST, OPTIONS, PATCH, DELETE)
		if len(parts) == 2 && parts[1] == "whep" {
			handleWHEP(w, r, camID, streamMgr)
			return
		}

		// Route: GET /api/v1/cameras/{id}/snapshot.jpg (id or stream name)
		if len(parts) == 2 && parts[1] == "snapshot.jpg" {
			handleSnapshot(w, r, streamMgr, camID)
			return
		}

		// Route: POST /api/v1/cameras/{id}/stream-name
		if len(parts) == 2 && parts[1] == "stream-name" && r.Method == http.MethodPost {
			handleCameraStreamName(w, r, camID, streamMgr, optPM...)
			return
		}

		// Route: GET /api/v1/cameras/{id}/hls/... (the viewer's HLS, relayed)
		if len(parts) >= 3 && parts[1] == "hls" && (r.Method == http.MethodGet || r.Method == http.MethodHead) {
			handleHLSRelay(w, r, camID, strings.Join(parts[2:], "/"), streamMgr)
			return
		}

		http.NotFound(w, r)
	})

	// -------------------------------------------------------------
	// 5. Backwards-Compatible Legacy Routes
	// -------------------------------------------------------------

	// Legacy /status queries StreamManager so cameras in LOCKOUT remain visible
	mux.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		cams := streamMgr.GetAllCameras()
		cameras := []map[string]any{}
		for _, mc := range cams {
			mc.mu.RLock()
			sess := mc.Session
			name := mc.Name
			uuid := mc.UUID
			model := mc.Model
			streaming := mc.Streaming
			mc.mu.RUnlock()

			camInfo := map[string]any{
				"name":      name,
				"uuid":      uuid,
				"model":     model,
				"streaming": streaming,
			}
			if sess != nil {
				sess.mu.Lock()
				camInfo["state"] = string(sess.State)
				camInfo["recovery_budget"] = sess.RecoveryBudget
				camInfo["strict_manual"] = sess.StrictManual
				sess.mu.Unlock()
			}
			cameras = append(cameras, camInfo)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status":  "ok",
			"cameras": cameras,
		})
	})

	// Legacy /camera/unlock
	mux.HandleFunc("/camera/unlock", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST required", http.StatusMethodNotAllowed)
			return
		}
		if om := GatewayOperatorManager(); om != nil {
			if !om.ValidateSecurityBoundary(w, r, true) {
				return
			}
		}
		cam := r.URL.Query().Get("cam")
		sess := findCameraSession(cam)
		if sess == nil {
			http.Error(w, "camera session not found", http.StatusNotFound)
			return
		}
		sess.Unlock()
		sess.mu.Lock()
		state := sess.State
		budget := sess.RecoveryBudget
		sess.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok":              true,
			"camera":          sess.Name,
			"uuid":            sess.UUID,
			"state":           string(state),
			"recovery_budget": budget,
		})
	})

	// Legacy /camera/mode
	mux.HandleFunc("/camera/mode", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST required", http.StatusMethodNotAllowed)
			return
		}
		if om := GatewayOperatorManager(); om != nil {
			if !om.ValidateSecurityBoundary(w, r, true) {
				return
			}
		}
		cam := r.URL.Query().Get("cam")
		manualStr := r.URL.Query().Get("manual")
		manual := manualStr == "true" || manualStr == "1"

		sess := findCameraSession(cam)
		if sess == nil {
			sessionsMu.Lock()
			for _, s := range sessions {
				s.SetStrictManual(manual)
			}
			sessionsMu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok":            true,
				"camera":        cam,
				"strict_manual": manual,
			})
			return
		}
		sess.SetStrictManual(manual)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok":            true,
			"camera":        sess.Name,
			"uuid":          sess.UUID,
			"strict_manual": sess.IsStrictManual(),
		})
	})

	// Legacy /ptz
	mux.HandleFunc("/ptz", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST required", http.StatusMethodNotAllowed)
			return
		}
		if om := GatewayOperatorManager(); om != nil {
			if !om.ValidateSecurityBoundary(w, r, signInRequired(r)) {
				return
			}
		}
		cam := r.URL.Query().Get("cam")
		dirStr := r.URL.Query().Get("dir")
		durStr := r.URL.Query().Get("dur")

		dir := 0
		switch strings.ToLower(dirStr) {
		case "left", "1":
			dir = 1
		case "right", "2":
			dir = 2
		case "up", "top", "3":
			dir = 3
		case "down", "bottom", "4":
			dir = 4
		case "stop", "0":
			dir = 0
		default:
			if d, err := strconv.Atoi(dirStr); err == nil {
				dir = d
			} else {
				http.Error(w, "invalid dir (left/1, right/2, up/3, down/4, stop/0)", 400)
				return
			}
		}

		dur := 400
		if durStr != "" {
			if d, err := strconv.Atoi(durStr); err == nil && d >= 0 {
				dur = d
			}
		}

		targetUUID, targetName, cc, ok := legacyControlTarget(w, cam, streamMgr)
		if !ok {
			return
		}
		fmt.Printf("[%s] [PTZ] dispatching direction=%d (dur=%dms)...\n", targetName, dir, dur)
		if err := cc.MovePTZ(r.Context(), targetUUID, dir, dur); err != nil {
			http.Error(w, fmt.Sprintf("failed to send: %v", err), 500)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok":          true,
			"camera":      targetName,
			"uuid":        targetUUID,
			"direction":   dir,
			"duration_ms": dur,
		})
	})

	// Legacy /ir
	mux.HandleFunc("/ir", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST required", http.StatusMethodNotAllowed)
			return
		}
		if om := GatewayOperatorManager(); om != nil {
			if !om.ValidateSecurityBoundary(w, r, signInRequired(r)) {
				return
			}
		}
		cam := r.URL.Query().Get("cam")
		modeStr := strings.ToLower(r.URL.Query().Get("mode"))
		targetUUID, targetName, cc, ok := legacyControlTarget(w, cam, streamMgr)
		if !ok {
			return
		}

		lightSw := 0
		irMode, okMode := bridge.ParseIRMode(modeStr)
		if !okMode {
			http.Error(w, "invalid mode: use auto, on (night vision) or off", 400)
			return
		}
		normMode := bridge.IRModeName(irMode)

		fmt.Printf("[%s] [IR] dispatching mode=%d\n", targetName, irMode)
		if err := cc.SetIR(r.Context(), targetUUID, irMode); err != nil {
			http.Error(w, fmt.Sprintf("failed to send: %v", err), 500)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok":           true,
			"camera":       targetName,
			"uuid":         targetUUID,
			"requested":    map[string]any{"mode": normMode, "IrLedMode": irMode, "LightSW": lightSw},
			"confirmation": "unconfirmed",
		})
	})

	// Legacy /ir/status
	mux.HandleFunc("/ir/status", func(w http.ResponseWriter, r *http.Request) {
		if om := GatewayOperatorManager(); om != nil {
			if !om.ValidateSecurityBoundary(w, r, true) {
				return
			}
		}
		cam := r.URL.Query().Get("cam")
		targetUUID, targetName, cc, ok := legacyControlTarget(w, cam, streamMgr)
		if !ok {
			return
		}

		ctx, cancel := context.WithTimeout(r.Context(), 1500*time.Millisecond)
		defer cancel()
		mode, err := cc.GetIR(ctx, targetUUID)
		if err != nil {
			http.Error(w, fmt.Sprintf("failed to query ir status: %v", err), 500)
			return
		}

		var modeStr string
		switch mode {
		case 1:
			modeStr = "off"
		case 2:
			modeStr = "on"
		default:
			modeStr = "auto"
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok":        true,
			"camera":    targetName,
			"uuid":      targetUUID,
			"mode":      modeStr,
			"IrLedMode": mode,
		})
	})

	// Legacy /led
	mux.HandleFunc("/led", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST required", http.StatusMethodNotAllowed)
			return
		}
		if om := GatewayOperatorManager(); om != nil {
			if !om.ValidateSecurityBoundary(w, r, signInRequired(r)) {
				return
			}
		}
		cam := r.URL.Query().Get("cam")
		modeStr := strings.ToLower(r.URL.Query().Get("mode"))
		targetUUID, targetName, cc, ok := legacyControlTarget(w, cam, streamMgr)
		if !ok {
			return
		}

		ledVal := 1
		normMode := "on"
		switch modeStr {
		case "off", "0", "false":
			ledVal = 0
			normMode = "off"
		case "on", "1", "true", "":
			ledVal = 1
			normMode = "on"
		default:
			http.Error(w, "invalid mode: use on/1/true or off/0/false", 400)
			return
		}

		fmt.Printf("[%s] [LED] dispatching on=%v\n", targetName, ledVal == 1)
		if err := cc.SetLED(r.Context(), targetUUID, ledVal == 1); err != nil {
			http.Error(w, fmt.Sprintf("failed to send: %v", err), 500)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok":           true,
			"camera":       targetName,
			"uuid":         targetUUID,
			"requested":    map[string]any{"mode": normMode, "LedOnOff": ledVal},
			"confirmation": "unconfirmed",
		})
	})

	// Legacy /led/status
	mux.HandleFunc("/led/status", func(w http.ResponseWriter, r *http.Request) {
		if om := GatewayOperatorManager(); om != nil {
			if !om.ValidateSecurityBoundary(w, r, true) {
				return
			}
		}
		cam := r.URL.Query().Get("cam")
		targetUUID, targetName, cc, ok := legacyControlTarget(w, cam, streamMgr)
		if !ok {
			return
		}

		ctx, cancel := context.WithTimeout(r.Context(), 1500*time.Millisecond)
		defer cancel()
		mode := 0
		on, err := cc.GetLED(ctx, targetUUID)
		if on {
			mode = 1
		}
		if err != nil {
			http.Error(w, fmt.Sprintf("failed to query led status: %v", err), 500)
			return
		}

		statusStr := "off"
		if mode == 1 {
			statusStr = "on"
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok":       true,
			"camera":   targetName,
			"uuid":     targetUUID,
			"LedOnOff": mode,
			"status":   statusStr,
		})
	})

	// Legacy /light
	mux.HandleFunc("/light", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST required", http.StatusMethodNotAllowed)
			return
		}
		if om := GatewayOperatorManager(); om != nil {
			if !om.ValidateSecurityBoundary(w, r, signInRequired(r)) {
				return
			}
		}
		cam := r.URL.Query().Get("cam")
		modeStr := strings.ToLower(r.URL.Query().Get("mode"))
		targetUUID, targetName, cc, ok := legacyControlTarget(w, cam, streamMgr)
		if !ok {
			return
		}

		lightVal := 1
		normMode := "on"
		switch modeStr {
		case "off", "0", "false":
			lightVal = 0
			normMode = "off"
		case "on", "1", "true", "":
			lightVal = 1
			normMode = "on"
		default:
			http.Error(w, "invalid mode: use on/1/true or off/0/false", 400)
			return
		}

		fmt.Printf("[%s] [LIGHT] dispatching on=%v\n", targetName, lightVal == 1)
		if err := cc.SetLight(r.Context(), targetUUID, lightVal == 1); err != nil {
			http.Error(w, fmt.Sprintf("failed to send: %v", err), 500)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok":           true,
			"camera":       targetName,
			"uuid":         targetUUID,
			"requested":    map[string]any{"mode": normMode, "LightSW": lightVal},
			"confirmation": "unconfirmed",
		})
	})

	// Legacy /mic
	mux.HandleFunc("/mic", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST required", http.StatusMethodNotAllowed)
			return
		}
		if om := GatewayOperatorManager(); om != nil {
			if !om.ValidateSecurityBoundary(w, r, true) {
				return
			}
		}
		cam := r.URL.Query().Get("cam")
		enableStr := strings.ToLower(r.URL.Query().Get("enable"))
		target := findTargetCam(cam)
		if target == nil {
			http.Error(w, "camera not found or not connected", 503)
			return
		}
		cc := cameraControl(streamMgr, target.dev.UUID)
		if cc == nil {
			http.Error(w, "camera not found or not connected", 503)
			return
		}

		enable := 1
		if enableStr == "0" || enableStr == "false" || enableStr == "off" {
			enable = 0
		}

		fmt.Printf("[%s] [MIC] dispatching AudioRecordSw=%d\n", target.dev.Name, enable)
		if err := cc.SetAttribute(r.Context(), target.dev.UUID, "AudioRecordSw", enable); err != nil {
			http.Error(w, fmt.Sprintf("failed to send: %v", err), 500)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok":            true,
			"camera":        target.dev.Name,
			"uuid":          target.dev.UUID,
			"AudioRecordSw": enable,
		})
	})

	// Legacy /cmd sends a raw vendor message for debugging. It is the one
	// route that bypasses ControlChannel, on purpose: its input is already a
	// vendor method name and payload.
	mux.HandleFunc("/cmd", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST required", http.StatusMethodNotAllowed)
			return
		}
		if om := GatewayOperatorManager(); om != nil {
			if !om.ValidateSecurityBoundary(w, r, true) {
				return
			}
		}
		cam := r.URL.Query().Get("cam")
		method := r.URL.Query().Get("method")
		jsonStr := r.URL.Query().Get("json")

		if r.Method == http.MethodPost {
			body, _ := io.ReadAll(r.Body)
			if len(body) > 0 {
				jsonStr = string(body)
			}
		}

		if method == "" {
			http.Error(w, "missing method parameter (e.g. atr.get, atr.set)", 400)
			return
		}

		target := findTargetCam(cam)
		if target == nil || target.sig == nil {
			http.Error(w, "camera not found or not connected", 503)
			return
		}

		var payload any
		if jsonStr != "" {
			if err := json.Unmarshal([]byte(jsonStr), &payload); err != nil {
				http.Error(w, fmt.Sprintf("invalid json payload: %v", err), 400)
				return
			}
		} else {
			payload = map[string]any{}
		}

		fmt.Printf("[%s] [CMD] dispatching method=%s data=%+v\n", target.dev.Name, method, payload)
		if isLocalControl(GatewayControlChannel()) {
			http.Error(w, "raw vendor commands are disabled during local control testing", http.StatusConflict)
			return
		}
		err := target.sig.Send(method, payload)
		if err != nil {
			http.Error(w, fmt.Sprintf("failed to send: %v", err), 500)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok":      true,
			"camera":  target.dev.Name,
			"uuid":    target.dev.UUID,
			"method":  method,
			"payload": payload,
		})
	})

	// Legacy /talk/start
	mux.HandleFunc("/talk/start", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST required", http.StatusMethodNotAllowed)
			return
		}
		if om := GatewayOperatorManager(); om != nil {
			if !om.ValidateSecurityBoundary(w, r, signInRequired(r)) {
				return
			}
		}
		cam := r.URL.Query().Get("cam")
		target := findTargetCam(cam)
		cc := GatewayControlChannel()

		if target == nil || target.viewer == nil {
			if cc != nil && cam != "" {
				sessionID := fmt.Sprintf("session-%s-%d", cam, time.Now().Unix())
				if err := cc.ArmTalk(r.Context(), cam, true, sessionID); err != nil {
					http.Error(w, fmt.Sprintf("failed to arm talk: %v", err), 500)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{
					"ok":          true,
					"camera":      cam,
					"uuid":        cam,
					"talk_active": true,
				})
				return
			}
			http.Error(w, "camera not found or media session not active", 503)
			return
		}

		ret, err := target.viewer.SetTalk(true)
		if err != nil {
			http.Error(w, fmt.Sprintf("failed to start talk: %v", err), 500)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok":          true,
			"camera":      target.dev.Name,
			"uuid":        target.dev.UUID,
			"talk_active": true,
			"ret":         ret,
		})
	})

	// Legacy /talk/stop
	mux.HandleFunc("/talk/stop", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST required", http.StatusMethodNotAllowed)
			return
		}
		if om := GatewayOperatorManager(); om != nil {
			if !om.ValidateSecurityBoundary(w, r, signInRequired(r)) {
				return
			}
		}
		cam := r.URL.Query().Get("cam")
		target := findTargetCam(cam)
		cc := GatewayControlChannel()

		if target == nil || target.viewer == nil {
			if cc != nil && cam != "" {
				if err := cc.ArmTalk(r.Context(), cam, false, ""); err != nil {
					http.Error(w, fmt.Sprintf("failed to disarm talk: %v", err), 500)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{
					"ok":          true,
					"camera":      cam,
					"uuid":        cam,
					"talk_active": false,
				})
				return
			}
			http.Error(w, "camera not found or media session not active", 503)
			return
		}

		ret, err := target.viewer.SetTalk(false)
		if err != nil {
			http.Error(w, fmt.Sprintf("failed to stop talk: %v", err), 500)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok":          true,
			"camera":      target.dev.Name,
			"uuid":        target.dev.UUID,
			"talk_active": false,
			"ret":         ret,
		})
	})

	// Legacy /talk/stream
	mux.HandleFunc("/talk/stream", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST required", http.StatusMethodNotAllowed)
			return
		}
		if om := GatewayOperatorManager(); om != nil {
			if !om.ValidateSecurityBoundary(w, r, true) {
				return
			}
		}

		cam := r.URL.Query().Get("cam")
		target := findTargetCam(cam)
		if target == nil || target.viewer == nil {
			http.Error(w, "camera not found or media session not active", 503)
			return
		}

		frames, err := bridge.ReadAllADTSFrames(r.Body)
		if err != nil {
			http.Error(w, fmt.Sprintf("invalid audio payload: %v", err), 400)
			return
		}

		if !target.viewer.IsTalkActive() {
			_, _ = target.viewer.SetTalk(true)
		}

		target.viewer.TouchAudioUp()

		for _, frame := range frames {
			if err := target.viewer.WriteAudioFrame(frame); err != nil {
				http.Error(w, fmt.Sprintf("stream audio frame: %v", err), 500)
				return
			}
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok":              true,
			"camera":          target.dev.Name,
			"uuid":            target.dev.UUID,
			"frames_streamed": len(frames),
		})
	})

	// -------------------------------------------------------------
	// 8. Embedded Web UI
	// -------------------------------------------------------------
	mux.Handle("/", ui.Handler())

	return mux
}

// legacyControlTarget resolves a legacy route's ?cam= (UUID or name) to a
// camera and the channel that controls it, answering 503 when there is none.
func legacyControlTarget(w http.ResponseWriter, cam string, streamMgr *StreamManager) (uuid, name string, cc bridge.ControlChannel, ok bool) {
	switch target := findTargetCam(cam); {
	case target != nil:
		uuid, name = target.dev.UUID, target.dev.Name
	case GatewayControlChannel() != nil && cam != "":
		uuid, name = cam, cam
	default:
		http.Error(w, "camera not found or not connected", 503)
		return "", "", nil, false
	}
	if cc = cameraControl(streamMgr, uuid); cc == nil {
		http.Error(w, "camera not found or not connected", 503)
		return "", "", nil, false
	}
	return uuid, name, cc, true
}

// controlFailed reports a command the control channel could not send.
func controlFailed(w http.ResponseWriter, what string, err error) {
	writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": fmt.Sprintf("%s failed: %v", what, err)})
}

// handleCameraControl handles POST /api/v1/cameras/{id}/control
func handleCameraControl(w http.ResponseWriter, r *http.Request, camID string, streamMgr *StreamManager) {
	mc, ok := streamMgr.GetCamera(camID)
	if !ok {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": "camera not found"})
		return
	}

	cc := cameraControl(streamMgr, camID)
	if cc == nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": "control transport unavailable"})
		return
	}

	var req struct {
		Action     string         `json:"action"`
		Value      any            `json:"value,omitempty"`
		Mode       any            `json:"mode,omitempty"`
		Direction  any            `json:"direction,omitempty"`
		DurationMs any            `json:"duration_ms,omitempty"`
		Params     map[string]any `json:"params,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": "invalid json payload"})
		return
	}

	// resolveParam searches for a parameter across top-level fields and nested req.Params keys.
	resolveParam := func(topLevel1 any, topLevel2 any, nestedKeys ...string) any {
		if topLevel1 != nil {
			return topLevel1
		}
		if topLevel2 != nil {
			return topLevel2
		}
		if req.Params != nil {
			for _, k := range nestedKeys {
				if v, exists := req.Params[k]; exists && v != nil {
					return v
				}
			}
		}
		return nil
	}

	start := time.Now()
	var requestedState map[string]any

	normAction := strings.ToLower(strings.TrimSpace(req.Action))
	switch normAction {
	case "ir", "set_ir", "set_ir_auto", "set_ir_on", "set_ir_off":
		// Mode first: "value" is a 0/1 convenience field for on/off switches and
		// cannot express IR's three states.
		rawMode := resolveParam(req.Mode, req.Value, "mode", "value", "IrLedMode")
		if rawMode == nil {
			switch normAction {
			case "set_ir_off":
				rawMode = "off"
			case "set_ir_on":
				rawMode = "on"
			default:
				rawMode = "auto"
			}
		}
		modeVal, okMode := bridge.ParseIRMode(rawMode)
		if !okMode {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": "invalid ir mode: use auto, on (night vision) or off"})
			return
		}

		if err := cc.SetIR(r.Context(), camID, modeVal); err != nil {
			controlFailed(w, "ir control", err)
			return
		}
		requestedState = map[string]any{"IrLedMode": modeVal}

	case "led", "set_led", "set_led_on", "set_led_off":
		ledVal := 1

		rawLed := resolveParam(req.Value, req.Mode, "mode", "value", "on", "LedOnOff")
		if rawLed != nil {
			switch v := rawLed.(type) {
			case string:
				switch strings.ToLower(strings.TrimSpace(v)) {
				case "off", "0", "false":
					ledVal = 0
				case "on", "1", "true", "":
					ledVal = 1
				}
			case bool:
				if !v {
					ledVal = 0
				} else {
					ledVal = 1
				}
			case float64:
				if int(v) == 0 {
					ledVal = 0
				} else {
					ledVal = 1
				}
			case int:
				if v == 0 {
					ledVal = 0
				} else {
					ledVal = 1
				}
			}
		} else {
			// Action-name fallback if value was omitted in compound actions
			switch normAction {
			case "set_led_off":
				ledVal = 0
			case "set_led_on":
				ledVal = 1
			default:
				mc.mu.RLock()
				cur := mc.ObservedLED
				mc.mu.RUnlock()
				if cur == "on" {
					ledVal = 0
				} else {
					ledVal = 1
				}
			}
		}

		if err := cc.SetLED(r.Context(), camID, ledVal == 1); err != nil {
			controlFailed(w, "led control", err)
			return
		}
		requestedState = map[string]any{"LedOnOff": ledVal}

	case "light", "set_light":
		lightVal := 1
		rawLight := resolveParam(req.Value, req.Mode, "mode", "value", "on", "LightSW")
		if rawLight != nil {
			switch v := rawLight.(type) {
			case string:
				switch strings.ToLower(strings.TrimSpace(v)) {
				case "off", "0", "false":
					lightVal = 0
				case "on", "1", "true", "":
					lightVal = 1
				}
			case bool:
				if !v {
					lightVal = 0
				} else {
					lightVal = 1
				}
			case float64:
				if int(v) == 0 {
					lightVal = 0
				} else {
					lightVal = 1
				}
			case int:
				if v == 0 {
					lightVal = 0
				} else {
					lightVal = 1
				}
			}
		}

		if err := cc.SetLight(r.Context(), camID, lightVal == 1); err != nil {
			controlFailed(w, "light control", err)
			return
		}
		requestedState = map[string]any{"LightSW": lightVal}

	case "ptz", "set_ptz":
		dir := 0
		dur := 400
		rawDir := resolveParam(req.Direction, req.Value, "direction", "value")
		if rawDir != nil {
			switch v := rawDir.(type) {
			case string:
				switch strings.ToLower(strings.TrimSpace(v)) {
				case "left", "1":
					dir = 1
				case "right", "2":
					dir = 2
				case "up", "top", "3":
					dir = 3
				case "down", "bottom", "4":
					dir = 4
				case "stop", "0", "":
					dir = 0
				}
			case float64:
				dir = int(v)
			case int:
				dir = v
			}
		}
		rawDur := resolveParam(req.DurationMs, nil, "duration_ms", "duration")
		if rawDur != nil {
			switch v := rawDur.(type) {
			case float64:
				if v >= 0 {
					dur = int(v)
				}
			case int:
				if v >= 0 {
					dur = v
				}
			}
		}

		if err := cc.MovePTZ(r.Context(), camID, dir, dur); err != nil {
			controlFailed(w, "ptz move", err)
			return
		}
		requestedState = map[string]any{"direction": dir}

	case "motion", "motion_detection", "sound", "sound_detection":
		// The camera's own detection switches (the ones in the Osaio app):
		// MotionDetectSW / SoundDetectSW, 1 = on, 0 = off.
		key := "MotionDetectSW"
		if strings.HasPrefix(normAction, "sound") {
			key = "SoundDetectSW"
		}
		val, okVal := parseOnOff(resolveParam(req.Value, req.Mode, "mode", "value", "on", key))
		if !okVal {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": "invalid value: use on or off"})
			return
		}
		fmt.Printf("[%s] [SETTINGS] dispatching %s=%d\n", mc.Name, key, val)
		if sendErr := cc.SetAttribute(r.Context(), camID, key, val); sendErr != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": fmt.Sprintf("%s change failed: %v", map[string]string{"MotionDetectSW": "motion detection", "SoundDetectSW": "sound detection"}[key], sendErr)})
			return
		}
		requestedState = map[string]any{key: val}

	default:
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": fmt.Sprintf("unsupported action: %s (supported: ir, led, light, ptz, motion, sound)", req.Action),
		})
		return
	}

	// A successful dispatch is enough for these controls. Camera telemetry is
	// optional and belongs to the separate shadow/status path, not this click.
	durMs := time.Since(start).Milliseconds()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ok":              true,
		"camera":          mc.Name,
		"uuid":            mc.UUID,
		"action":          req.Action,
		"requested_state": requestedState,
		"status":          "sent",
		"confirmed_state": map[string]any{},
		"confirmation":    "not_requested",
		"duration_ms":     durMs,
	})
}

// handleCameraShadow handles GET /api/v1/cameras/{id}/shadow
func handleCameraShadow(w http.ResponseWriter, r *http.Request, camID string, streamMgr *StreamManager) {
	mc, ok := streamMgr.GetCamera(camID)
	if !ok {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": "camera not found"})
		return
	}

	cc := GatewayControlChannel()
	if local, ok := cc.(*bridge.MQTTControlChannel); ok && local != nil {
		doc, err := local.ReportedShadow(camID)
		if err != nil {
			doc = &shadowshim.ShadowDocument{State: shadowshim.ShadowState{Reported: map[string]any{}}}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"state": doc.State, "metadata": doc.Metadata, "timestamp": doc.Timestamp, "version": doc.Version, "control": local.LocalStatus(camID)})
		return
	}
	if cc == nil {
		cc = cameraControl(streamMgr, camID)
	}
	ctx, cancel := context.WithTimeout(r.Context(), 1500*time.Millisecond)
	defer cancel()
	reported, err := readCameraAttributes(ctx, cc, camID, "IrLedMode", "LedOnOff", "LightSW", "MotionDetectSW", "SoundDetectSW")
	updateCameraObservations(mc, reported)
	ts := int64(0)
	if len(reported) > 0 {
		ts = time.Now().Unix()
	}
	readbackError := ""
	if err != nil {
		readbackError = err.Error()
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"state": map[string]any{"reported": reported}, "timestamp": ts, "readback_error": readbackError})
}

// handleTalkStart handles POST /api/v1/cameras/{id}/talk/start
func handleTalkStart(w http.ResponseWriter, r *http.Request, camID string, streamMgr *StreamManager) {
	mc, ok := streamMgr.GetCamera(camID)
	if !ok {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": "camera not found"})
		return
	}

	mc.mu.RLock()
	viewer := mc.Viewer
	mc.mu.RUnlock()

	cc := GatewayControlChannel()
	if viewer != nil {
		ret, err := viewer.SetTalk(true)
		if err != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": fmt.Sprintf("failed to start talk: %v", err)})
			return
		}
		if ret != 0 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadGateway)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": fmt.Sprintf("the camera refused talkback (code %d); it may already be in a call from the vendor app", ret)})
			return
		}
	} else if cc != nil {
		sessionID := fmt.Sprintf("session-%s-%d", camID, time.Now().Unix())
		if err := cc.ArmTalk(r.Context(), camID, true, sessionID); err != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": fmt.Sprintf("failed to arm talk: %v", err)})
			return
		}
	} else {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": "camera not found or media session not active"})
		return
	}

	answered := true
	if viewer != nil {
		answered, _, _, _, _ = viewer.TalkDiagnostics()
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ok":              true,
		"camera":          mc.Name,
		"uuid":            mc.UUID,
		"talk_active":     true,
		"camera_answered": answered,
	})
}

// handleTalkStop handles POST /api/v1/cameras/{id}/talk/stop
func handleTalkStop(w http.ResponseWriter, r *http.Request, camID string, streamMgr *StreamManager) {
	mc, ok := streamMgr.GetCamera(camID)
	if !ok {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": "camera not found"})
		return
	}

	mc.mu.RLock()
	viewer := mc.Viewer
	mc.mu.RUnlock()

	cc := GatewayControlChannel()
	if viewer != nil {
		if _, err := viewer.SetTalk(false); err != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": fmt.Sprintf("failed to stop talk: %v", err)})
			return
		}
	} else if cc != nil {
		if err := cc.ArmTalk(r.Context(), camID, false, ""); err != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": fmt.Sprintf("failed to disarm talk: %v", err)})
			return
		}
	} else {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": "camera not found or media session not active"})
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ok":          true,
		"camera":      mc.Name,
		"uuid":        mc.UUID,
		"talk_active": false,
	})
}

// handleTalkStream handles POST /api/v1/cameras/{id}/talk/stream
func handleTalkStream(w http.ResponseWriter, r *http.Request, camID string, streamMgr *StreamManager) {
	mc, ok := streamMgr.GetCamera(camID)
	if !ok {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": "camera not found"})
		return
	}

	mc.mu.RLock()
	viewer := mc.Viewer
	streaming := mc.Streaming
	mc.mu.RUnlock()

	if !streaming || viewer == nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": "camera not found or media session not active"})
		return
	}

	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": fmt.Sprintf("read audio body: %v", err)})
		return
	}
	if len(bodyBytes) == 0 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": "empty audio stream (0 bytes)"})
		return
	}

	// Reject WebM/EBML uploads: only raw PCM and AAC are accepted
	if len(bodyBytes) >= 4 && bodyBytes[0] == 0x1A && bodyBytes[1] == 0x45 && bodyBytes[2] == 0xDF && bodyBytes[3] == 0xA3 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": "invalid audio payload: legacy WebM payload not supported"})
		return
	}

	// Uploads belong to an explicitly started session. A late upload after
	// Stop must never re-arm the speaker or revive the encoder.
	if !viewer.IsTalkActive() {
		writeJSON(w, http.StatusConflict, map[string]any{"error": "talkback is stopped; start Talk before uploading audio"})
		return
	}

	var frames [][]byte
	ct := strings.ToLower(r.Header.Get("Content-Type"))
	isPCM := strings.Contains(ct, "pcm") || strings.Contains(ct, "audio/l16") || strings.Contains(ct, "audio/raw")

	// Browser PCM (16 kHz s16le mono): one long-running encoder per talk
	// session, matched to the camera's own audio format and paced in real time.
	if isPCM || !(len(bodyBytes) >= 7 && bodyBytes[0] == 0xFF && (bodyBytes[1]&0xF0) == 0xF0) {
		if !viewer.TalkAudioRunning() {
			_ = viewer.SetTalkFormat(bridge.DefaultTalkFormatID)
		}
		if err := viewer.StartActiveTalkAudio(streamMgr.ffmpegPath); err != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": err.Error()})
			return
		}
		if err := viewer.WriteTalkPCM(bodyBytes); err != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": fmt.Sprintf("talkback encoder: %v", err)})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok":             true,
			"camera":         mc.Name,
			"uuid":           mc.UUID,
			"pcm_bytes":      len(bodyBytes),
			"encoder_active": true,
		})
		return
	}

	// Pre-encoded ADTS AAC (API clients): forwarded frame by frame.
	frames, err = bridge.ReadAllADTSFrames(bytes.NewReader(bodyBytes))
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": fmt.Sprintf("invalid audio payload: %v", err),
		})
		return
	}

	viewer.TouchAudioUp()

	totalBytes := 0
	for _, frame := range frames {
		totalBytes += len(frame)
		if err := viewer.WriteAudioFrame(frame); err != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"error": fmt.Sprintf("stream audio frame: %v", err),
			})
			return
		}
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ok":              true,
		"camera":          mc.Name,
		"uuid":            mc.UUID,
		"frames_streamed": len(frames),
		"bytes_streamed":  totalBytes,
	})
}

// transcodePCMToADTS converts raw 16kHz s16le mono PCM into 16kHz mono AAC-LC ADTS frames via FFmpeg.
func transcodePCMToADTS(ffmpegPath string, pcmData []byte) ([][]byte, error) {
	if ffmpegPath == "" {
		ffmpegPath = "ffmpeg"
	}
	cmd := exec.Command(ffmpegPath,
		"-hide_banner", "-loglevel", "error",
		"-f", "s16le", "-ar", "16000", "-ac", "1",
		"-i", "pipe:0",
		"-c:a", "aac", "-b:a", "32k", "-ar", "16000", "-ac", "1",
		"-f", "adts", "pipe:1",
	)
	hideConsole(cmd)
	cmd.Stdin = bytes.NewReader(pcmData)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("ffmpeg pcm transcode failed: %w (stderr: %s)", err, strings.TrimSpace(stderr.String()))
	}
	return bridge.ReadAllADTSFrames(&stdout)
}

// handleWHEP proxies WebRTC HTTP Egress Protocol requests between browser client and MediaMTX WebRTC server.
func handleWHEP(w http.ResponseWriter, r *http.Request, camID string, streamMgr *StreamManager) {
	mc, ok := streamMgr.GetCamera(camID)
	if !ok {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": "camera not found"})
		return
	}

	// Same-origin only: the web UI calls this with its session cookie + CSRF
	// token, so no CORS headers are offered.
	w.Header().Set("Accept-Post", "application/sdp")

	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	// ?video=only (the page's muted live view): the video-only copy, which
	// skips the audio/video alignment delay, when it is live.
	path, copyUsed := mc.UUID, "main"
	if r.Method == http.MethodPost && r.URL.Query().Get("video") == "only" {
		mc.mu.RLock()
		viewer := mc.Viewer
		mc.mu.RUnlock()
		if viewer != nil && viewer.ViewerCopyReady() {
			path, copyUsed = mc.UUID+bridge.ViewerPathSuffix, "video-only"
		}
	}
	w.Header().Set("X-BombeCam-Stream", copyUsed)
	targetURL := fmt.Sprintf("%s/%s/whep", strings.TrimRight(streamMgr.WebRTCBase(), "/"), path)

	proxyReq, err := http.NewRequestWithContext(r.Context(), r.Method, targetURL, r.Body)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": fmt.Sprintf("create whep proxy req: %v", err)})
		return
	}

	// Forward only WHEP-relevant headers; never the browser's cookies or CSRF token.
	for _, k := range []string{"Content-Type", "Accept", "If-Match"} {
		if v := r.Header.Get(k); v != "" {
			proxyReq.Header.Set(k, v)
		}
	}

	whepClient := &http.Client{Timeout: 10 * time.Second}
	resp, err := whepClient.Do(proxyReq)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": fmt.Sprintf("whep upstream error: %v", err)})
		return
	}
	defer resp.Body.Close()

	for _, k := range []string{"Content-Type", "ETag", "Link", "Accept-Patch"} {
		if v := resp.Header.Get(k); v != "" {
			w.Header().Set(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

// parseOnOff reads an on/off switch value (1/0, true/false, "on"/"off").
func parseOnOff(v any) (int, bool) {
	switch x := v.(type) {
	case nil:
		return 0, false
	case bool:
		if x {
			return 1, true
		}
		return 0, true
	case float64:
		if x != 0 {
			return 1, true
		}
		return 0, true
	case int:
		if x != 0 {
			return 1, true
		}
		return 0, true
	case string:
		switch strings.ToLower(strings.TrimSpace(x)) {
		case "on", "1", "true", "yes":
			return 1, true
		case "off", "0", "false", "no":
			return 0, true
		}
	}
	return 0, false
}
