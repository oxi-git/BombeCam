package main

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/Fever-r/BombeCam/pkg/auth"
)

func resetShutdownForTest(t *testing.T) {
	t.Helper()
	shutdownOnce = sync.Once{}
	shutdownRequested = make(chan struct{})
	old := shutdownDelay
	shutdownDelay = 0
	t.Cleanup(func() { shutdownDelay = old })
}

func shutdownFired() bool {
	select {
	case <-shutdownRequested:
		return true
	case <-time.After(500 * time.Millisecond):
		return false
	}
}

// The Settings dialog's Shut down button: allowed from this PC with the page's
// CSRF token, refused from the network without operator auth, POST only.
func TestGatewayShutdown(t *testing.T) {
	om := auth.NewOperatorManager(auth.OperatorConfig{KeyFile: t.TempDir() + "/tokens.json"})
	SetGatewayOperatorManager(om)
	defer SetGatewayOperatorManager(nil)

	resetShutdownForTest(t)
	rec := httptest.NewRecorder()
	handleGatewayShutdown(rec, httptest.NewRequest(http.MethodGet, "/api/v1/gateway/shutdown", nil), nil)
	if rec.Code != http.StatusMethodNotAllowed || shutdownFired() {
		t.Fatalf("GET must be refused and not shut down (code %d)", rec.Code)
	}

	// From another device, without credentials.
	resetShutdownForTest(t)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/gateway/shutdown", nil)
	req.Host = "127.0.0.1:8654"
	req.RemoteAddr = "192.168.1.50:50000"
	rec = httptest.NewRecorder()
	handleGatewayShutdown(rec, req, nil)
	if rec.Code != http.StatusUnauthorized || shutdownFired() {
		t.Fatalf("an unauthenticated request from the network must be refused (code %d)", rec.Code)
	}

	// The web page on this PC, with its session cookie but no CSRF token.
	sess, err := om.CreateSession()
	if err != nil {
		t.Fatal(err)
	}
	resetShutdownForTest(t)
	req = httptest.NewRequest(http.MethodPost, "/api/v1/gateway/shutdown", nil)
	req.Host = "127.0.0.1:8654"
	req.RemoteAddr = "127.0.0.1:50000"
	req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: sess.Token})
	rec = httptest.NewRecorder()
	handleGatewayShutdown(rec, req, nil)
	if rec.Code != http.StatusForbidden || shutdownFired() {
		t.Fatalf("a browser request without the CSRF token must be refused (code %d)", rec.Code)
	}

	// ...and with it.
	resetShutdownForTest(t)
	req.Header.Set(auth.CSRFHeaderName, sess.CSRFToken)
	rec = httptest.NewRecorder()
	handleGatewayShutdown(rec, req, nil)
	if rec.Code != http.StatusOK || !shutdownFired() {
		t.Fatalf("the page on this PC must be able to shut down (code %d, body %s)", rec.Code, rec.Body.String())
	}
	requestShutdown() // a second press is harmless
}
