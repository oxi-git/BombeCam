package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

var (
	ErrInvalidToken    = errors.New("invalid or expired token")
	ErrInvalidCookie   = errors.New("invalid or expired session cookie")
	ErrForbiddenHost   = errors.New("forbidden host header")
	ErrForbiddenOrigin = errors.New("forbidden origin header")
	ErrInvalidCSRF     = errors.New("invalid or missing csrf token")
	ErrUnauthorized    = errors.New("unauthorized")
)

const (
	SessionCookieName = "bombecam_session"
	CSRFHeaderName    = "X-CSRF-Token"
	DefaultSessionTTL = 24 * time.Hour
)

// Session represents an active operator or visitor session.
type Session struct {
	Token      string    `json:"token"`
	CSRFToken  string    `json:"csrf_token"`
	CreatedAt  time.Time `json:"created_at"`
	ExpiresAt  time.Time `json:"expires_at"`
	IsOperator bool      `json:"is_operator"`
}

// OperatorConfig holds configuration for the OperatorManager.
type OperatorConfig struct {
	AllowedHosts []string
	SessionTTL   time.Duration
	KeyFile      string
}

// OperatorManager manages operator sessions, API tokens, Host and Origin validation,
// and CSRF defense against DNS rebinding and cross-origin attacks.
type OperatorManager struct {
	mu           sync.RWMutex
	allowedHosts map[string]struct{}
	sessions     map[string]*Session
	apiTokens    map[string]time.Time
	sessionTTL   time.Duration
	keyFile      string
}

// NewOperatorManager creates a new OperatorManager with safe defaults.
func NewOperatorManager(cfg OperatorConfig) *OperatorManager {
	ttl := cfg.SessionTTL
	if ttl <= 0 {
		ttl = DefaultSessionTTL
	}

	om := &OperatorManager{
		allowedHosts: make(map[string]struct{}),
		sessions:     make(map[string]*Session),
		apiTokens:    make(map[string]time.Time),
		sessionTTL:   ttl,
		keyFile:      cfg.KeyFile,
	}

	// Standard allowed hosts for local operator access
	defaults := []string{
		"127.0.0.1",
		"localhost",
		"::1",
		"[::1]",
		"host.docker.internal",
		"example.com", // standard Go httptest host
	}
	for _, h := range defaults {
		om.allowedHosts[strings.ToLower(h)] = struct{}{}
	}

	for _, h := range cfg.AllowedHosts {
		if trimmed := strings.TrimSpace(strings.ToLower(h)); trimmed != "" {
			om.allowedHosts[trimmed] = struct{}{}
		}
	}

	om.loadPersistedAPITokens()
	return om
}

// AddAllowedHost registers an additional allowed host or IP (e.g. configured LAN IP).
func (om *OperatorManager) AddAllowedHost(host string) {
	om.mu.Lock()
	defer om.mu.Unlock()
	h := cleanHost(host)
	if h != "" {
		om.allowedHosts[h] = struct{}{}
	}
}

func cleanHost(host string) string {
	h := strings.TrimSpace(strings.ToLower(host))
	if stripped, _, err := net.SplitHostPort(h); err == nil {
		h = stripped
	}
	h = strings.TrimPrefix(h, "[")
	h = strings.TrimSuffix(h, "]")
	return h
}

// IsLoopbackAddr reports whether the provided IP address or host:port string is a loopback address.
func IsLoopbackAddr(addr string) bool {
	if addr == "" {
		return false
	}
	h := cleanHost(addr)
	if h == "localhost" {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && (ip.IsLoopback() || ip.IsUnspecified())
}

// ValidateHost validates incoming Host header to defend against DNS rebinding attacks.
func (om *OperatorManager) ValidateHost(hostHeader string) bool {
	if hostHeader == "" {
		return false
	}
	h := cleanHost(hostHeader)
	if h == "" {
		return false
	}

	om.mu.RLock()
	defer om.mu.RUnlock()

	// Direct match in allowed hosts
	if _, ok := om.allowedHosts[h]; ok {
		return true
	}

	// Valid IP literal (DNS rebinding attacks require a domain name, not an IP literal)
	if ip := net.ParseIP(h); ip != nil {
		return true
	}

	return false
}

// ValidateOrigin validates the Origin header on state-mutating requests.
func (om *OperatorManager) ValidateOrigin(originHeader string) bool {
	if originHeader == "" {
		return true // direct non-browser requests
	}

	u, err := url.Parse(originHeader)
	if err != nil {
		return false
	}

	host := u.Hostname()
	if host == "" {
		return false
	}

	return om.ValidateHost(host)
}

// ValidateRequestOrigin binds browser mutations to this exact origin, including
// its port. An arbitrary allowed IP/domain is not the gateway's own origin.
func (om *OperatorManager) ValidateRequestOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return err == nil && u.User == nil && u.Path == "" && u.RawQuery == "" && u.Fragment == "" &&
		strings.EqualFold(u.Scheme, scheme) && strings.EqualFold(u.Host, r.Host) && om.ValidateHost(u.Host)
}

// CreateSession creates a new authenticated operator session with a secure token and CSRF token.
func (om *OperatorManager) CreateSession() (*Session, error) {
	return om.createSessionInternal(true)
}

// CreateVisitorSession creates an unauthenticated visitor session with CSRF protection.
func (om *OperatorManager) CreateVisitorSession() (*Session, error) {
	return om.createSessionInternal(false)
}

// ElevateSession marks an active visitor session as an authenticated operator session.
func (om *OperatorManager) ElevateSession(token string) bool {
	if token == "" {
		return false
	}
	om.mu.Lock()
	defer om.mu.Unlock()
	sess, ok := om.sessions[token]
	if !ok || sess == nil {
		return false
	}
	sess.IsOperator = true
	return true
}

func (om *OperatorManager) createSessionInternal(isOperator bool) (*Session, error) {
	tokBytes := make([]byte, 32)
	if _, err := rand.Read(tokBytes); err != nil {
		return nil, fmt.Errorf("failed to generate session token: %w", err)
	}
	csrfBytes := make([]byte, 32)
	if _, err := rand.Read(csrfBytes); err != nil {
		return nil, fmt.Errorf("failed to generate csrf token: %w", err)
	}

	sess := &Session{
		Token:      hex.EncodeToString(tokBytes),
		CSRFToken:  hex.EncodeToString(csrfBytes),
		CreatedAt:  time.Now().UTC(),
		ExpiresAt:  time.Now().UTC().Add(om.sessionTTL),
		IsOperator: isOperator,
	}

	om.mu.Lock()
	defer om.mu.Unlock()
	om.sessions[sess.Token] = sess
	return sess, nil
}

// IssueSessionCookie sets an HttpOnly; SameSite=Lax session cookie on ResponseWriter.
// Lax allows the cookie on top-level navigations (e.g., login POST redirect) which is
// required for LAN access where the cookie origin (IP) differs from the referrer.
func (om *OperatorManager) IssueSessionCookie(w http.ResponseWriter, sess *Session) {
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookieName,
		Value:    sess.Token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Expires:  sess.ExpiresAt,
	})
}

// ClearSessionCookie clears the session cookie.
func (om *OperatorManager) ClearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
		Expires:  time.Unix(0, 0),
	})
}

// VerifySession verifies that a session token exists and has not expired.
func (om *OperatorManager) VerifySession(token string) (*Session, bool) {
	if token == "" {
		return nil, false
	}

	om.mu.RLock()
	defer om.mu.RUnlock()

	sess, ok := om.sessions[token]
	if !ok || sess == nil {
		return nil, false
	}

	if time.Now().UTC().After(sess.ExpiresAt) {
		return nil, false
	}

	return sess, true
}

// VerifyCSRFToken verifies that the CSRF token matches the session's CSRF token.
func (om *OperatorManager) VerifyCSRFToken(sessionToken, csrfToken string) bool {
	sess, ok := om.VerifySession(sessionToken)
	if !ok || sess == nil || csrfToken == "" {
		return false
	}

	return subtle.ConstantTimeCompare([]byte(sess.CSRFToken), []byte(csrfToken)) == 1
}

// GenerateAPIToken generates and persists a new API Bearer token for programmatic integrations (Frigate, Home Assistant).
func (om *OperatorManager) GenerateAPIToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("failed to generate api token: %w", err)
	}
	tok := "bc_tok_" + hex.EncodeToString(b)

	om.mu.Lock()
	om.apiTokens[tok] = time.Now().UTC()
	om.mu.Unlock()

	om.savePersistedAPITokens()
	return tok, nil
}

// VerifyAPIToken verifies a programmatic Bearer token.
func (om *OperatorManager) VerifyAPIToken(token string) bool {
	if token == "" {
		return false
	}
	om.mu.RLock()
	defer om.mu.RUnlock()
	_, ok := om.apiTokens[token]
	return ok
}

// AuthenticateRequest inspects the HTTP request for authentication.
// Returns (authenticated bool, isBearer bool, sess *Session).
func (om *OperatorManager) AuthenticateRequest(r *http.Request) (bool, bool, *Session) {
	authOK, isBearer, sess, _ := om.AuthenticateRequestWithStatus(r)
	return authOK, isBearer, sess
}

// AuthenticateRequestWithStatus inspects the HTTP request for authentication,
// distinguishing valid credentials, missing credentials, and invalid credentials.
// Returns (authOK bool, isBearer bool, sess *Session, credErr error).
func (om *OperatorManager) AuthenticateRequestWithStatus(r *http.Request) (bool, bool, *Session, error) {
	// 1. Check Bearer token (programmatic API clients)
	authHeader := r.Header.Get("Authorization")
	if authHeader != "" {
		if strings.HasPrefix(authHeader, "Bearer ") {
			token := strings.TrimSpace(strings.TrimPrefix(authHeader, "Bearer "))
			if token == "" {
				return false, true, nil, ErrInvalidToken
			}
			if om.VerifyAPIToken(token) {
				return true, true, nil, nil
			}
			if sess, ok := om.VerifySession(token); ok {
				return sess.IsOperator, true, sess, nil
			}
			return false, true, nil, ErrInvalidToken
		}
		if !strings.HasPrefix(authHeader, "Basic ") {
			return false, false, nil, ErrInvalidToken
		}
	}

	// 2. Check session cookie (browser sessions)
	cookie, err := r.Cookie(SessionCookieName)
	if err == nil && cookie != nil && cookie.Value != "" {
		if sess, ok := om.VerifySession(cookie.Value); ok {
			return sess.IsOperator, false, sess, nil
		}
		return false, false, nil, ErrInvalidCookie
	}

	return false, false, nil, nil
}

// ValidateSecurityBoundary performs complete Host, Origin, and CSRF boundary checks on a request.
func (om *OperatorManager) ValidateSecurityBoundary(w http.ResponseWriter, r *http.Request, requireAuth bool) bool {
	// 1. Host header validation (DNS rebinding prevention)
	if !om.ValidateHost(r.Host) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error":   "forbidden_host",
			"message": "Host header rejected by local security boundary",
		})
		return false
	}

	// 2. Origin validation on state-mutating requests
	isStateMutating := r.Method == http.MethodPost || r.Method == http.MethodPut || r.Method == http.MethodDelete || r.Method == http.MethodPatch
	origin := r.Header.Get("Origin")
	if isStateMutating && origin != "" {
		if !om.ValidateRequestOrigin(r) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"error":   "forbidden_origin",
				"message": "Origin rejected by cross-origin security boundary",
			})
			return false
		}
	}

	// 3. Inspect credentials
	authOK, isBearer, sess, credErr := om.AuthenticateRequestWithStatus(r)

	// If credentials were provided but are invalid/expired, reject immediately with 401 Unauthorized
	if credErr != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		errMsg := "Invalid or expired session cookie"
		if errors.Is(credErr, ErrInvalidToken) {
			errMsg = "Invalid or expired authorization token"
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error":   "unauthorized",
			"message": errMsg,
		})
		return false
	}

	// 4. If authentication is required but missing, reject with 401 Unauthorized
	if requireAuth && !authOK {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error":   "unauthorized",
			"message": "Local operator authentication required",
		})
		return false
	}

	// 5. Programmatic Bearer clients are exempt from CSRF checks
	if isBearer && authOK {
		return true
	}

	// 6. Browser cookie-authenticated clients MUST provide valid CSRF token on state-mutating requests
	if sess != nil && !isBearer && isStateMutating {
		csrf := r.Header.Get(CSRFHeaderName)
		if csrf == "" || !om.VerifyCSRFToken(sess.Token, csrf) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"error":   "invalid_csrf_token",
				"message": "Missing or invalid CSRF token on state-mutating request",
			})
			return false
		}
	}

	return true
}

func (om *OperatorManager) loadPersistedAPITokens() {
	if om.keyFile == "" {
		return
	}
	data, err := os.ReadFile(om.keyFile)
	if err != nil {
		return
	}
	var tokens map[string]time.Time
	if err := json.Unmarshal(data, &tokens); err == nil {
		om.mu.Lock()
		for k, v := range tokens {
			om.apiTokens[k] = v
		}
		om.mu.Unlock()
	}
}

func (om *OperatorManager) savePersistedAPITokens() {
	if om.keyFile == "" {
		return
	}
	om.mu.RLock()
	data, err := json.Marshal(om.apiTokens)
	om.mu.RUnlock()
	if err != nil {
		return
	}
	_ = os.MkdirAll(filepath.Dir(om.keyFile), 0700)
	_ = os.WriteFile(om.keyFile, data, 0600)
}
