package main

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Fever-r/BombeCam/pkg/bridge"
	"github.com/Fever-r/BombeCam/pkg/serverkey"
)

// SessionStatus represents the vendor cloud authentication state.
type SessionStatus string

const (
	SessionStatusAuthenticated      SessionStatus = "authenticated"
	SessionStatusAuthenticating     SessionStatus = "authenticating"
	SessionStatusCredentialsMissing SessionStatus = "credentials_missing"
	SessionStatusInvalidCredentials SessionStatus = "invalid_credentials"
	SessionStatusNetworkTimeout     SessionStatus = "network_timeout"
	SessionStatusUnavailable        SessionStatus = "unavailable"
	// SessionStatusServerKeyMissing: no usable server key, so nothing was sent.
	SessionStatusServerKeyMissing SessionStatus = "server_key_missing"
	// SessionStatusAppIDMissing: no usable Osaio app ID, so nothing was sent.
	SessionStatusAppIDMissing SessionStatus = "app_id_missing"
)

// appIDMissingMessage is shown when sign-in cannot start for lack of an app ID.
const appIDMissingMessage = "BombeCam needs the Osaio app ID before it can sign in. Enter it on the sign-in page or set it as described in docs/technical/CONFIGURATION.md."

// serverKeyMissingMessage is shown when sign-in cannot start for lack of a key.
const serverKeyMissingMessage = "BombeCam needs the Osaio server key before it can sign in. Enter it on the sign-in page or set it as described in docs/technical/CONFIGURATION.md."

// rejectedSignInMessage covers a sign-in Osaio refused. A wrong password and
// an outdated server key or app ID can look the same from here, so all are
// named.
const rejectedSignInMessage = "Osaio rejected the sign-in. Check the email and password. If Osaio recently updated its app, the server key or app ID may also need updating (Settings > Server key, Settings > App ID)."

// SessionStatusInfo is returned by GET /api/v1/session/status.
// Note: Passwords and vendor auth tokens are strictly omitted.
type SessionStatusInfo struct {
	Status       SessionStatus `json:"status"`
	AccountEmail string        `json:"account_email"`
	VendorUID    string        `json:"vendor_uid,omitempty"`
	LastLoginTS  string        `json:"last_login_ts,omitempty"`
	Error        string        `json:"error"`
}

// SessionManager is one Osaio login's cloud session (see SessionRegistry).
type SessionManager struct {
	mu          sync.RWMutex
	cloud       *bridge.Cloud
	email       string
	country     string
	phoneCode   string
	status      SessionStatus
	lastError   string
	lastLoginTS time.Time
	cachedDevs  []bridge.Device
	discoveryTS time.Time
}

// NewSessionManager creates a new SessionManager whose cloud client signs
// with the key from keys.
func NewSessionManager(country, phoneCode string, keys serverkey.Provider) *SessionManager {
	return &SessionManager{
		cloud:     bridge.NewCloud(country, phoneCode, keys),
		country:   country,
		phoneCode: phoneCode,
		status:    SessionStatusUnavailable,
	}
}

// Login attempts authentication with the OSAIO vendor cloud without crashing the host process.
func (sm *SessionManager) Login(email, password string) error {
	sm.mu.Lock()
	if email == "" || password == "" {
		sm.status = SessionStatusCredentialsMissing
		sm.email = email
		sm.lastError = "missing credentials: use --email/--password or env OSAIO_EMAIL/OSAIO_PASSWORD"
		message := sm.lastError
		sm.mu.Unlock()
		return fmt.Errorf("%s", message)
	}

	sm.email = email
	country, validCountry := normalizedCountry(sm.country)
	if !validCountry {
		sm.status = SessionStatusCredentialsMissing
		sm.lastError = "Account country calling code is required; correct it when signing in."
		message := sm.lastError
		sm.mu.Unlock()
		return fmt.Errorf("%s", message)
	}
	sm.country = country
	if sm.cloud != nil {
		sm.cloud.Country = country
	}
	sm.status = SessionStatusAuthenticating
	sm.lastError = ""
	cloud := sm.cloud
	sm.mu.Unlock()

	cloud.GetBaseURL(email)
	err := cloud.Login(email, password)

	sm.mu.Lock()
	defer sm.mu.Unlock()

	if err != nil {
		var httpErr *bridge.UpstreamHTTPError
		if errors.Is(err, bridge.ErrNoAppID) {
			sm.status = SessionStatusAppIDMissing
			sm.lastError = appIDMissingMessage
		} else if errors.Is(err, bridge.ErrServerKey) {
			sm.status = SessionStatusServerKeyMissing
			sm.lastError = serverKeyMissingMessage
		} else if (errors.As(err, &httpErr) && (httpErr.StatusCode == 503 || httpErr.StatusCode == 504)) || strings.Contains(strings.ToLower(err.Error()), "timeout") || strings.Contains(strings.ToLower(err.Error()), "deadline") {
			sm.status = SessionStatusNetworkTimeout
			sm.lastError = "Connection to vendor cloud timed out."
		} else if errors.Is(err, bridge.ErrUpstreamServiceUnavailable) || (errors.As(err, &httpErr) && httpErr.StatusCode >= 500) {
			sm.status = SessionStatusUnavailable
			sm.lastError = "Vendor cloud service is currently unavailable. Please try again later."
		} else if errors.Is(err, bridge.ErrInvalidCredentials) || (errors.As(err, &httpErr) && (httpErr.StatusCode == 401 || httpErr.StatusCode == 403)) || strings.Contains(strings.ToLower(err.Error()), "password") || strings.Contains(strings.ToLower(err.Error()), "credential") || strings.Contains(strings.ToLower(err.Error()), "code=") {
			sm.status = SessionStatusInvalidCredentials
			sm.lastError = rejectedSignInMessage
		} else {
			sm.status = SessionStatusUnavailable
			sm.lastError = "Unable to connect to vendor cloud service."
		}
		return err
	}

	sm.status = SessionStatusAuthenticated
	sm.lastLoginTS = time.Now().UTC()
	sm.lastError = ""
	return nil
}

// SetCountry sets the account country code and propagates to bridge.Cloud.
func (sm *SessionManager) SetCountry(country string) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	sm.country = country
	if sm.cloud != nil {
		sm.cloud.Country = country
	}
}

// Country returns the account country code.
func (sm *SessionManager) Country() string {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	return sm.country
}

// SetPhoneCode sets the phone code and propagates to bridge.Cloud.
func (sm *SessionManager) SetPhoneCode(phoneCode string) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	sm.phoneCode = phoneCode
	if sm.cloud != nil {
		sm.cloud.PhoneCode = phoneCode
	}
}

// PhoneCode returns the phone code.
func (sm *SessionManager) PhoneCode() string {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	return sm.phoneCode
}

// SetStatus manually updates status and error (e.g. for testing or startup detection).
func (sm *SessionManager) SetStatus(status SessionStatus, errStr string) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	sm.status = status
	sm.lastError = errStr
}

// GetStatus returns the current session status without exposing sensitive credentials.
func (sm *SessionManager) GetStatus() SessionStatusInfo {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	info := SessionStatusInfo{
		Status:       sm.status,
		AccountEmail: sm.email,
		Error:        sm.lastError,
	}
	if sm.status == SessionStatusAuthenticated && sm.cloud != nil {
		info.VendorUID = sm.cloud.UID
	}
	if !sm.lastLoginTS.IsZero() {
		info.LastLoginTS = sm.lastLoginTS.Format(time.RFC3339)
	}
	return info
}

// IsAuthenticated returns true if session status is SessionStatusAuthenticated.
func (sm *SessionManager) IsAuthenticated() bool {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	return sm.status == SessionStatusAuthenticated
}

// DeviceList queries device inventory from the vendor cloud, caching results.
func (sm *SessionManager) DeviceList(refresh bool) ([]bridge.Device, time.Time, error) {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	if sm.status != SessionStatusAuthenticated {
		return nil, time.Time{}, fmt.Errorf("unauthenticated")
	}

	// Use cache if available and not explicitly refreshing within 5 minutes
	if !refresh && !sm.discoveryTS.IsZero() && time.Since(sm.discoveryTS) < 5*time.Minute {
		res := make([]bridge.Device, len(sm.cachedDevs))
		copy(res, sm.cachedDevs)
		return res, sm.discoveryTS, nil
	}

	devs, err := sm.cloud.DeviceList()
	if err != nil {
		return nil, time.Time{}, err
	}

	sm.cachedDevs = devs
	sm.discoveryTS = time.Now().UTC()
	for _, d := range devs {
		logDeviceSettings(d)
	}
	res := make([]bridge.Device, len(devs))
	copy(res, devs)
	return res, sm.discoveryTS, nil
}

// GetCachedDevices returns a copy of discovered devices currently held in memory.
func (sm *SessionManager) GetCachedDevices() []bridge.Device {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	res := make([]bridge.Device, len(sm.cachedDevs))
	copy(res, sm.cachedDevs)
	return res
}

// Cloud returns the underlying bridge.Cloud instance.
// Email returns the account email this session is associated with.
func (sm *SessionManager) Email() string {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	return sm.email
}

func (sm *SessionManager) Cloud() *bridge.Cloud {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	return sm.cloud
}

// SetCloud allows replacing the cloud client (e.g. for testing).
func (sm *SessionManager) SetCloud(c *bridge.Cloud) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	sm.cloud = c
}

// Adopt takes over a sign-in made elsewhere (a verified login, or another
// login's session moving into this one) without contacting Osaio again.
func (sm *SessionManager) Adopt(email, country string, c *bridge.Cloud) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	if c != nil {
		sm.cloud = c
	}
	sm.email = email
	if country != "" {
		sm.country = country
	}
	sm.status = SessionStatusAuthenticated
	sm.lastError = ""
	sm.lastLoginTS = time.Now().UTC()
	sm.cachedDevs = nil
	sm.discoveryTS = time.Time{}
}

// Reset clears credentials, tokens, and session state back to first-run state.
func (sm *SessionManager) Reset() {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	sm.email = ""
	sm.status = SessionStatusCredentialsMissing
	sm.lastError = "first-run: no profile configured; complete onboarding via API"
	sm.lastLoginTS = time.Time{}
	sm.cachedDevs = nil
	sm.discoveryTS = time.Time{}
	if sm.cloud != nil {
		sm.cloud.UID = ""
		sm.cloud.APIToken = ""
	}
}

// deviceListSetting returns a camera setting from the Osaio device list of
// whichever signed-in login lists the camera.
func deviceListSetting(uuid, key string) (any, bool) {
	reg := currentStreamManager().Registry()
	if reg == nil {
		return nil, false
	}
	if d, _, ok := reg.FindDevice(uuid); ok && d.Config != nil {
		v, found := d.Config[key]
		return v, found
	}
	return nil, false
}

// logDeviceSettings writes the camera's scalar settings (as the device list
// reports them) to the log once per camera, which documents what each model
// supports.
var loggedDeviceSettings sync.Map

func logDeviceSettings(d bridge.Device) {
	if len(d.Config) == 0 {
		return
	}
	if _, seen := loggedDeviceSettings.LoadOrStore(d.UUID, true); seen {
		return
	}
	keys := make([]string, 0, len(d.Config))
	for k, v := range d.Config {
		switch v.(type) {
		case float64, bool:
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%v", k, d.Config[k]))
	}
	fmt.Printf("[devices] %s settings reported by Osaio: %s\n", d.Name, strings.Join(parts, ", "))
}
