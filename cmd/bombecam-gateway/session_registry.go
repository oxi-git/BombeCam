package main

import (
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/Fever-r/BombeCam/pkg/bridge"
	"github.com/Fever-r/BombeCam/pkg/profile"
	"github.com/Fever-r/BombeCam/pkg/serverkey"
)

// SessionRegistry holds one Osaio cloud session per stored login. Every login
// is equal: cameras stream and are controlled through the session of the
// login they were added with, and no login is the "main" one.
//
// The template session is never a login's session. It carries what every
// new session starts from (the server key, Osaio's addresses, the time zone,
// the default country) and the gateway's own state while no login is
// signed in (first run, waiting for a server key or app ID, restoring at
// start-up).
type SessionRegistry struct {
	mu        sync.RWMutex
	country   string
	phoneCode string
	keys      serverkey.Provider
	template  *SessionManager
	byEmail   map[string]*SessionManager
	restoring bool
}

// NewSessionRegistry creates an empty registry. New sessions copy the
// template's cloud client; without a template they sign with keys.
func NewSessionRegistry(template *SessionManager, country, phoneCode string, keys serverkey.Provider) *SessionRegistry {
	return &SessionRegistry{
		country:   country,
		phoneCode: phoneCode,
		keys:      keys,
		template:  template,
		byEmail:   make(map[string]*SessionManager),
	}
}

// Template returns the session new logins start from.
func (r *SessionRegistry) Template() *SessionManager {
	return r.template
}

// newSessionLocked builds a signed-out session for a login.
func (r *SessionRegistry) newSessionLocked(country string) *SessionManager {
	if country == "" {
		country = r.country
	}
	if r.template != nil && r.template.Cloud() != nil {
		c := r.template.Cloud().Clone()
		if country != "" {
			c.Country = country
		}
		phone := r.template.PhoneCode()
		if phone == "" {
			phone = r.phoneCode
		}
		return &SessionManager{cloud: c, country: country, phoneCode: phone, status: SessionStatusUnavailable}
	}
	return NewSessionManager(country, r.phoneCode, r.keys)
}

// NewTrialSession returns a session that belongs to no login, for checking a
// password before anything stored is touched.
func (r *SessionRegistry) NewTrialSession(country string) *SessionManager {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.newSessionLocked(country)
}

// Get returns the session for a login, if one exists.
func (r *SessionRegistry) Get(email string) (*SessionManager, bool) {
	email = profile.NormalizeEmail(email)
	r.mu.RLock()
	defer r.mu.RUnlock()
	sm, ok := r.byEmail[email]
	return sm, ok && email != ""
}

// Ensure returns the existing session for a login or creates a signed-out one.
func (r *SessionRegistry) Ensure(email string) *SessionManager {
	email = profile.NormalizeEmail(email)
	r.mu.Lock()
	defer r.mu.Unlock()
	if sm, ok := r.byEmail[email]; ok {
		return sm
	}
	sm := r.newSessionLocked("")
	sm.email = email
	r.byEmail[email] = sm
	return sm
}

// Login signs a login in on its own session, creating it if necessary.
func (r *SessionRegistry) Login(email, password string) (*SessionManager, error) {
	sm := r.Ensure(email)
	return sm, sm.Login(profile.NormalizeEmail(email), password)
}

// CloudFor returns a login's cloud while it is signed in, else nil.
func (r *SessionRegistry) CloudFor(email string) *bridge.Cloud {
	if sm, ok := r.Get(email); ok && sm.IsAuthenticated() {
		return sm.Cloud()
	}
	return nil
}

// Remove drops a login's session (the stored login is not affected).
func (r *SessionRegistry) Remove(email string) {
	email = profile.NormalizeEmail(email)
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.byEmail, email)
}

// Clear forgets every login's session (Delete all stored information).
func (r *SessionRegistry) Clear() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.byEmail = make(map[string]*SessionManager)
}

// Emails lists the logins that have a session, sorted.
func (r *SessionRegistry) Emails() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.byEmail))
	for e := range r.byEmail {
		out = append(out, e)
	}
	sort.Strings(out)
	return out
}

// Sessions returns every login's session, in email order.
func (r *SessionRegistry) Sessions() []*SessionManager {
	emails := r.Emails()
	out := make([]*SessionManager, 0, len(emails))
	for _, e := range emails {
		if sm, ok := r.Get(e); ok {
			out = append(out, sm)
		}
	}
	return out
}

// SetRestoring marks start-up restore in progress, so the page waits instead
// of asking for a sign-in.
func (r *SessionRegistry) SetRestoring(on bool) {
	r.mu.Lock()
	r.restoring = on
	r.mu.Unlock()
}

// AnyAuthenticated reports whether at least one login is signed in.
func (r *SessionRegistry) AnyAuthenticated() bool {
	for _, sm := range r.Sessions() {
		if sm.IsAuthenticated() {
			return true
		}
	}
	return false
}

// Status sums up the logins for the page: signed in as soon as any login is,
// and otherwise the state that most needs the user. With no login session it
// is the gateway's own state (first run, server key, start-up problems).
func (r *SessionRegistry) Status() SessionStatusInfo {
	r.mu.RLock()
	restoring := r.restoring
	r.mu.RUnlock()
	var gateway SessionStatusInfo
	if r.template != nil {
		gateway = r.template.GetStatus()
		gateway.AccountEmail, gateway.VendorUID = "", ""
		if gateway.Status == SessionStatusServerKeyMissing || gateway.Status == SessionStatusAppIDMissing {
			return gateway
		}
	}
	sessions := r.Sessions()
	if len(sessions) == 0 {
		if restoring {
			return SessionStatusInfo{Status: SessionStatusAuthenticating, Error: "restoring saved logins"}
		}
		if gateway.Status == "" {
			gateway.Status = SessionStatusCredentialsMissing
		}
		return gateway
	}
	rank := map[SessionStatus]int{
		SessionStatusAuthenticated:      0,
		SessionStatusAuthenticating:     1,
		SessionStatusAppIDMissing:       2,
		SessionStatusServerKeyMissing:   2,
		SessionStatusInvalidCredentials: 3,
		SessionStatusCredentialsMissing: 4,
		SessionStatusNetworkTimeout:     5,
		SessionStatusUnavailable:        6,
	}
	best := SessionStatusInfo{Status: SessionStatusUnavailable}
	bestRank := 99
	for _, sm := range sessions {
		st := sm.GetStatus()
		if r, ok := rank[st.Status]; ok && r < bestRank {
			best, bestRank = st, r
		}
	}
	if restoring && best.Status != SessionStatusAuthenticated {
		return SessionStatusInfo{Status: SessionStatusAuthenticating, Error: "restoring saved logins"}
	}
	best.AccountEmail, best.VendorUID = "", ""
	return best
}

// errNoLoginSignedIn: no stored login is signed in to list cameras with.
var errNoLoginSignedIn = errors.New("no Osaio login is signed in")

// DeviceList lists the cameras every signed-in login can see, each with the
// login it came from. A login whose list fails is skipped; it is an error
// only when no login could list its cameras.
func (r *SessionRegistry) DeviceList(refresh bool) ([]bridge.Device, map[string]string, time.Time, error) {
	var devs []bridge.Device
	owners := map[string]string{}
	var newest time.Time
	var lastErr error
	listed := false
	for _, sm := range r.Sessions() {
		if !sm.IsAuthenticated() {
			continue
		}
		list, ts, err := sm.DeviceList(refresh)
		if err != nil {
			lastErr = err
			continue
		}
		listed = true
		if ts.After(newest) {
			newest = ts
		}
		email := profile.NormalizeEmail(sm.Email())
		for _, d := range list {
			if _, dup := owners[d.UUID]; dup {
				continue // shared with several logins: the first one lists it
			}
			owners[d.UUID] = email
			devs = append(devs, d)
		}
	}
	if !listed {
		if lastErr == nil {
			lastErr = errNoLoginSignedIn
		}
		return nil, nil, time.Time{}, lastErr
	}
	return devs, owners, newest, nil
}

// FindDevice returns the cached Osaio device entry for a camera from any
// signed-in login, and that login's session.
func (r *SessionRegistry) FindDevice(uuid string) (bridge.Device, *SessionManager, bool) {
	for _, sm := range r.Sessions() {
		for _, d := range sm.GetCachedDevices() {
			if d.UUID == uuid {
				return d, sm, true
			}
		}
	}
	return bridge.Device{}, nil, false
}
