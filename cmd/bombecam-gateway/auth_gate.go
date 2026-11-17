package main

import (
	"context"
	"net/http"
	"strings"
)

// The gateway has one rule for who may call a route, and exactly one place
// applies it.
//
// The rule used to be spread over the security middleware, a map of public
// paths, and a check inside most handlers. Routes then asked for a sign-in
// their neighbours did not, some checked twice, and the middleware had to
// reach for the process-wide manager because a handler's own stream manager
// never reached it. Now a route names its level when it is registered (gate),
// and the middleware enforces the level it finds.

// authLevel is how much a route demands of its caller.
type authLevel int

const (
	// authHostOnly: the Host header, nothing else. The page and its files must
	// load with an expired session, and the CSRF endpoints exist to replace one.
	authHostOnly authLevel = iota
	// authHostOrigin: Host, Origin and credentials, but no sign-in. The routes
	// that sign someone in or out, and the status the page asks before it can.
	authHostOrigin
	// authSignIn: those checks plus a signed-in operator, whenever
	// signInRequired says one is needed. What nearly every route wants.
	authSignIn
	// authOperatorOnly: always a signed-in operator. Reading the camera's own
	// switches stays here so a browser on the LAN cannot poll them with only a
	// visitor cookie.
	authOperatorOnly
)

// authGate is a handler that carries its own auth level, so registrations read
// as "this route wants a sign-in" with nothing to remember at the handler.
type authGate struct {
	level authLevel
	next  http.HandlerFunc
}

// gated wraps a route handler with the level the middleware must enforce.
func gated(level authLevel, next http.HandlerFunc) http.HandlerFunc {
	g := &authGate{level: level, next: next}
	return g.ServeHTTP
}

// ServeHTTP marks the request with its route's level before the handler runs.
// The middleware wraps the mux, so it cannot see this wrapper directly; the
// mark travels on the request.
func (g *authGate) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	g.next(w, r.WithContext(context.WithValue(r.Context(), gateKey{}, g.level)))
}

// levelFor is the auth level a request is enforced at: the one its route
// named, else the default for its path.
func levelFor(r *http.Request) authLevel {
	if l, ok := r.Context().Value(gateKey{}).(authLevel); ok {
		return l
	}
	return defaultLevel(r)
}

type gateKey struct{}

// defaultLevel is the level of a route that did not name one.
func defaultLevel(r *http.Request) authLevel {
	switch {
	case isCSRFEndpoint(r) || isStaticAsset(r):
		return authHostOnly
	case publicAPIPaths[r.URL.Path]:
		return authHostOrigin
	default:
		return authSignIn
	}
}

// publicAPIPaths are reachable without a sign-in: they sign someone in, or
// report what the page needs before it can ask. Each still checks Host, Origin
// and credentials (authHostOrigin), so a forged or expired cookie is refused.
var publicAPIPaths = map[string]bool{
	"/api/v1/operator/token":   true, // exchanges credentials for a token
	"/api/v1/admin/status":     true, // tells the page whether to ask for a password
	"/api/v1/admin/setup":      true, // this PC only, before an administrator exists
	"/api/v1/admin/signin":     true,
	"/api/v1/admin/signout":    true,
	"/api/v1/admin/start-over": true,
}

// isCSRFEndpoint reports the routes that hand out a session and its CSRF
// token. They must answer even to a browser holding an expired cookie.
func isCSRFEndpoint(r *http.Request) bool {
	p := r.URL.Path
	return p == "/api/v1/auth/csrf" || p == "/api/v1/operator/csrf"
}

// isStaticAsset reports the page and its files, which carry no data of their
// own. Only their Host is checked, so an expired session still shows the page.
func isStaticAsset(r *http.Request) bool {
	p := r.URL.Path
	if p == "/" || p == "/index.html" {
		return true
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return false
	}
	for _, ext := range []string{".js", ".css", ".svg", ".png", ".ico", ".woff2", ".map", ".jpg", ".webp"} {
		if strings.HasSuffix(p, ext) {
			return true
		}
	}
	return false
}
