package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Fever-r/BombeCam/pkg/auth"
	"github.com/Fever-r/BombeCam/pkg/profile"
)

func setupOperatorTokenEnv(t *testing.T) (*SessionManager, *StreamManager, profile.ProfileManager, *auth.OperatorManager, *http.ServeMux, http.Handler) {
	t.Helper()
	tmpDir := t.TempDir()
	profilePath := filepath.Join(tmpDir, "profile.enc")
	masterKey := []byte("01234567890123456789012345678901")
	pm := profile.NewManager(profilePath, masterKey)
	SetActiveProfileManager(pm)

	om := auth.NewOperatorManager(auth.OperatorConfig{
		KeyFile:      filepath.Join(tmpDir, "api_tokens.json"),
		AllowedHosts: []string{"127.0.0.1", "localhost"},
	})
	SetGatewayOperatorManager(om)

	sm := NewSessionManager("1", "test-code", testServerKey)
	streamMgr := NewStreamManager(nil, "rtsp://127.0.0.1:8554", "http://127.0.0.1:8888", nil, false, "ffmpeg")

	streamMgr.SetProfileManager(pm)
	// Same wiring as main: the manager the gateway reads from, and its pool.
	setCurrentStreamManager(streamMgr)
	ensureTestRegistry(streamMgr, sm, sm.Country(), sm.PhoneCode(), testServerKey)
	mux := SetupAPIMux(sm, streamMgr)
	handler := SecurityBoundaryHandler(mux)

	t.Cleanup(func() {
		setCurrentStreamManager(nil)
		SetActiveProfileManager(nil)
		SetGatewayOperatorManager(nil)
	})

	return sm, streamMgr, pm, om, mux, handler
}

// 1. Unauthenticated caller rejected with 401.
func TestOperatorToken_Unauthenticated_RejectedWith401(t *testing.T) {
	_, _, _, _, _, handler := setupOperatorTokenEnv(t)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/operator/token", nil)
	req.Host = "127.0.0.1:8654"
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 Unauthorized for unauthenticated caller, got %d. Body: %s", rr.Code, rr.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if resp["error"] != "unauthorized" {
		t.Fatalf("expected error='unauthorized', got %v", resp["error"])
	}
}

// 2. Stranger visitor session rejected with 401.
func TestOperatorToken_StrangerVisitorSession_RejectedWith401(t *testing.T) {
	_, _, _, _, _, handler := setupOperatorTokenEnv(t)

	// Step 1: Obtain a visitor session and CSRF token via GET /api/v1/auth/csrf
	csrfReq := httptest.NewRequest(http.MethodGet, "/api/v1/auth/csrf", nil)
	csrfReq.Host = "127.0.0.1:8654"
	csrfRR := httptest.NewRecorder()
	handler.ServeHTTP(csrfRR, csrfReq)

	if csrfRR.Code != http.StatusOK {
		t.Fatalf("expected 200 OK for CSRF endpoint, got %d", csrfRR.Code)
	}
	var csrfResp map[string]any
	_ = json.Unmarshal(csrfRR.Body.Bytes(), &csrfResp)
	csrfToken, _ := csrfResp["csrf_token"].(string)

	var sessionCookie *http.Cookie
	for _, c := range csrfRR.Result().Cookies() {
		if c.Name == auth.SessionCookieName {
			sessionCookie = c
			break
		}
	}
	if sessionCookie == nil {
		t.Fatal("expected session cookie from csrf endpoint")
	}

	// Step 2: Call POST /api/v1/operator/token with visitor cookie and valid CSRF token
	tokenReq := httptest.NewRequest(http.MethodPost, "/api/v1/operator/token", nil)
	tokenReq.Host = "127.0.0.1:8654"
	tokenReq.AddCookie(sessionCookie)
	tokenReq.Header.Set(auth.CSRFHeaderName, csrfToken)
	tokenRR := httptest.NewRecorder()
	handler.ServeHTTP(tokenRR, tokenReq)

	if tokenRR.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 Unauthorized for stranger visitor session, got %d. Body: %s", tokenRR.Code, tokenRR.Body.String())
	}
}

// 3. Logged-in vendor state (sessionMgr.IsAuthenticated() == true) does NOT allow stranger session; rejected with 401.
func TestOperatorToken_LoggedInVendorState_DoesNotAllowStranger(t *testing.T) {
	sm, streamMgr, _, _, _, handler := setupOperatorTokenEnv(t)

	// Simulate gateway owner is already logged in to vendor cloud
	sm.SetStateForTest(SessionStatusAuthenticated, "owner@example.com", "vendor-uid-12345", "", nil)
	if !streamMgr.Registry().AnyAuthenticated() {
		t.Fatal("expected a signed-in Osaio login")
	}

	// 1. Completely anonymous caller
	reqAnon := httptest.NewRequest(http.MethodPost, "/api/v1/operator/token", nil)
	reqAnon.Host = "127.0.0.1:8654"
	rrAnon := httptest.NewRecorder()
	handler.ServeHTTP(rrAnon, reqAnon)

	if rrAnon.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for anonymous caller even when vendor state is authenticated, got %d. Body: %s", rrAnon.Code, rrAnon.Body.String())
	}

	// 2. Stranger visitor session with CSRF token
	csrfReq := httptest.NewRequest(http.MethodGet, "/api/v1/auth/csrf", nil)
	csrfReq.Host = "127.0.0.1:8654"
	csrfRR := httptest.NewRecorder()
	handler.ServeHTTP(csrfRR, csrfReq)

	var csrfResp map[string]any
	_ = json.Unmarshal(csrfRR.Body.Bytes(), &csrfResp)
	csrfToken, _ := csrfResp["csrf_token"].(string)

	var sessionCookie *http.Cookie
	for _, c := range csrfRR.Result().Cookies() {
		if c.Name == auth.SessionCookieName {
			sessionCookie = c
			break
		}
	}
	if sessionCookie == nil {
		t.Fatal("expected session cookie from csrf endpoint")
	}

	reqStranger := httptest.NewRequest(http.MethodPost, "/api/v1/operator/token", nil)
	reqStranger.Host = "127.0.0.1:8654"
	reqStranger.AddCookie(sessionCookie)
	reqStranger.Header.Set(auth.CSRFHeaderName, csrfToken)
	rrStranger := httptest.NewRecorder()
	handler.ServeHTTP(rrStranger, reqStranger)

	if rrStranger.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for stranger visitor session even when vendor state is authenticated, got %d. Body: %s", rrStranger.Code, rrStranger.Body.String())
	}
}

// 4. Verified operator session with valid CSRF succeeds with 200 and token prefixed with bc_tok_.
func TestOperatorToken_VerifiedOperatorSession_Success(t *testing.T) {
	_, _, _, om, _, handler := setupOperatorTokenEnv(t)

	// Create an elevated operator session
	sess, err := om.CreateSession()
	if err != nil {
		t.Fatalf("failed to create operator session: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/operator/token", nil)
	req.Host = "127.0.0.1:8654"
	req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: sess.Token})
	req.Header.Set(auth.CSRFHeaderName, sess.CSRFToken)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 OK for verified operator session, got %d. Body: %s", rr.Code, rr.Body.String())
	}

	var resp struct {
		OK    bool   `json:"ok"`
		Token string `json:"token"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse token response: %v", err)
	}
	if !resp.OK {
		t.Fatal("expected ok=true")
	}
	if !strings.HasPrefix(resp.Token, "bc_tok_") {
		t.Fatalf("expected token to start with 'bc_tok_', got %q", resp.Token)
	}
	if !om.VerifyAPIToken(resp.Token) {
		t.Fatal("expected newly minted token to be verified by OperatorManager")
	}
}

// 5. Verified operator session missing CSRF returns 403.
func TestOperatorToken_VerifiedOperatorSession_MissingCSRF_Forbidden(t *testing.T) {
	_, _, _, om, _, handler := setupOperatorTokenEnv(t)

	sess, err := om.CreateSession()
	if err != nil {
		t.Fatalf("failed to create operator session: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/operator/token", nil)
	req.Host = "127.0.0.1:8654"
	req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: sess.Token})
	// Intentionally missing CSRF header
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403 Forbidden for operator session missing CSRF, got %d. Body: %s", rr.Code, rr.Body.String())
	}
}

// 6. Bootstrap credentials (JSON) matching saved profile returns 200.
func TestOperatorToken_BootstrapCredentials_JSON_Success(t *testing.T) {
	_, _, pm, om, _, handler := setupOperatorTokenEnv(t)

	// Persist profile with saved credentials
	prof := &profile.Profile{
		Version: profile.CurrentSchemaVersion,
		Credentials: profile.CloudCredentials{
			AccountEmail: "operator@local.lan",
			Password:     profile.SecretString("OsaioPassword-not-BombeCam"),
		},
	}
	if err := pm.Save(context.Background(), prof); err != nil {
		t.Fatalf("failed to save profile: %v", err)
	}
	seedAdmin(t, pm, "operator", "CorrectBootstrapPass123!", false)

	// An Osaio login's password never signs anyone in to BombeCam.
	osaioReq := httptest.NewRequest(http.MethodPost, "/api/v1/operator/token", strings.NewReader(`{"account_email": "operator@local.lan", "password": "OsaioPassword-not-BombeCam"}`))
	osaioReq.Host = "127.0.0.1:8654"
	osaioRR := httptest.NewRecorder()
	handler.ServeHTTP(osaioRR, osaioReq)
	if osaioRR.Code != http.StatusUnauthorized {
		t.Fatalf("an Osaio password must not grant access, got %d", osaioRR.Code)
	}
	operatorSignIns.succeeded("192.0.2.1")

	// Create visitor session cookie
	visSess, err := om.CreateVisitorSession()
	if err != nil {
		t.Fatalf("failed to create visitor session: %v", err)
	}
	if visSess.IsOperator {
		t.Fatal("expected visitor session IsOperator to be false")
	}

	body := `{"name": "operator", "password": "CorrectBootstrapPass123!"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/operator/token", strings.NewReader(body))
	req.Host = "127.0.0.1:8654"
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: visSess.Token})
	req.Header.Set(auth.CSRFHeaderName, visSess.CSRFToken)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 OK for valid bootstrap credentials (JSON), got %d. Body: %s", rr.Code, rr.Body.String())
	}

	var resp struct {
		OK    bool   `json:"ok"`
		Token string `json:"token"`
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &resp)
	if !resp.OK || !strings.HasPrefix(resp.Token, "bc_tok_") {
		t.Fatalf("invalid token response: %+v", resp)
	}

	// Verify visitor session was elevated
	elevSess, ok := om.VerifySession(visSess.Token)
	if !ok || !elevSess.IsOperator {
		t.Fatal("expected visitor session cookie to be elevated to operator")
	}
}

// 7. Bootstrap credentials (Basic Auth) matching saved profile returns 200.
func TestOperatorToken_BootstrapCredentials_BasicAuth_Success(t *testing.T) {
	_, _, pm, _, _, handler := setupOperatorTokenEnv(t)

	seedAdmin(t, pm, "admin", "BasicAuthPassword999!", false)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/operator/token", nil)
	req.Host = "127.0.0.1:8654"
	req.SetBasicAuth("admin", "BasicAuthPassword999!")
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 OK for valid Basic Auth bootstrap credentials, got %d. Body: %s", rr.Code, rr.Body.String())
	}

	var resp struct {
		OK    bool   `json:"ok"`
		Token string `json:"token"`
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &resp)
	if !resp.OK || !strings.HasPrefix(resp.Token, "bc_tok_") {
		t.Fatalf("invalid token response: %+v", resp)
	}
}

// 8. Bootstrap credentials with invalid password returns 401.
func TestOperatorToken_BootstrapCredentials_InvalidPassword_RejectedWith401(t *testing.T) {
	_, _, pm, _, _, handler := setupOperatorTokenEnv(t)

	prof := &profile.Profile{
		Version: profile.CurrentSchemaVersion,
		Credentials: profile.CloudCredentials{
			AccountEmail: "admin@local.lan",
			Password:     profile.SecretString("RealSecretPassword!"),
		},
	}
	if err := pm.Save(context.Background(), prof); err != nil {
		t.Fatalf("failed to save profile: %v", err)
	}

	// 1. JSON with wrong password
	body := `{"account_email": "admin@local.lan", "password": "WrongPassword!"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/operator/token", strings.NewReader(body))
	req.Host = "127.0.0.1:8654"
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 Unauthorized for wrong password in JSON, got %d. Body: %s", rr.Code, rr.Body.String())
	}

	// 2. Basic Auth with wrong password
	reqBasic := httptest.NewRequest(http.MethodPost, "/api/v1/operator/token", nil)
	reqBasic.Host = "127.0.0.1:8654"
	reqBasic.SetBasicAuth("admin@local.lan", "WrongPassword!")
	rrBasic := httptest.NewRecorder()
	handler.ServeHTTP(rrBasic, reqBasic)

	if rrBasic.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 Unauthorized for wrong password in Basic Auth, got %d. Body: %s", rrBasic.Code, rrBasic.Body.String())
	}
}

// 9. GET method returns 405.
func TestOperatorToken_MethodNotAllowed(t *testing.T) {
	_, _, _, _, _, handler := setupOperatorTokenEnv(t)

	methods := []string{http.MethodGet, http.MethodPut, http.MethodDelete, http.MethodPatch}
	for _, m := range methods {
		req := httptest.NewRequest(m, "/api/v1/operator/token", nil)
		req.Host = "127.0.0.1:8654"
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)

		if rr.Code != http.StatusMethodNotAllowed {
			t.Errorf("[%s] expected 405 Method Not Allowed, got %d", m, rr.Code)
		}
	}
}

// 10. Foreign origin returns 403.
func TestOperatorToken_ForeignOrigin_Forbidden(t *testing.T) {
	_, _, _, om, _, handler := setupOperatorTokenEnv(t)

	sess, err := om.CreateSession()
	if err != nil {
		t.Fatalf("failed to create operator session: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/operator/token", nil)
	req.Host = "127.0.0.1:8654"
	req.Header.Set("Origin", "https://untrusted-attacker.com")
	req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: sess.Token})
	req.Header.Set(auth.CSRFHeaderName, sess.CSRFToken)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403 Forbidden for foreign origin, got %d. Body: %s", rr.Code, rr.Body.String())
	}
}

// 11. Existing Bearer API token succeeds with 200.
func TestOperatorToken_ExistingBearerToken_Success(t *testing.T) {
	_, _, _, om, _, handler := setupOperatorTokenEnv(t)

	existingTok, err := om.GenerateAPIToken()
	if err != nil {
		t.Fatalf("failed to generate initial token: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/operator/token", nil)
	req.Host = "127.0.0.1:8654"
	req.Header.Set("Authorization", "Bearer "+existingTok)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 OK for caller with valid Bearer API token, got %d. Body: %s", rr.Code, rr.Body.String())
	}

	var resp struct {
		OK    bool   `json:"ok"`
		Token string `json:"token"`
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &resp)
	if !resp.OK || !strings.HasPrefix(resp.Token, "bc_tok_") {
		t.Fatalf("unexpected token response: %+v", resp)
	}
}

// 12. Host validation failure returns 403.
func TestOperatorToken_HostValidation_Forbidden(t *testing.T) {
	_, _, _, _, _, handler := setupOperatorTokenEnv(t)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/operator/token", nil)
	req.Host = "evil-rebind.attacker.com"
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403 Forbidden for evil host header, got %d. Body: %s", rr.Code, rr.Body.String())
	}
}
