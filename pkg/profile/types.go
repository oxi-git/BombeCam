package profile

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

const (
	// CurrentSchemaVersion defines the active profile schema version.
	CurrentSchemaVersion = 1

	// EnvelopeMagicHeader identifies the encrypted container.
	EnvelopeMagicHeader = "BOMBE_ENC_V1"
)

// Profile represents the root configuration and credential state persisted at rest.
type Profile struct {
	Version   int       `json:"version"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	// Privacy is the "Block cloud video" setting as last applied to the router.
	Privacy PrivacySettings `json:"privacy"`
	// Credentials only carries the single login of a profile saved by an
	// older version until Normalize moves it into Accounts; it is never saved.
	Credentials CloudCredentials `json:"-"`
	// Accounts is the pool of stored Osaio logins. Every login is equal; each
	// camera belongs to the login it was added with.
	Accounts   []CloudCredentials       `json:"accounts,omitempty"`
	Cameras    map[string]CameraProfile `json:"cameras"`
	Connection ConnectionParameters     `json:"connection"`
	// RouterKey is BombeCam's private SSH key set for the router (see
	// routerpush.KeySet). On the router it can only run the script's gate
	// command. Never marshalled except into the encrypted profile.
	RouterKey SecretString `json:"-"`
	// Integrations holds the Frigate / Home Assistant settings.
	Integrations IntegrationSettings `json:"integrations"`
	// Users are BombeCam's own sign-ins. Today that is one administrator.
	// The list (with a role per user) leaves room for dependent profiles
	// later, each with its own Osaio logins and cameras; Osaio logins never
	// sign anyone in to BombeCam. Never marshalled except into the
	// encrypted profile.
	Users []UserProfile `json:"-"`
	// Security holds how BombeCam's own sign-in is used.
	Security SecuritySettings `json:"security"`
}

// Roles of BombeCam users.
const (
	RoleAdmin = "admin"
)

// UserProfile is one BombeCam sign-in.
type UserProfile struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Role string `json:"role"`
	// PasswordHash is an encoded Argon2id hash (see auth.HashPassword).
	PasswordHash SecretString `json:"-"`
	// TOTPSecret turns on two-step sign-in with an authenticator app.
	TOTPSecret SecretString `json:"-"`
	// TOTPLastStep is the time step of the last code used, so a code works
	// only once.
	TOTPLastStep int64 `json:"-"`
	// RecoveryCodes are hashes of the unused one-time recovery codes.
	RecoveryCodes []string `json:"-"`
	// PasswordHint is the user's own reminder, shown under "Forgot the
	// password?" on the PC running BombeCam only.
	PasswordHint string    `json:"-"`
	CreatedAt    time.Time `json:"created_at"`
}

// TwoStep reports whether the user signs in with a code as well.
func (u UserProfile) TwoStep() bool { return !u.TOTPSecret.IsEmpty() }

// SecuritySettings are the administrator's sign-in choices.
type SecuritySettings struct {
	// RequireLocalSignIn asks for the sign-in on the PC running BombeCam
	// too. Other devices always have to sign in.
	RequireLocalSignIn bool `json:"require_local_sign_in,omitempty"`
}

// Admin returns the administrator, if one is set up.
func (p *Profile) Admin() (UserProfile, bool) {
	if p == nil {
		return UserProfile{}, false
	}
	for _, u := range p.Users {
		if u.Role == RoleAdmin {
			return u, true
		}
	}
	return UserProfile{}, false
}

// UpdateAdmin applies fn to the administrator; false if there is none.
func (p *Profile) UpdateAdmin(fn func(u *UserProfile)) bool {
	for i := range p.Users {
		if p.Users[i].Role == RoleAdmin {
			fn(&p.Users[i])
			return true
		}
	}
	return false
}

// IntegrationSettings are the "Use with Frigate / Home Assistant" choices.
// Passwords are SecretStrings and only ever written into the encrypted profile.
type IntegrationSettings struct {
	// NVRAddress is the IPv4 address of this PC that NVRs and Home Assistant
	// use to reach it. Empty means automatic (the default-route address).
	NVRAddress string `json:"nvr_address,omitempty"`

	// StreamAuth requires StreamUser/StreamPassword for stream reads (RTSP,
	// HLS, WebRTC) from any address other than this PC.
	StreamAuth     bool         `json:"stream_auth,omitempty"`
	StreamUser     string       `json:"stream_user,omitempty"`
	StreamPassword SecretString `json:"-"`

	// Snapshots serves read-only JPEG snapshots to the network on SnapshotPort.
	Snapshots bool `json:"snapshots,omitempty"`

	// Ports; 0 means the default (RTSP 8554, HLS 8888, WebRTC 8889,
	// WebRTC media 8189/udp, snapshots 8655).
	RTSPPort      int `json:"rtsp_port,omitempty"`
	HLSPort       int `json:"hls_port,omitempty"`
	WebRTCPort    int `json:"webrtc_port,omitempty"`
	WebRTCICEPort int `json:"webrtc_ice_port,omitempty"`
	SnapshotPort  int `json:"snapshot_port,omitempty"`

	MQTT MQTTSettings `json:"mqtt"`
}

// MQTTSettings configures Home Assistant MQTT discovery.
type MQTTSettings struct {
	Enabled  bool         `json:"enabled,omitempty"`
	Host     string       `json:"host,omitempty"`
	Port     int          `json:"port,omitempty"`
	Username string       `json:"username,omitempty"`
	Password SecretString `json:"-"`
	// DiscoveryPrefix is Home Assistant's discovery prefix ("homeassistant").
	DiscoveryPrefix string `json:"discovery_prefix,omitempty"`
}

// PrivacySettings records "Block cloud video": which cameras should be
// blocked, and what the router the cameras connect to was last set to. The
// router's password is never stored. Connecting the router once installs
// BombeCam's own restricted key (Profile.RouterKey) for later changes.
type PrivacySettings struct {
	// Blocked is the per-camera choice (camera ID -> blocked). A camera that
	// is not listed follows BlockNewCameras.
	Blocked map[string]bool `json:"blocked,omitempty"`
	// BlockNewCameras is set by "Block all" and cleared by unblocking all.
	BlockNewCameras bool `json:"block_new_cameras,omitempty"`

	// BlockCloudVideo is nil until the router has been set up once; after
	// that it says whether the router blocks any camera.
	BlockCloudVideo *bool     `json:"block_cloud_video,omitempty"`
	AppliedAt       time.Time `json:"applied_at,omitempty"`
	RouterAddress   string    `json:"router_address,omitempty"`
	// LastRouterAddress is the router BombeCam last connected to. It stays
	// after Disconnect so the next Connect router suggests the same one.
	LastRouterAddress string `json:"last_router_address,omitempty"`
	RouterUser        string `json:"router_user,omitempty"`
	// RouterHostKey is the router's SSH host key fingerprint, pinned the
	// first time BombeCam connects, so a different device cannot pose as it.
	RouterHostKey     string   `json:"router_host_key,omitempty"`
	RouterHostKeyType string   `json:"router_host_key_type,omitempty"` // e.g. "ssh-ed25519"
	RouterFirewall    string   `json:"router_firewall,omitempty"`      // "fw4" or "fw3"
	AppliedCameras    []string `json:"applied_cameras,omitempty"`      // MACs the router blocks
	// RouterConnected: BombeCam's key is installed on the router, so camera
	// changes apply without the password.
	RouterConnected     bool   `json:"router_connected,omitempty"`
	RouterScriptVersion string `json:"router_script_version,omitempty"`
	// BlockStreamSetup mirrors policy.Options.BlockStreamSetup.
	BlockStreamSetup bool `json:"block_stream_setup,omitempty"`
	// Region is the Osaio region the camera account belongs to ("us" or
	// "eu"). The firewall allowlist trusts only that region's servers.
	// Empty means the default (US).
	Region string `json:"region,omitempty"`
	// AppliedRegion is the region the router last applied. It lets BombeCam
	// notice a changed choice and reapply even when the camera list is stable.
	AppliedRegion string `json:"applied_region,omitempty"`
}

// Clone deep-copies the settings.
func (s PrivacySettings) Clone() PrivacySettings {
	cp := s
	if s.BlockCloudVideo != nil {
		v := *s.BlockCloudVideo
		cp.BlockCloudVideo = &v
	}
	if s.AppliedCameras != nil {
		cp.AppliedCameras = append([]string(nil), s.AppliedCameras...)
	}
	if s.Blocked != nil {
		cp.Blocked = make(map[string]bool, len(s.Blocked))
		for k, v := range s.Blocked {
			cp.Blocked[k] = v
		}
	}
	return cp
}

// BoolPtr returns a pointer to b.
func BoolPtr(b bool) *bool { return &b }

// CloudCredentials stores vendor authentication tokens and credentials.
type CloudCredentials struct {
	AccountEmail   string       `json:"account_email"`
	Password       SecretString `json:"password,omitempty"`
	Country        string       `json:"country,omitempty"`
	PhoneCode      string       `json:"phone_code,omitempty"`
	VendorUID      string       `json:"vendor_uid,omitempty"`
	AuthToken      SecretString `json:"auth_token,omitempty"`
	RefreshToken   SecretString `json:"refresh_token,omitempty"`
	TokenExpiresAt time.Time    `json:"token_expires_at,omitempty"`
	Region         string       `json:"region,omitempty"`
}

// CameraProfile stores configuration and metadata for an enrolled camera.
type CameraProfile struct {
	UUID string `json:"uuid"`
	// AccountEmail is the Osaio login that owns this camera (for routing).
	AccountEmail string             `json:"account_email,omitempty"`
	Name         string             `json:"name"`
	Model        string             `json:"model"`
	IPAddress    string             `json:"ip_address,omitempty"`
	MACAddress   string             `json:"mac_address,omitempty"`
	EnrolledAt   time.Time          `json:"enrolled_at"`
	LastSeenAt   time.Time          `json:"last_seen_at,omitempty"`
	Online       bool               `json:"online"`
	TokenCache   SecretString       `json:"token_cache,omitempty"`
	StreamConfig CameraStreamConfig `json:"stream_config,omitempty"`
}

// CameraStreamConfig holds per-camera media stream parameters.
type CameraStreamConfig struct {
	RTSPEnabled bool   `json:"rtsp_enabled"`
	HLSEnabled  bool   `json:"hls_enabled"`
	RTSPPath    string `json:"rtsp_path,omitempty"`
	CustomFPS   int    `json:"custom_fps,omitempty"`
}

// ConnectionParameters stores gateway network and service binding configuration.
type ConnectionParameters struct {
	RTSPBaseURL      string   `json:"rtsp_base_url,omitempty"`
	HLSBaseURL       string   `json:"hls_base_url,omitempty"`
	NetAPIURL        string   `json:"net_api_url,omitempty"`
	ControlTransport string   `json:"control_transport,omitempty"`
	MosquittoURL     string   `json:"mosquitto_url,omitempty"`
	StrictManual     bool     `json:"strict_manual"`
	AllowedSubnets   []string `json:"allowed_subnets,omitempty"`
	Timezone         string   `json:"timezone,omitempty"`
	TimezoneOffset   float64  `json:"timezone_offset,omitempty"`
}

// PublicProfileView is the sanitized, redacted view safe for REST API serialization and logging.
type PublicProfileView struct {
	Version      int                          `json:"version"`
	Privacy      PrivacySettings              `json:"privacy"`
	Accounts     []PublicCredentialsView      `json:"accounts"`
	Cameras      map[string]CameraProfileView `json:"cameras"`
	Connection   ConnectionParameters         `json:"connection"`
	Integrations IntegrationSettings          `json:"integrations"`
	CreatedAt    time.Time                    `json:"created_at"`
	UpdatedAt    time.Time                    `json:"updated_at"`
}

// CameraProfileView is the public view of an enrolled camera.
type CameraProfileView struct {
	UUID         string    `json:"uuid"`
	AccountEmail string    `json:"account_email,omitempty"`
	Name         string    `json:"name"`
	Model        string    `json:"model"`
	StreamName   string    `json:"stream_name,omitempty"`
	IPAddress    string    `json:"ip_address,omitempty"`
	MACAddress   string    `json:"mac_address,omitempty"`
	EnrolledAt   time.Time `json:"enrolled_at"`
	Online       bool      `json:"online"`
}

// PublicCredentialsView is the sanitized view of credentials.
type PublicCredentialsView struct {
	AccountEmail   string     `json:"account_email"`
	VendorUID      string     `json:"vendor_uid,omitempty"`
	Region         string     `json:"region,omitempty"`
	Country        string     `json:"country,omitempty"`
	HasPassword    bool       `json:"has_password"`
	HasToken       bool       `json:"has_token"`
	TokenExpiresAt *time.Time `json:"token_expires_at,omitempty"`
}

// RedactSecrets returns a PublicProfileView with all sensitive credentials stripped.
func (p *Profile) RedactSecrets() *PublicProfileView {
	if p == nil {
		return nil
	}
	if p.Credentials.AccountEmail != "" {
		p = p.Clone() // an older profile: show its login in the pool
		p.Normalize()
	}
	camerasView := make(map[string]CameraProfileView, len(p.Cameras))
	for k, c := range p.Cameras {
		camerasView[k] = CameraProfileView{
			UUID:         c.UUID,
			AccountEmail: c.AccountEmail,
			Name:         c.Name,
			Model:        c.Model,
			StreamName:   c.StreamConfig.RTSPPath,
			IPAddress:    c.IPAddress,
			MACAddress:   c.MACAddress,
			EnrolledAt:   c.EnrolledAt,
			Online:       c.Online,
		}
	}
	accountsView := make([]PublicCredentialsView, 0, len(p.Accounts))
	for _, a := range p.Accounts {
		accountsView = append(accountsView, a.RedactSecrets())
	}
	return &PublicProfileView{
		Version:      p.Version,
		Privacy:      p.Privacy.Clone(),
		Accounts:     accountsView,
		Cameras:      camerasView,
		Connection:   p.Connection,
		Integrations: p.Integrations,
		CreatedAt:    p.CreatedAt,
		UpdatedAt:    p.UpdatedAt,
	}
}

// GetPublicView is an alias for RedactSecrets.
func (p *Profile) GetPublicView() *PublicProfileView {
	return p.RedactSecrets()
}

// Clone creates a deep copy of Profile, isolating mutable maps and slices.
func (p *Profile) Clone() *Profile {
	if p == nil {
		return nil
	}
	cp := *p
	cp.Privacy = p.Privacy.Clone()
	if p.Accounts != nil {
		cp.Accounts = make([]CloudCredentials, len(p.Accounts))
		copy(cp.Accounts, p.Accounts)
	}
	if p.Cameras != nil {
		cp.Cameras = make(map[string]CameraProfile, len(p.Cameras))
		for k, v := range p.Cameras {
			cp.Cameras[k] = v
		}
	}
	if p.Users != nil {
		cp.Users = make([]UserProfile, len(p.Users))
		for i, u := range p.Users {
			u.RecoveryCodes = append([]string(nil), u.RecoveryCodes...)
			cp.Users[i] = u
		}
	}
	if p.Connection.AllowedSubnets != nil {
		cp.Connection.AllowedSubnets = make([]string, len(p.Connection.AllowedSubnets))
		copy(cp.Connection.AllowedSubnets, p.Connection.AllowedSubnets)
	}
	return &cp
}

// MarshalJSON ensures any direct json.Marshal of a Profile produces the PublicProfileView.
func (p Profile) MarshalJSON() ([]byte, error) {
	return json.Marshal(p.RedactSecrets())
}

// String implements fmt.Stringer to ensure %v or %+v never prints sensitive credentials.
func (p Profile) String() string {
	block := "unset"
	if p.Privacy.BlockCloudVideo != nil {
		block = fmt.Sprint(*p.Privacy.BlockCloudVideo)
	}
	logins := make([]string, 0, len(p.Accounts))
	for _, a := range p.Accounts {
		logins = append(logins, a.String())
	}
	return fmt.Sprintf("Profile{Version: %d, BlockCloudVideo: %s, Cameras: %d, Logins: [%s]}",
		p.Version, block, len(p.Cameras), strings.Join(logins, ", "))
}

// GoString implements fmt.GoStringer to ensure %#v never prints sensitive credentials.
func (p Profile) GoString() string {
	return p.String()
}

// LogValue implements slog.LogValuer for structured logging.
func (p Profile) LogValue() slog.Value {
	logins := make([]slog.Attr, 0, len(p.Accounts))
	for i, a := range p.Accounts {
		logins = append(logins, slog.Any(fmt.Sprint(i), a.LogValue()))
	}
	return slog.GroupValue(
		slog.Int("version", p.Version),
		slog.Bool("block_cloud_video", p.Privacy.BlockCloudVideo != nil && *p.Privacy.BlockCloudVideo),
		slog.Int("camera_count", len(p.Cameras)),
		slog.Attr{Key: "logins", Value: slog.GroupValue(logins...)},
	)
}

// RedactSecrets returns PublicCredentialsView for CloudCredentials.
func (c CloudCredentials) RedactSecrets() PublicCredentialsView {
	var expiresAt *time.Time
	if !c.TokenExpiresAt.IsZero() {
		exp := c.TokenExpiresAt
		expiresAt = &exp
	}
	return PublicCredentialsView{
		AccountEmail:   c.AccountEmail,
		VendorUID:      c.VendorUID,
		Region:         c.Region,
		Country:        c.Country,
		HasPassword:    !c.Password.IsEmpty(),
		HasToken:       !c.AuthToken.IsEmpty(),
		TokenExpiresAt: expiresAt,
	}
}

// MarshalJSON ensures direct json.Marshal of CloudCredentials produces PublicCredentialsView.
func (c CloudCredentials) MarshalJSON() ([]byte, error) {
	return json.Marshal(c.RedactSecrets())
}

// String implements fmt.Stringer on CloudCredentials.
func (c CloudCredentials) String() string {
	return fmt.Sprintf("CloudCredentials{AccountEmail: %q, VendorUID: %q, Region: %q, Password: %s, AuthToken: %s}",
		c.AccountEmail, c.VendorUID, c.Region, RedactedPlaceholder, RedactedPlaceholder)
}

// GoString implements fmt.GoStringer on CloudCredentials.
func (c CloudCredentials) GoString() string {
	return c.String()
}

// LogValue implements slog.LogValuer on CloudCredentials.
func (c CloudCredentials) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("account_email", c.AccountEmail),
		slog.String("vendor_uid", c.VendorUID),
		slog.String("region", c.Region),
		slog.String("password", RedactedPlaceholder),
		slog.String("auth_token", RedactedPlaceholder),
	)
}
