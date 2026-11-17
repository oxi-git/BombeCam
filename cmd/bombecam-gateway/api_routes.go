package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Fever-r/BombeCam/internal/ui"
	"github.com/Fever-r/BombeCam/pkg/auth"
	"github.com/Fever-r/BombeCam/pkg/bridge"
)

// SetupAPIMux registers both the modern REST API (/api/v1/...) and legacy endpoints.
func SetupAPIMux(sessionMgr *SessionManager, streamMgr *StreamManager) *http.ServeMux {
	mux := http.NewServeMux()

	registerOperatorRoutes(mux, sessionMgr, streamMgr)
	registerOnboardingRoutes(mux, sessionMgr, streamMgr)
	registerPrivacyRoutes(mux, streamMgr)
	registerIntegrationRoutes(mux, streamMgr)
	registerRecordingRoutes(mux, streamMgr)
	registerCameraRoutes(mux, sessionMgr, streamMgr)
	registerLegacyRoutes(mux, streamMgr)

	mux.Handle("/", ui.Handler())
	return mux
}

// registerOperatorRoutes registers the operator session, CSRF and API token
// endpoints. These authenticate the administrator, so they check the caller
// themselves instead of going through the security boundary handler.
func registerOperatorRoutes(mux *http.ServeMux, sessionMgr *SessionManager, streamMgr *StreamManager) {
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
		if om.RejectForbiddenHost(w, r) {
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
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":         true,
			"csrf_token": sess.CSRFToken,
		})
	}
	mux.HandleFunc("/api/v1/auth/csrf", csrfHandler)
	mux.HandleFunc("/api/v1/operator/csrf", csrfHandler)

	tokenHandler := func(w http.ResponseWriter, r *http.Request) {
		handleOperatorToken(w, r, streamMgr)
	}
	mux.HandleFunc("/api/v1/operator/token", tokenHandler)
	registerAdminRoutes(mux, streamMgr)
	mux.HandleFunc("/api/v1/admin/start-over", func(w http.ResponseWriter, r *http.Request) {
		handleAdminStartOver(w, r, sessionMgr, streamMgr)
	})

	mux.HandleFunc("/api/v1/session/status", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "GET required", http.StatusMethodNotAllowed)
			return
		}
		writeJSON(w, http.StatusOK, streamMgr.Registry().Status())
	})
}

// handleOperatorToken exchanges verified operator credentials for an API token.
// The administrator's name and password (and two-step code, if on) are the only
// credentials that grant access to BombeCam; an Osaio login never does.
func handleOperatorToken(w http.ResponseWriter, r *http.Request, streamMgr *StreamManager) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	om := GatewayOperatorManager()
	if om == nil {
		om = auth.NewOperatorManager(auth.OperatorConfig{})
		SetGatewayOperatorManager(om)
	}

	// Host and Origin validation (DNS rebinding and cross-origin defense).
	if om.RejectForbiddenHost(w, r) {
		return
	}
	if origin := r.Header.Get("Origin"); origin != "" && !om.ValidateRequestOrigin(r) {
		writeJSON(w, http.StatusForbidden, map[string]any{
			"error":   "forbidden_origin",
			"message": "Origin rejected by cross-origin security boundary",
		})
		return
	}

	callerAuthorized := false

	// An existing API token or an operator session token.
	if tok, ok := bearerToken(r); ok {
		if om.VerifyAPIToken(tok) {
			callerAuthorized = true
		} else if s, ok := om.VerifySession(tok); ok && s != nil && s.IsOperator {
			callerAuthorized = true
		}
	}

	cookie, cErr := r.Cookie(auth.SessionCookieName)
	var callerSess *auth.Session
	if cErr == nil && cookie != nil && cookie.Value != "" {
		callerSess, _ = om.VerifySession(cookie.Value)
	}

	if !callerAuthorized && callerSess != nil && callerSess.IsOperator {
		// A cookie-based operator session needs the CSRF token too.
		csrf := r.Header.Get(auth.CSRFHeaderName)
		if csrf == "" || !om.VerifyCSRFToken(callerSess.Token, csrf) {
			writeJSON(w, http.StatusForbidden, map[string]any{
				"error":   "invalid_csrf_token",
				"message": "Missing or invalid CSRF token on state-mutating request",
			})
			return
		}
		callerAuthorized = true
	}

	addr := requestAddress(r)
	if !callerAuthorized && operatorSignIns.blocked(addr) {
		w.Header().Set("Retry-After", strconv.Itoa(int(signInWindow/time.Second)))
		writeJSON(w, http.StatusTooManyRequests, map[string]any{
			"error":   "too_many_attempts",
			"message": "Too many wrong passwords from this address. Try again in 10 minutes.",
		})
		return
	}
	if !callerAuthorized {
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
			if pm := streamMgr.ProfileManager(); pm != nil && pm.GetProfile() != nil {
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
		writeJSON(w, http.StatusUnauthorized, map[string]any{
			"error":   "unauthorized",
			"message": "Verified caller session or local bootstrap credentials required",
		})
		return
	}

	// A visitor session that just proved the bootstrap credentials becomes an
	// operator session.
	if callerSess != nil && !callerSess.IsOperator {
		om.ElevateSession(callerSess.Token)
	}

	tok, err := om.GenerateAPIToken()
	if err != nil {
		http.Error(w, "failed to generate token", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":    true,
		"token": tok,
	})
}

func bearerToken(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	suffix, ok := strings.CutPrefix(h, "Bearer ")
	if !ok {
		return "", false
	}
	tok := strings.TrimSpace(suffix)
	return tok, tok != ""
}

// registerOnboardingRoutes registers first-run setup, the Osaio logins the
// gateway uses to find cameras, and the add/remove camera lifecycle.
func registerOnboardingRoutes(mux *http.ServeMux, sessionMgr *SessionManager, streamMgr *StreamManager) {
	mux.HandleFunc("/api/v1/onboarding/status", func(w http.ResponseWriter, r *http.Request) {
		handleOnboardingStatus(w, r, sessionMgr, streamMgr)
	})

	// Osaio app ID and server key: status (never the value) and saving a new one.
	mux.HandleFunc("/api/v1/app-id", func(w http.ResponseWriter, r *http.Request) {
		handleAppID(w, r, sessionMgr, streamMgr)
	})
	mux.HandleFunc("/api/v1/server-key", func(w http.ResponseWriter, r *http.Request) {
		handleServerKey(w, r, sessionMgr, streamMgr)
	})

	mux.HandleFunc("/api/v1/onboarding/setup", func(w http.ResponseWriter, r *http.Request) {
		handleOnboardingSetup(w, r, sessionMgr, streamMgr)
	})

	mux.HandleFunc("/api/v1/onboarding/profile", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			handleOnboardingProfile(w, r, streamMgr)
		case http.MethodDelete:
			handleOnboardingReset(w, r, sessionMgr, streamMgr)
		default:
			http.Error(w, "GET or DELETE required", http.StatusMethodNotAllowed)
		}
	})

	mux.HandleFunc("/api/v1/onboarding/reset", func(w http.ResponseWriter, r *http.Request) {
		handleOnboardingReset(w, r, sessionMgr, streamMgr)
	})

	// Onboarding aliases.
	mux.HandleFunc("/api/v1/setup", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			handleOnboardingStatus(w, r, sessionMgr, streamMgr)
		case http.MethodPost:
			handleOnboardingSetup(w, r, sessionMgr, streamMgr)
		default:
			http.Error(w, "GET or POST required", http.StatusMethodNotAllowed)
		}
	})

	discoverHandler := func(w http.ResponseWriter, r *http.Request) {
		handleCameraDiscover(w, r, sessionMgr, streamMgr)
	}
	mux.HandleFunc("/api/v1/onboarding/cameras/discover", discoverHandler)
	mux.HandleFunc("/api/v1/inventory/discover", discoverHandler)
	mux.HandleFunc("/api/v1/devices/discover", discoverHandler)

	enrollHandler := func(w http.ResponseWriter, r *http.Request) {
		handleCameraEnroll(w, r, sessionMgr, streamMgr)
	}
	mux.HandleFunc("/api/v1/onboarding/cameras/enroll", enrollHandler)
	mux.HandleFunc("/api/v1/inventory/enroll", enrollHandler)
	mux.HandleFunc("/api/v1/devices/enroll", enrollHandler)

	// Add-cameras lifecycle: reusable per-login search, additive add, and
	// per-camera remove. These coexist with the legacy discover/enroll routes.
	mux.HandleFunc("/api/v1/cameras/search", func(w http.ResponseWriter, r *http.Request) {
		handleCameraSearch(w, r, streamMgr)
	})
	mux.HandleFunc("/api/v1/cameras/add", func(w http.ResponseWriter, r *http.Request) {
		handleCameraAdd(w, r, streamMgr)
	})

	// Osaio logins: the pool of logins used to find and reach cameras.
	mux.HandleFunc("/api/v1/accounts", func(w http.ResponseWriter, r *http.Request) {
		handleAccountsList(w, r, streamMgr)
	})
	mux.HandleFunc("/api/v1/accounts/signin", func(w http.ResponseWriter, r *http.Request) {
		handleAccountSignIn(w, r, sessionMgr, streamMgr)
	})
	mux.HandleFunc("/api/v1/accounts/cameras", func(w http.ResponseWriter, r *http.Request) {
		handleAccountCameras(w, r, streamMgr)
	})
	mux.HandleFunc("/api/v1/accounts/remove", func(w http.ResponseWriter, r *http.Request) {
		handleAccountRemove(w, r, sessionMgr, streamMgr)
	})

	// Delete all stored information (logins and cameras); returns to first-run.
	mux.HandleFunc("/api/v1/profile/forget", func(w http.ResponseWriter, r *http.Request) {
		handleOnboardingReset(w, r, sessionMgr, streamMgr)
	})
}

// registerPrivacyRoutes registers the "block cloud video?" router allowlist,
// the gateway start/stop controls and the favicon.
func registerPrivacyRoutes(mux *http.ServeMux, streamMgr *StreamManager) {
	mux.HandleFunc("/api/v1/control/status", func(w http.ResponseWriter, r *http.Request) {
		handleControlStatus(w, r, streamMgr)
	})
	mux.HandleFunc("/api/v1/privacy", func(w http.ResponseWriter, r *http.Request) {
		handlePrivacyStatus(w, r, streamMgr)
	})
	mux.HandleFunc("/api/v1/privacy/apply", func(w http.ResponseWriter, r *http.Request) {
		handlePrivacyApply(w, r, streamMgr)
	})
	mux.HandleFunc("/api/v1/privacy/camera", func(w http.ResponseWriter, r *http.Request) {
		handlePrivacyCamera(w, r, streamMgr)
	})
	mux.HandleFunc("/api/v1/privacy/block-all", func(w http.ResponseWriter, r *http.Request) {
		handlePrivacyBlockAll(w, r, streamMgr)
	})
	mux.HandleFunc("/api/v1/privacy/router/connect", func(w http.ResponseWriter, r *http.Request) {
		handleRouterConnect(w, r, streamMgr)
	})
	mux.HandleFunc("/api/v1/privacy/router/disconnect", func(w http.ResponseWriter, r *http.Request) {
		handleRouterDisconnect(w, r, streamMgr)
	})
	mux.HandleFunc("/api/v1/privacy/router/check", func(w http.ResponseWriter, r *http.Request) {
		handleRouterCheck(w, r, streamMgr)
	})
	mux.HandleFunc("/api/v1/privacy/check", func(w http.ResponseWriter, r *http.Request) {
		handlePrivacyCheck(w, r, streamMgr)
	})
	mux.HandleFunc("/api/v1/privacy/backend", func(w http.ResponseWriter, r *http.Request) {
		handlePrivacyBackend(w, r, streamMgr)
	})
	mux.HandleFunc("/api/v1/privacy/region", func(w http.ResponseWriter, r *http.Request) {
		handlePrivacyRegion(w, r, streamMgr)
	})
	mux.HandleFunc("/api/v1/privacy/forget-router", func(w http.ResponseWriter, r *http.Request) {
		handlePrivacyForgetRouter(w, r, streamMgr)
	})
	mux.HandleFunc("/api/v1/privacy/router-script", handlePrivacyRouterScript)

	mux.HandleFunc("/api/v1/gateway/start", func(w http.ResponseWriter, r *http.Request) {
		handleGatewayStart(w, r, streamMgr)
	})
	mux.HandleFunc("/api/v1/gateway/stop", func(w http.ResponseWriter, r *http.Request) {
		handleGatewayStop(w, r, streamMgr)
	})
	mux.HandleFunc("/api/v1/gateway/shutdown", func(w http.ResponseWriter, r *http.Request) { handleGatewayShutdown(w, r, streamMgr) })
	mux.HandleFunc("/api/v1/gateway/info", handleGatewayInfo)
	mux.HandleFunc("/api/v1/gateway/autostart", func(w http.ResponseWriter, r *http.Request) { handleAutostart(w, r, streamMgr) })
	mux.HandleFunc("/favicon.ico", handleFavicon)
}

// registerIntegrationRoutes registers the third-party integrations (Frigate,
// Home Assistant) and the settings that point them at this gateway.
func registerIntegrationRoutes(mux *http.ServeMux, streamMgr *StreamManager) {
	mux.HandleFunc("/api/v1/integrations/frigate", func(w http.ResponseWriter, r *http.Request) {
		handleFrigateIntegration(w, r, streamMgr)
	})
	mux.HandleFunc("/api/v1/integrations/homeassistant", func(w http.ResponseWriter, r *http.Request) {
		handleHomeAssistantIntegration(w, r, streamMgr)
	})
	mux.HandleFunc("/api/v1/integrations/streams", func(w http.ResponseWriter, r *http.Request) {
		handleStreamsIntegration(w, r, streamMgr)
	})
	mux.HandleFunc("/api/v1/integrations/settings", func(w http.ResponseWriter, r *http.Request) {
		handleIntegrationSettings(w, r, streamMgr)
	})
}

// registerRecordingRoutes registers the local recording browser and download.
func registerRecordingRoutes(mux *http.ServeMux, streamMgr *StreamManager) {
	mux.HandleFunc("/api/v1/recordings", func(w http.ResponseWriter, r *http.Request) {
		handleRecordings(w, r, streamMgr)
	})
	mux.HandleFunc("/api/v1/recordings/download", func(w http.ResponseWriter, r *http.Request) {
		handleRecordingDownload(w, r, streamMgr)
	})
}

// registerCameraRoutes registers the RESTful per-camera routes.
//
// The camera id is the first path segment; everything else is matched here.
// A path that matches no route falls through to http.NotFound.
func registerCameraRoutes(mux *http.ServeMux, sessionMgr *SessionManager, streamMgr *StreamManager) {
	mux.HandleFunc("/api/v1/cameras/", func(w http.ResponseWriter, r *http.Request) {
		relPath := strings.TrimPrefix(r.URL.Path, "/api/v1/cameras/")
		parts := strings.Split(relPath, "/")
		if len(parts) == 0 || parts[0] == "" {
			http.NotFound(w, r)
			return
		}
		camID := parts[0]
		handleCameraRoute(w, r, camID, parts, sessionMgr, streamMgr)
	})
}

// handleCameraRoute dispatches one /api/v1/cameras/{id}/... request. parts is
// the path split on "/" with parts[0] the camera id.
func handleCameraRoute(w http.ResponseWriter, r *http.Request, camID string, parts []string, sessionMgr *SessionManager, streamMgr *StreamManager) {
	get, post := r.Method == http.MethodGet, r.Method == http.MethodPost

	switch {
	case len(parts) == 1 && r.Method == http.MethodDelete:
		handleCameraRemove(w, r, camID, streamMgr)

	case len(parts) == 2 && parts[1] == "status" && get:
		mc, ok := streamMgr.GetCamera(camID)
		if !ok {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "camera not found"})
			return
		}
		writeJSON(w, http.StatusOK, mc.GetStatus(GatewayControlChannel(), r))

	case len(parts) == 2 && parts[1] == "shadow" && get:
		handleCameraShadow(w, r, camID, streamMgr)

	case len(parts) == 2 && parts[1] == "control" && post:
		handleCameraControl(w, r, camID, streamMgr)

	case len(parts) == 2 && parts[1] == "ptz" && post:
		handleDedicatedPTZ(w, r, camID, streamMgr)

	case len(parts) == 2 && parts[1] == "stream" && get:
		handleCameraStream(w, r, camID, streamMgr)

	case len(parts) == 2 && parts[1] == "unlock" && post:
		handleCameraUnlock(w, r, camID, streamMgr)

	case len(parts) == 2 && parts[1] == "mode" && post:
		handleCameraMode(w, r, camID, streamMgr)

	case len(parts) == 3 && parts[1] == "talk" && parts[2] == "start" && post:
		handleTalkStart(w, r, camID, streamMgr)

	case len(parts) == 3 && parts[1] == "talk" && parts[2] == "stop" && post:
		handleTalkStop(w, r, camID, streamMgr)

	case len(parts) == 3 && parts[1] == "talk" && parts[2] == "stream" && post:
		handleTalkStream(w, r, camID, streamMgr)

	case len(parts) == 2 && parts[1] == "whep":
		handleWHEP(w, r, camID, streamMgr)

	case len(parts) == 2 && parts[1] == "snapshot.jpg":
		handleSnapshot(w, r, streamMgr, camID)

	case len(parts) == 2 && parts[1] == "stream-name" && post:
		handleCameraStreamName(w, r, camID, streamMgr)

	case len(parts) >= 3 && parts[1] == "hls" && (get || r.Method == http.MethodHead):
		handleHLSRelay(w, r, camID, strings.Join(parts[2:], "/"), streamMgr)

	case len(parts) == 2 && parts[1] == "sdcard" && get:
		handleSDCardInfo(w, r, camID, streamMgr)

	case len(parts) == 3 && parts[1] == "sdcard" && parts[2] == "recordings" && get:
		handleSDCardRecordings(w, r, camID, streamMgr)

	case len(parts) == 3 && parts[1] == "sdcard" && parts[2] == "videos" && get:
		handleSDCardVideos(w, r, camID, streamMgr)

	case len(parts) == 3 && parts[1] == "cloud" && parts[2] == "days" && get:
		handleCameraCloudDays(w, r, camID, r.URL.Query().Get("month"), streamMgr)

	case len(parts) == 3 && parts[1] == "cloud" && parts[2] == "playback" && post:
		handleCameraCloudPlayback(w, r, camID, streamMgr)

	case len(parts) == 3 && parts[1] == "sdcard" && parts[2] == "playback" && post:
		handleSDCardPlayback(w, r, camID, streamMgr)

	default:
		http.NotFound(w, r)
	}
}

func handleCameraUnlock(w http.ResponseWriter, r *http.Request, camID string, streamMgr *StreamManager) {
	mc, err := streamMgr.Unlock(camID)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "camera not found"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":           "unlocked",
		"camera":           mc.Name,
		"uuid":             mc.UUID,
		"remaining_budget": 3,
		"ceiling_seconds":  30,
	})
}

func handleCameraMode(w http.ResponseWriter, r *http.Request, camID string, streamMgr *StreamManager) {
	var body struct {
		StrictManual bool `json:"strict_manual"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid json payload"})
		return
	}
	if err := streamMgr.SetStrictManual(camID, body.StrictManual); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "camera not found"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":            true,
		"strict_manual": body.StrictManual,
	})
}

// registerLegacyRoutes registers the pre-/api/v1 endpoints the web page and
// integrations still call. Each takes ?cam= (a UUID or a camera name).
func registerLegacyRoutes(mux *http.ServeMux, streamMgr *StreamManager) {
	mux.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		handleLegacyStatus(w, r, streamMgr)
	})
	mux.HandleFunc("/camera/unlock", func(w http.ResponseWriter, r *http.Request) {
		handleLegacyUnlock(w, r, streamMgr)
	})
	mux.HandleFunc("/camera/mode", func(w http.ResponseWriter, r *http.Request) {
		handleLegacyMode(w, r, streamMgr)
	})
	mux.HandleFunc("/ptz", func(w http.ResponseWriter, r *http.Request) {
		handleLegacyPTZ(w, r, streamMgr)
	})
	mux.HandleFunc("/ir", func(w http.ResponseWriter, r *http.Request) {
		handleLegacyIR(w, r, streamMgr)
	})
	mux.HandleFunc("/ir/status", gated(authOperatorOnly, func(w http.ResponseWriter, r *http.Request) {
		handleLegacyIRStatus(w, r, streamMgr)
	}))
	mux.HandleFunc("/led", func(w http.ResponseWriter, r *http.Request) {
		handleLegacyLED(w, r, streamMgr)
	})
	mux.HandleFunc("/led/status", gated(authOperatorOnly, func(w http.ResponseWriter, r *http.Request) {
		handleLegacyLEDStatus(w, r, streamMgr)
	}))
	mux.HandleFunc("/light", func(w http.ResponseWriter, r *http.Request) {
		handleLegacyLight(w, r, streamMgr)
	})
	mux.HandleFunc("/mic", func(w http.ResponseWriter, r *http.Request) {
		handleLegacyMic(w, r, streamMgr)
	})
	mux.HandleFunc("/cmd", func(w http.ResponseWriter, r *http.Request) {
		handleLegacyCommand(w, r)
	})
	mux.HandleFunc("/talk/start", func(w http.ResponseWriter, r *http.Request) {
		handleLegacyTalkStart(w, r, streamMgr)
	})
	mux.HandleFunc("/talk/stop", func(w http.ResponseWriter, r *http.Request) {
		handleLegacyTalkStop(w, r, streamMgr)
	})
	mux.HandleFunc("/talk/stream", func(w http.ResponseWriter, r *http.Request) {
		handleLegacyTalkStream(w, r)
	})
}

// allowLegacyPost requires POST on a legacy route, reporting whether the
// handler should continue. Who may call it is the route's auth level, which
// the middleware has already enforced by the time this runs.
func allowLegacyPost(w http.ResponseWriter, r *http.Request) bool {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return false
	}
	return true
}

// handleLegacyStatus lists every enrolled camera with its stream state, so a
// camera in LOCKOUT stays visible.
func handleLegacyStatus(w http.ResponseWriter, r *http.Request, streamMgr *StreamManager) {
	cameras := []map[string]any{}
	for _, mc := range streamMgr.GetAllCameras() {
		mc.mu.RLock()
		sess := mc.Session
		camInfo := map[string]any{
			"name":      mc.Name,
			"uuid":      mc.UUID,
			"model":     mc.Model,
			"streaming": mc.Streaming,
		}
		mc.mu.RUnlock()

		if sess != nil {
			sess.mu.Lock()
			camInfo["state"] = string(sess.State)
			camInfo["recovery_budget"] = sess.RecoveryBudget
			camInfo["strict_manual"] = sess.StrictManual
			sess.mu.Unlock()
		}
		cameras = append(cameras, camInfo)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":  "ok",
		"cameras": cameras,
	})
}

func handleLegacyUnlock(w http.ResponseWriter, r *http.Request, streamMgr *StreamManager) {
	if !allowLegacyPost(w, r) {
		return
	}
	sess := cameraSessionByName(streamMgr, r.URL.Query().Get("cam"))
	if sess == nil {
		http.Error(w, "camera session not found", http.StatusNotFound)
		return
	}
	sess.Unlock()
	sess.mu.Lock()
	state, budget := sess.State, sess.RecoveryBudget
	sess.mu.Unlock()

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":              true,
		"camera":          sess.Name,
		"uuid":            sess.UUID,
		"state":           string(state),
		"recovery_budget": budget,
	})
}

// handleLegacyMode sets strict-manual recovery for one camera, or for every
// camera when ?cam= names none of them.
func handleLegacyMode(w http.ResponseWriter, r *http.Request, streamMgr *StreamManager) {
	if !allowLegacyPost(w, r) {
		return
	}
	cam := r.URL.Query().Get("cam")
	manualStr := r.URL.Query().Get("manual")
	manual := manualStr == "true" || manualStr == "1"

	sess := cameraSessionByName(streamMgr, cam)
	if sess == nil {
		for _, s := range streamMgr.Sessions() {
			s.SetStrictManual(manual)
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":            true,
			"camera":        cam,
			"strict_manual": manual,
		})
		return
	}
	sess.SetStrictManual(manual)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":            true,
		"camera":        sess.Name,
		"uuid":          sess.UUID,
		"strict_manual": sess.IsStrictManual(),
	})
}

// ptzDirection parses the legacy direction vocabulary: left/1, right/2,
// up/3, down/4, stop/0, or a bare number.
func ptzDirection(s string) (int, bool) {
	switch strings.ToLower(s) {
	case "left", "1":
		return 1, true
	case "right", "2":
		return 2, true
	case "up", "top", "3":
		return 3, true
	case "down", "bottom", "4":
		return 4, true
	case "stop", "0":
		return 0, true
	}
	if d, err := strconv.Atoi(s); err == nil {
		return d, true
	}
	return 0, false
}

func handleLegacyPTZ(w http.ResponseWriter, r *http.Request, streamMgr *StreamManager) {
	if !allowLegacyPost(w, r) {
		return
	}
	dir, ok := ptzDirection(r.URL.Query().Get("dir"))
	if !ok {
		http.Error(w, "invalid dir (left/1, right/2, up/3, down/4, stop/0)", http.StatusBadRequest)
		return
	}
	dur := 400
	if d, err := strconv.Atoi(r.URL.Query().Get("dur")); err == nil && d >= 0 {
		dur = d
	}

	uuid, name, cc, ok := legacyControlTarget(w, r.URL.Query().Get("cam"), streamMgr)
	if !ok {
		return
	}
	fmt.Printf("[%s] [PTZ] dispatching direction=%d (dur=%dms)...\n", name, dir, dur)
	if err := cc.MovePTZ(r.Context(), uuid, dir, dur); err != nil {
		controlFailed(w, "ptz move", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":          true,
		"camera":      name,
		"uuid":        uuid,
		"direction":   dir,
		"duration_ms": dur,
	})
}

func handleLegacyIR(w http.ResponseWriter, r *http.Request, streamMgr *StreamManager) {
	if !allowLegacyPost(w, r) {
		return
	}
	irMode, okMode := bridge.ParseIRMode(r.URL.Query().Get("mode"))
	if !okMode {
		http.Error(w, "invalid mode: use auto, on (night vision) or off", http.StatusBadRequest)
		return
	}
	uuid, name, cc, ok := legacyControlTarget(w, r.URL.Query().Get("cam"), streamMgr)
	if !ok {
		return
	}
	fmt.Printf("[%s] [IR] dispatching mode=%d\n", name, irMode)
	if err := cc.SetIR(r.Context(), uuid, irMode); err != nil {
		controlFailed(w, "ir control", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":           true,
		"camera":       name,
		"uuid":         uuid,
		"requested":    map[string]any{"mode": bridge.IRModeName(irMode), "IrLedMode": irMode, "LightSW": 0},
		"confirmation": "unconfirmed",
	})
}

func handleLegacyIRStatus(w http.ResponseWriter, r *http.Request, streamMgr *StreamManager) {
	uuid, name, cc, ok := legacyControlTarget(w, r.URL.Query().Get("cam"), streamMgr)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 1500*time.Millisecond)
	defer cancel()
	mode, err := cc.GetIR(ctx, uuid)
	if err != nil {
		controlFailed(w, "query ir status", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":        true,
		"camera":    name,
		"uuid":      uuid,
		"mode":      bridge.IRModeName(mode),
		"IrLedMode": mode,
	})
}

// legacyOnOff reads the legacy on/off vocabulary: on/1/true (and empty, which
// means on) or off/0/false.
func legacyOnOff(s string) (int, bool) {
	switch strings.ToLower(s) {
	case "off", "0", "false":
		return 0, true
	case "on", "1", "true", "":
		return 1, true
	}
	return 0, false
}

func handleLegacyLED(w http.ResponseWriter, r *http.Request, streamMgr *StreamManager) {
	if !allowLegacyPost(w, r) {
		return
	}
	val, ok := legacyOnOff(r.URL.Query().Get("mode"))
	if !ok {
		http.Error(w, "invalid mode: use on/1/true or off/0/false", http.StatusBadRequest)
		return
	}
	uuid, name, cc, ok := legacyControlTarget(w, r.URL.Query().Get("cam"), streamMgr)
	if !ok {
		return
	}
	fmt.Printf("[%s] [LED] dispatching on=%v\n", name, val == 1)
	if err := cc.SetLED(r.Context(), uuid, val == 1); err != nil {
		controlFailed(w, "led control", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":           true,
		"camera":       name,
		"uuid":         uuid,
		"requested":    map[string]any{"mode": onOffName(val), "LedOnOff": val},
		"confirmation": "unconfirmed",
	})
}

func handleLegacyLEDStatus(w http.ResponseWriter, r *http.Request, streamMgr *StreamManager) {
	uuid, name, cc, ok := legacyControlTarget(w, r.URL.Query().Get("cam"), streamMgr)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 1500*time.Millisecond)
	defer cancel()
	on, err := cc.GetLED(ctx, uuid)
	if err != nil {
		controlFailed(w, "query led status", err)
		return
	}
	mode := 0
	if on {
		mode = 1
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":       true,
		"camera":   name,
		"uuid":     uuid,
		"LedOnOff": mode,
		"status":   onOffName(mode),
	})
}

func onOffName(val int) string {
	if val == 1 {
		return "on"
	}
	return "off"
}

func handleLegacyLight(w http.ResponseWriter, r *http.Request, streamMgr *StreamManager) {
	if !allowLegacyPost(w, r) {
		return
	}
	val, ok := legacyOnOff(r.URL.Query().Get("mode"))
	if !ok {
		http.Error(w, "invalid mode: use on/1/true or off/0/false", http.StatusBadRequest)
		return
	}
	uuid, name, cc, ok := legacyControlTarget(w, r.URL.Query().Get("cam"), streamMgr)
	if !ok {
		return
	}
	fmt.Printf("[%s] [LIGHT] dispatching on=%v\n", name, val == 1)
	if err := cc.SetLight(r.Context(), uuid, val == 1); err != nil {
		controlFailed(w, "light control", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":           true,
		"camera":       name,
		"uuid":         uuid,
		"requested":    map[string]any{"mode": onOffName(val), "LightSW": val},
		"confirmation": "unconfirmed",
	})
}

// handleLegacyMic sets the camera's AudioRecordSw attribute.
func handleLegacyMic(w http.ResponseWriter, r *http.Request, streamMgr *StreamManager) {
	if !allowLegacyPost(w, r) {
		return
	}
	target := findSignaling(r.URL.Query().Get("cam"))
	if target == nil {
		http.Error(w, "camera not found or not connected", http.StatusServiceUnavailable)
		return
	}
	cc := cameraControl(streamMgr, target.dev.UUID)
	if cc == nil {
		http.Error(w, "camera not found or not connected", http.StatusServiceUnavailable)
		return
	}

	enable := 1
	if v, ok := legacyOnOff(r.URL.Query().Get("enable")); ok {
		enable = v
	}

	fmt.Printf("[%s] [MIC] dispatching AudioRecordSw=%d\n", target.dev.Name, enable)
	if err := cc.SetAttribute(r.Context(), target.dev.UUID, "AudioRecordSw", enable); err != nil {
		controlFailed(w, "microphone change", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":            true,
		"camera":        target.dev.Name,
		"uuid":          target.dev.UUID,
		"AudioRecordSw": enable,
	})
}

// handleLegacyCommand sends a raw vendor message for debugging. It is the one
// route that bypasses ControlChannel, on purpose: its input is already a vendor
// method name and payload.
func handleLegacyCommand(w http.ResponseWriter, r *http.Request) {
	if !allowLegacyPost(w, r) {
		return
	}
	method := r.URL.Query().Get("method")
	jsonStr := r.URL.Query().Get("json")

	if body, _ := io.ReadAll(r.Body); len(body) > 0 {
		jsonStr = string(body)
	}
	if method == "" {
		http.Error(w, "missing method parameter (e.g. atr.get, atr.set)", http.StatusBadRequest)
		return
	}

	target := findSignaling(r.URL.Query().Get("cam"))
	if target == nil || target.sig == nil {
		http.Error(w, "camera not found or not connected", http.StatusServiceUnavailable)
		return
	}

	var payload any = map[string]any{}
	if jsonStr != "" {
		if err := json.Unmarshal([]byte(jsonStr), &payload); err != nil {
			http.Error(w, fmt.Sprintf("invalid json payload: %v", err), http.StatusBadRequest)
			return
		}
	}

	fmt.Printf("[%s] [CMD] dispatching method=%s data=%+v\n", target.dev.Name, method, payload)
	if isLocalControl(GatewayControlChannel()) {
		http.Error(w, "raw vendor commands are disabled during local control testing", http.StatusConflict)
		return
	}
	if err := target.sig.Send(method, payload); err != nil {
		controlFailed(w, "send", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"camera":  target.dev.Name,
		"uuid":    target.dev.UUID,
		"method":  method,
		"payload": payload,
	})
}

// handleLegacyTalkStart arms talk on the camera named by ?cam, through the
// same armTalk the REST route uses.
func handleLegacyTalkStart(w http.ResponseWriter, r *http.Request, streamMgr *StreamManager) {
	if !allowLegacyPost(w, r) {
		return
	}
	mc, ok := streamMgr.ResolveCamera(r.URL.Query().Get("cam"))
	if !ok {
		http.Error(w, "camera not found", http.StatusNotFound)
		return
	}
	armTalk(w, r, mc, true)
}

// handleLegacyTalkStop disarms talk on the camera named by ?cam.
func handleLegacyTalkStop(w http.ResponseWriter, r *http.Request, streamMgr *StreamManager) {
	if !allowLegacyPost(w, r) {
		return
	}
	mc, ok := streamMgr.ResolveCamera(r.URL.Query().Get("cam"))
	if !ok {
		http.Error(w, "camera not found", http.StatusNotFound)
		return
	}
	armTalk(w, r, mc, false)
}

func handleLegacyTalkStream(w http.ResponseWriter, r *http.Request) {
	if !allowLegacyPost(w, r) {
		return
	}
	target := findSignaling(r.URL.Query().Get("cam"))
	if target == nil || target.viewer == nil {
		http.Error(w, "camera not found or media session not active", http.StatusServiceUnavailable)
		return
	}

	frames, err := bridge.ReadAllADTSFrames(r.Body)
	if err != nil {
		http.Error(w, fmt.Sprintf("invalid audio payload: %v", err), http.StatusBadRequest)
		return
	}
	if !target.viewer.IsTalkActive() {
		_, _ = target.viewer.SetTalk(true)
	}
	target.viewer.TouchAudioUp()

	for _, frame := range frames {
		if err := target.viewer.WriteAudioFrame(frame); err != nil {
			http.Error(w, fmt.Sprintf("stream audio frame: %v", err), http.StatusInternalServerError)
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":              true,
		"camera":          target.dev.Name,
		"uuid":            target.dev.UUID,
		"frames_streamed": len(frames),
	})
}
