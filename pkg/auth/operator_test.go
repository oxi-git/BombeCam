package auth

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestOperatorManager_HostValidation(t *testing.T) {
	om := NewOperatorManager(OperatorConfig{
		AllowedHosts: []string{"192.168.1.100"},
	})

	tests := []struct {
		host  string
		valid bool
	}{
		{"127.0.0.1", true},
		{"127.0.0.1:8654", true},
		{"localhost", true},
		{"localhost:8654", true},
		{"::1", true},
		{"[::1]:8654", true},
		{"host.docker.internal", true},
		{"host.docker.internal:8654", true},
		{"example.com", true},
		{"192.168.1.100", true},
		{"192.168.1.100:8654", true},
		{"10.0.0.5", true}, // IP literal
		{"untrusted.example", false},
		{"rebind.attacker.com", false},
		{"evil.com:8654", false},
		{"", false},
	}

	for _, tc := range tests {
		got := om.ValidateHost(tc.host)
		if got != tc.valid {
			t.Errorf("ValidateHost(%q) = %v; want %v", tc.host, got, tc.valid)
		}
	}
}

func TestOperatorManager_OriginValidation(t *testing.T) {
	om := NewOperatorManager(OperatorConfig{
		AllowedHosts: []string{"192.168.1.50"},
	})

	tests := []struct {
		origin string
		valid  bool
	}{
		{"", true}, // non-browser
		{"http://127.0.0.1:8654", true},
		{"http://localhost:8654", true},
		{"http://192.168.1.50:8654", true},
		{"https://untrusted.example", false},
		{"http://attacker.org:8080", false},
		{"null", false},
	}

	for _, tc := range tests {
		got := om.ValidateOrigin(tc.origin)
		if got != tc.valid {
			t.Errorf("ValidateOrigin(%q) = %v; want %v", tc.origin, got, tc.valid)
		}
	}
}

func TestOperatorManager_SessionLifecycle(t *testing.T) {
	om := NewOperatorManager(OperatorConfig{
		SessionTTL: 50 * time.Millisecond,
	})

	sess, err := om.CreateSession()
	if err != nil {
		t.Fatalf("CreateSession failed: %v", err)
	}
	if sess.Token == "" || sess.CSRFToken == "" {
		t.Fatal("empty tokens generated")
	}

	// Immediate verification
	verified, ok := om.VerifySession(sess.Token)
	if !ok || verified == nil {
		t.Fatal("expected session verification to succeed")
	}

	// CSRF verification
	if !om.VerifyCSRFToken(sess.Token, sess.CSRFToken) {
		t.Fatal("expected CSRF token verification to succeed")
	}
	if om.VerifyCSRFToken(sess.Token, "wrong-csrf-token") {
		t.Fatal("expected wrong CSRF token to fail")
	}

	// Cookie issuance
	rr := httptest.NewRecorder()
	om.IssueSessionCookie(rr, sess)
	cookies := rr.Result().Cookies()
	if len(cookies) == 0 {
		t.Fatal("expected session cookie to be set")
	}
	sc := cookies[0]
	if sc.Name != SessionCookieName || sc.Value != sess.Token || !sc.HttpOnly || sc.SameSite != http.SameSiteLaxMode {
		t.Fatalf("unexpected cookie attributes: %+v", sc)
	}

	// Expiration check
	time.Sleep(60 * time.Millisecond)
	_, ok = om.VerifySession(sess.Token)
	if ok {
		t.Fatal("expected session to expire")
	}
}

func TestOperatorManager_APITokenExemptionAndCSRFBoundary(t *testing.T) {
	om := NewOperatorManager(OperatorConfig{})

	// 1. Programmatic API Bearer Token
	apiToken, err := om.GenerateAPIToken()
	if err != nil {
		t.Fatalf("GenerateAPIToken failed: %v", err)
	}
	if !om.VerifyAPIToken(apiToken) {
		t.Fatal("VerifyAPIToken failed")
	}

	// Bearer request on state-mutating POST without CSRF token MUST succeed
	reqBearer := httptest.NewRequest(http.MethodPost, "/api/v1/firewall/mode", strings.NewReader(`{"privacy_mode": true}`))
	reqBearer.Header.Set("Authorization", "Bearer "+apiToken)
	rrBearer := httptest.NewRecorder()
	if !om.ValidateSecurityBoundary(rrBearer, reqBearer, true) {
		t.Fatalf("expected Bearer client to pass security boundary without CSRF: %d", rrBearer.Code)
	}

	// 2. Cookie-based Browser Session
	sess, err := om.CreateSession()
	if err != nil {
		t.Fatalf("CreateSession failed: %v", err)
	}

	// State-mutating request with Cookie but MISSING CSRF token MUST be rejected
	reqNoCSRF := httptest.NewRequest(http.MethodPost, "/api/v1/firewall/mode", strings.NewReader(`{"privacy_mode": true}`))
	reqNoCSRF.AddCookie(&http.Cookie{Name: SessionCookieName, Value: sess.Token})
	rrNoCSRF := httptest.NewRecorder()
	if om.ValidateSecurityBoundary(rrNoCSRF, reqNoCSRF, true) {
		t.Fatal("expected cookie request without CSRF token to be rejected")
	}
	if rrNoCSRF.Code != http.StatusForbidden {
		t.Fatalf("expected 403 Forbidden for missing CSRF, got %d", rrNoCSRF.Code)
	}

	// State-mutating request with Cookie and VALID CSRF token MUST pass
	reqValidCSRF := httptest.NewRequest(http.MethodPost, "/api/v1/firewall/mode", strings.NewReader(`{"privacy_mode": true}`))
	reqValidCSRF.AddCookie(&http.Cookie{Name: SessionCookieName, Value: sess.Token})
	reqValidCSRF.Header.Set(CSRFHeaderName, sess.CSRFToken)
	rrValidCSRF := httptest.NewRecorder()
	if !om.ValidateSecurityBoundary(rrValidCSRF, reqValidCSRF, true) {
		t.Fatalf("expected valid CSRF request to pass, got code %d", rrValidCSRF.Code)
	}

	// State-mutating request with Foreign Origin MUST be rejected
	reqForeignOrigin := httptest.NewRequest(http.MethodPost, "/api/v1/firewall/mode", strings.NewReader(`{"privacy_mode": true}`))
	reqForeignOrigin.Header.Set("Origin", "https://untrusted.example")
	reqForeignOrigin.AddCookie(&http.Cookie{Name: SessionCookieName, Value: sess.Token})
	reqForeignOrigin.Header.Set(CSRFHeaderName, sess.CSRFToken)
	rrForeign := httptest.NewRecorder()
	if om.ValidateSecurityBoundary(rrForeign, reqForeignOrigin, true) {
		t.Fatal("expected foreign origin request to be rejected")
	}
	if rrForeign.Code != http.StatusForbidden {
		t.Fatalf("expected 403 Forbidden for foreign origin, got %d", rrForeign.Code)
	}
}

func TestOperatorManager_APITokenPersistence(t *testing.T) {
	keyFile := filepath.Join(t.TempDir(), "api_tokens.json")
	om1 := NewOperatorManager(OperatorConfig{KeyFile: keyFile})
	tok1, _ := om1.GenerateAPIToken()

	// Recreate from file
	om2 := NewOperatorManager(OperatorConfig{KeyFile: keyFile})
	if !om2.VerifyAPIToken(tok1) {
		t.Fatalf("persisted token was not restored after reload")
	}
}

func TestOperatorManager_InvalidCredentialsAndAuthBypassDefense(t *testing.T) {
	om := NewOperatorManager(OperatorConfig{})

	// 1. Forged/invalid session cookie on mutation with requireAuth=false MUST return 401
	reqInvalidCookie := httptest.NewRequest(http.MethodPost, "/api/v1/firewall/mode", strings.NewReader(`{"privacy_mode": true}`))
	reqInvalidCookie.AddCookie(&http.Cookie{Name: SessionCookieName, Value: "forged-invalid-session-token"})
	rrInvalidCookie := httptest.NewRecorder()
	if om.ValidateSecurityBoundary(rrInvalidCookie, reqInvalidCookie, false) {
		t.Fatal("expected forged cookie to be rejected even when requireAuth=false")
	}
	if rrInvalidCookie.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 Unauthorized for forged cookie, got %d", rrInvalidCookie.Code)
	}

	// 2. Forged/invalid Bearer token MUST return 401
	reqInvalidBearer := httptest.NewRequest(http.MethodPost, "/api/v1/firewall/mode", strings.NewReader(`{"privacy_mode": true}`))
	reqInvalidBearer.Header.Set("Authorization", "Bearer invalid-token-12345")
	rrInvalidBearer := httptest.NewRecorder()
	if om.ValidateSecurityBoundary(rrInvalidBearer, reqInvalidBearer, false) {
		t.Fatal("expected invalid Bearer token to be rejected")
	}
	if rrInvalidBearer.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 Unauthorized for invalid Bearer token, got %d", rrInvalidBearer.Code)
	}

	// 3. Unauthenticated request with requireAuth=false MUST pass
	reqPublic := httptest.NewRequest(http.MethodPost, "/api/v1/onboarding/setup", strings.NewReader(`{}`))
	rrPublic := httptest.NewRecorder()
	if !om.ValidateSecurityBoundary(rrPublic, reqPublic, false) {
		t.Fatal("expected unauthenticated request to pass when requireAuth=false")
	}

	// 4. Unauthenticated request with requireAuth=true MUST return 401
	reqProtected := httptest.NewRequest(http.MethodPost, "/api/v1/firewall/mode", strings.NewReader(`{"privacy_mode": true}`))
	rrProtected := httptest.NewRecorder()
	if om.ValidateSecurityBoundary(rrProtected, reqProtected, true) {
		t.Fatal("expected unauthenticated request to be rejected when requireAuth=true")
	}
	if rrProtected.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 Unauthorized, got %d", rrProtected.Code)
	}
}
