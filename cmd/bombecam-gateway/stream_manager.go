package main

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Fever-r/BombeCam/pkg/bridge"
	"github.com/Fever-r/BombeCam/pkg/netstack"
	"github.com/Fever-r/BombeCam/pkg/profile"
)

type SessionState string

const (
	SessionStateActive     SessionState = "ACTIVE"
	SessionStateRecovering SessionState = "RECOVERING"
	SessionStateLockout    SessionState = "LOCKOUT"
)

// CameraSession tracks recovery state, budget, and strict manual mode per camera.
type CameraSession struct {
	mu             sync.Mutex
	UUID           string
	Name           string
	State          SessionState
	RecoveryBudget int
	StrictManual   bool
	unlockCh       chan struct{}
}

// NewCameraSession creates a new CameraSession with initial 3 retry attempts.
func NewCameraSession(uuid, name string, strictManual bool) *CameraSession {
	return &CameraSession{
		UUID:           uuid,
		Name:           name,
		State:          SessionStateActive,
		RecoveryBudget: 3,
		StrictManual:   strictManual,
		unlockCh:       make(chan struct{}, 1),
	}
}

// RecordFailure decrements retry budget and engages LOCKOUT when exhausted.
func (s *CameraSession) RecordFailure() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.RecoveryBudget--
	if s.RecoveryBudget <= 0 {
		s.State = SessionStateLockout
		s.RecoveryBudget = 0
		return false // locked out
	}
	s.State = SessionStateRecovering
	return true // can retry
}

// RecordSuccess resets budget to 3 and restores ACTIVE state.
func (s *CameraSession) RecordSuccess() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.State = SessionStateActive
	s.RecoveryBudget = 3
}

// Unlock clears lockout and resets budget to 3.
func (s *CameraSession) Unlock() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.RecoveryBudget = 3
	s.State = SessionStateActive
	select {
	case s.unlockCh <- struct{}{}:
	default:
	}
}

// WaitUnlock blocks until an unlock signal is received.
func (s *CameraSession) WaitUnlock() {
	<-s.unlockCh
}

// WaitUnlockContext blocks until unlock signal is received or context is cancelled.
func (s *CameraSession) WaitUnlockContext(ctx context.Context) error {
	select {
	case <-s.unlockCh:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// SetStrictManual toggles strict manual mode.
func (s *CameraSession) SetStrictManual(manual bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.StrictManual = manual
}

// IsStrictManual returns whether strict manual mode is active.
func (s *CameraSession) IsStrictManual() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.StrictManual
}

// CameraStatusResponse represents the JSON response for GET /api/v1/cameras/{id}/status.
type CameraStatusResponse struct {
	ID               string `json:"id"`
	Name             string `json:"name"`
	Streaming        bool   `json:"streaming"`
	StreamURL        string `json:"stream_url"`
	HlsURL           string `json:"hls_url"`
	Transport        string `json:"transport"`
	ObservedIR       string `json:"observed_ir"`
	ObservedLED      string `json:"observed_led"`
	RecoveryState    string `json:"recovery_state"`
	RecoveryAttempts int    `json:"recovery_attempts"`
	StrictManual     bool   `json:"strict_manual"`
	FreshnessTS      string `json:"freshness_ts"`
}

// ManagedCamera maintains the persistent runtime state of an enrolled camera.
type ManagedCamera struct {
	mu               sync.RWMutex
	UUID             string
	Name             string
	Model            string
	IP               string
	RTSPBase         string // Internal publish RTSP base (backward compatibility)
	PublishRTSPBase  string
	ConsumerRTSPBase string
	HLSBase          string
	ConsumerHLSBase  string
	RTPPort          int
	Session          *CameraSession
	CancelStream     context.CancelFunc
	Viewer           *bridge.Viewer
	Signaling        *bridge.Signaling
	Streaming        bool
	Transport        string
	ObservedIR       string
	ObservedLED      string
	ObservedLight    string
	ObservedIRAt     time.Time
	ObservedLEDAt    time.Time
	ObservedLightAt  time.Time
	Freshness        time.Time
	sm               *StreamManager // for advertised URLs (nil in some tests)
}

// GetStatus computes the current CameraStatusResponse.
func (mc *ManagedCamera) GetStatus(cc bridge.ControlChannel, optReq ...*http.Request) CameraStatusResponse {
	var req *http.Request
	if len(optReq) > 0 {
		req = optReq[0]
	}
	// Resolve the URLs before taking mc.mu: they read the StreamManager's
	// lock, which elsewhere is taken before mc.mu.
	var streamURL, hlsURL string
	if mc.sm != nil {
		streamURL, hlsURL = mc.resolveConsumerRTSPURL(req), mc.resolveConsumerHLSURL(req)
	}

	mc.mu.RLock()
	defer mc.mu.RUnlock()
	if mc.sm == nil {
		streamURL, hlsURL = mc.resolveConsumerRTSPURL(req), mc.resolveConsumerHLSURL(req)
	}

	mc.Session.mu.Lock()
	recoveryState := string(mc.Session.State)
	recoveryAttempts := mc.Session.RecoveryBudget
	strictManual := mc.Session.StrictManual
	mc.Session.mu.Unlock()

	transport := mc.Transport
	if transport == "" {
		transport = "unknown"
	}

	localMode := isLocalControl(cc)
	ir := mc.ObservedIR
	if localMode || mc.ObservedIRAt.IsZero() || time.Since(mc.ObservedIRAt) > cameraObservationMaxAge {
		ir = "unknown"
	}
	if ir == "" {
		ir = "unknown"
	}
	led := mc.ObservedLED
	if localMode || mc.ObservedLEDAt.IsZero() || time.Since(mc.ObservedLEDAt) > cameraObservationMaxAge {
		led = "unknown"
	}
	if led == "" {
		led = "unknown"
	}

	// Query fresh cached status from ControlChannel if active
	if cc != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		if mode, err := cc.GetIR(ctx, mc.UUID); err == nil {
			ir = bridge.IRModeName(mode)
		}
		if on, err := cc.GetLED(ctx, mc.UUID); err == nil {
			if on {
				led = "on"
			} else {
				led = "off"
			}
		}
		cancel()
	}

	freshness := ""
	if !mc.Freshness.IsZero() {
		freshness = mc.Freshness.UTC().Format(time.RFC3339)
	}
	return CameraStatusResponse{
		ID:               mc.UUID,
		Name:             mc.Name,
		Streaming:        mc.Streaming,
		StreamURL:        streamURL,
		HlsURL:           hlsURL,
		Transport:        transport,
		ObservedIR:       ir,
		ObservedLED:      led,
		RecoveryState:    recoveryState,
		RecoveryAttempts: recoveryAttempts,
		StrictManual:     strictManual,
		FreshnessTS:      freshness,
	}
}

func (mc *ManagedCamera) resolveConsumerRTSPURL(r *http.Request) string {
	if mc.sm != nil {
		return mc.sm.ConsumerRTSPURL(mc.UUID, r)
	}
	if mc.ConsumerRTSPBase != "" {
		return fmt.Sprintf("%s/%s", strings.TrimRight(mc.ConsumerRTSPBase, "/"), mc.UUID)
	}
	base := mc.PublishRTSPBase
	if base == "" {
		base = mc.RTSPBase
	}
	derived := deriveConsumerBase(r, base, "rtsp", portOfURL(base, 8554))
	return fmt.Sprintf("%s/%s", derived, mc.UUID)
}

func (mc *ManagedCamera) resolveConsumerHLSURL(r *http.Request) string {
	if mc.sm != nil {
		return mc.sm.ConsumerHLSURL(mc.UUID, r)
	}
	if mc.ConsumerHLSBase != "" {
		return fmt.Sprintf("%s/%s/index.m3u8", strings.TrimRight(mc.ConsumerHLSBase, "/"), mc.UUID)
	}
	derived := deriveConsumerBase(r, mc.HLSBase, "http", portOfURL(mc.HLSBase, 8888))
	return fmt.Sprintf("%s/%s/index.m3u8", derived, mc.UUID)
}

// UpdateObservedIR updates cached IR observation.
// LANIP returns the camera's LAN address as last learned (under mc.mu).
func (mc *ManagedCamera) LANIP() string {
	mc.mu.RLock()
	defer mc.mu.RUnlock()
	return mc.IP
}

func (mc *ManagedCamera) UpdateObservedIR(mode string) {
	mc.mu.Lock()
	defer mc.mu.Unlock()
	mc.ObservedIR = mode
	mc.ObservedIRAt = time.Now().UTC()
	mc.Freshness = time.Now().UTC()
}

// UpdateObservedLED updates cached LED observation.
func (mc *ManagedCamera) UpdateObservedLED(mode string) {
	mc.mu.Lock()
	defer mc.mu.Unlock()
	mc.ObservedLED = mode
	mc.ObservedLEDAt = time.Now().UTC()
	mc.Freshness = time.Now().UTC()
}

// UpdateObservedLight updates cached spotlight observation.
func (mc *ManagedCamera) UpdateObservedLight(mode string) {
	mc.mu.Lock()
	defer mc.mu.Unlock()
	mc.ObservedLight = mode
	mc.ObservedLightAt = time.Now().UTC()
	mc.Freshness = time.Now().UTC()
}

// GetRTPPort returns the assigned RTP ingest port.
func (mc *ManagedCamera) GetRTPPort() int {
	mc.mu.RLock()
	defer mc.mu.RUnlock()
	return mc.RTPPort
}

// CameraRunner defines the function signature for running a camera's media stream.
type CameraRunner func(ctx context.Context, mc *ManagedCamera, rtpPort int)

// StreamManager coordinates camera enrollment, stream lifecycle, and persistent camera state.
type StreamManager struct {
	mu               sync.RWMutex
	cameras          map[string]*ManagedCamera
	rtspBase         string // Legacy accessor compatibility
	publishRTSPBase  string
	consumerRTSPBase string
	hlsBase          string
	consumerHLSBase  string
	webrtcBase       string
	cameraIPs        []string
	allowedSubnets   []*net.IPNet
	strictManual     bool
	// localControlTest makes a stream end in lockout and closes signaling once
	// media flows, so the local firewall path can be exercised without a real
	// outage. Set once at startup by -local-control-test.
	localControlTest bool
	// profileManager is where camera credentials and settings live. Set once
	// at startup; handlers read it instead of reaching for a global.
	profileManager profile.ProfileManager
	ffmpegPath     string
	cloud          *bridge.Cloud
	cloudResolver  func(uuid string) *bridge.Cloud // per-camera owning-account cloud; nil -> use cloud
	loginRenewer   func(old *bridge.Cloud) *bridge.Cloud
	runner         CameraRunner
	portBase       int
	publisher      string
	retryMaxWait   time.Duration

	// Advertised addresses for NVRs / Home Assistant (see lan_address.go).
	nvrAddress  string            // chosen LAN address ("" = automatic)
	streamUser  string            // stream password on: embedded in advertised URLs
	streamPass  string            // (never logged)
	streamNames map[string]string // camera ID -> friendly stream path
}

// SetPublisher selects how camera media is published (bridge.PublisherFFmpeg or bridge.PublisherNative).
func (sm *StreamManager) SetPublisher(mode string) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	sm.publisher = mode
}

// Publisher returns the configured publisher mode.
func (sm *StreamManager) Publisher() string {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	if sm.publisher == "" {
		if sm.ffmpegPath == "" {
			return bridge.PublisherNative
		}
		return bridge.PublisherFFmpeg
	}
	return sm.publisher
}

// SetAllowedSubnets configures allowed subnets for candidate filtering across managed streams.
func (sm *StreamManager) SetAllowedSubnets(subnets []*net.IPNet) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	sm.allowedSubnets = make([]*net.IPNet, len(subnets))
	copy(sm.allowedSubnets, subnets)
}

// SetAllowedSubnet parses and configures a single allowed subnet.
func (sm *StreamManager) SetAllowedSubnet(cidr string) error {
	_, ipNet, err := net.ParseCIDR(cidr)
	if err != nil {
		return err
	}
	sm.SetAllowedSubnets([]*net.IPNet{ipNet})
	return nil
}

// SetLocalControlTest enables the local-firewall test mode: a stream that ends
// goes straight to lockout instead of retrying, and signaling is closed once
// media flows.
func (sm *StreamManager) SetLocalControlTest(on bool) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	sm.localControlTest = on
}

// LocalControlTest reports whether test mode is on.
func (sm *StreamManager) LocalControlTest() bool {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	return sm.localControlTest
}

// SetProfileManager is the profile store handlers and the stream loop read
// credentials and settings from.
func (sm *StreamManager) SetProfileManager(pm profile.ProfileManager) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	sm.profileManager = pm
}

// ProfileManager returns the profile store in use, or nil before startup has
// installed one.
func (sm *StreamManager) ProfileManager() profile.ProfileManager {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	return sm.profileManager
}

// AllowedSubnets returns a copy of configured allowed subnets.
func (sm *StreamManager) AllowedSubnets() []*net.IPNet {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	res := make([]*net.IPNet, len(sm.allowedSubnets))
	copy(res, sm.allowedSubnets)
	return res
}

// NewStreamManager creates a StreamManager.
func NewStreamManager(cloud *bridge.Cloud, rtspBase, hlsBase string, cameraIPs []string, strictManual bool, ffmpegPath string) *StreamManager {
	sm := &StreamManager{
		cameras:         make(map[string]*ManagedCamera),
		rtspBase:        rtspBase,
		publishRTSPBase: rtspBase,
		hlsBase:         hlsBase,
		cameraIPs:       cameraIPs,
		strictManual:    strictManual,
		ffmpegPath:      ffmpegPath,
		cloud:           cloud,
		portBase:        5000,
	}
	sm.runner = sm.defaultRunner
	return sm
}

// SetPublishRTSPBase sets the internal MediaMTX RTSP publishing target.
func (sm *StreamManager) SetPublishRTSPBase(base string) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	sm.publishRTSPBase = base
	sm.rtspBase = base
}

func (sm *StreamManager) publishRTSPBaseLocked() string {
	if sm.publishRTSPBase != "" {
		return sm.publishRTSPBase
	}
	if sm.rtspBase != "" {
		return sm.rtspBase
	}
	return "rtsp://127.0.0.1:8554"
}

// PublishRTSPBase returns the internal MediaMTX publishing base URL.
func (sm *StreamManager) PublishRTSPBase() string {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	return sm.publishRTSPBaseLocked()
}

// SetConsumerRTSPBase sets the explicit external consumer RTSP base URL.
func (sm *StreamManager) SetConsumerRTSPBase(base string) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	sm.consumerRTSPBase = base
}

// ConsumerRTSPBase returns the configured consumer RTSP base URL (or empty if auto-derived).
func (sm *StreamManager) ConsumerRTSPBase() string {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	return sm.consumerRTSPBase
}

// SetConsumerHLSBase sets the explicit external consumer HLS base URL.
func (sm *StreamManager) SetConsumerHLSBase(base string) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	sm.consumerHLSBase = base
}

// ConsumerHLSBase returns the configured consumer HLS base URL (or empty if auto-derived).
func (sm *StreamManager) ConsumerHLSBase() string {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	return sm.consumerHLSBase
}

// SetNVRAddress sets the LAN address advertised to NVRs and Home Assistant
// ("" = automatic).
func (sm *StreamManager) SetNVRAddress(ip string) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	sm.nvrAddress = strings.TrimSpace(ip)
}

// NVRAddress returns the chosen advertised LAN address ("" = automatic).
func (sm *StreamManager) NVRAddress() string {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	return sm.nvrAddress
}

// SetStreamCredentials sets the user and password that advertised stream
// URLs carry when the stream password is on (empty user = off).
func (sm *StreamManager) SetStreamCredentials(user, pass string) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	sm.streamUser, sm.streamPass = user, pass
}

// StreamCredentials returns the stream user and password ("" when off).
func (sm *StreamManager) StreamCredentials() (string, string) {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	return sm.streamUser, sm.streamPass
}

// AdvertisedHost is the host other devices should use for this PC's streams,
// where it came from (addrFrom*), and whether a chosen address is missing.
func (sm *StreamManager) AdvertisedHost(r *http.Request) (host, source string, chosenMissing bool) {
	return resolveAdvertisedHost(r, sm.NVRAddress())
}

// RTSPPort is the port MediaMTX serves RTSP on (from the publish base).
func (sm *StreamManager) RTSPPort() int {
	return portOfURL(sm.PublishRTSPBase(), 8554)
}

// HLSPort is the port MediaMTX serves HLS on.
func (sm *StreamManager) HLSPort() int {
	return portOfURL(sm.HLSBase(), 8888)
}

// portOfURL returns the explicit port of a URL, else def.
func portOfURL(raw string, def int) int {
	if raw == "" {
		return def
	}
	u, err := url.Parse(raw)
	if err != nil {
		return def
	}
	if p, err := strconv.Atoi(u.Port()); err == nil && p > 0 {
		return p
	}
	return def
}

// withUserInfo puts the stream user and password into a base URL.
func (sm *StreamManager) withUserInfo(base string) string {
	user, pass := sm.StreamCredentials()
	if user == "" {
		return base
	}
	u, err := url.Parse(base)
	if err != nil || u.Host == "" {
		return base
	}
	u.User = url.UserPassword(user, pass)
	return strings.TrimRight(u.String(), "/")
}

// ConsumerRTSPBaseURL resolves the RTSP base URL other devices use:
// BOMBECAM_RTSP_CONSUMER_BASE if set, else this PC's advertised LAN address
// (see resolveAdvertisedHost), with the stream password when that is on.
func (sm *StreamManager) ConsumerRTSPBaseURL(r *http.Request) string {
	sm.mu.RLock()
	consumerBase := sm.consumerRTSPBase
	sm.mu.RUnlock()
	if consumerBase != "" {
		return sm.withUserInfo(strings.TrimRight(consumerBase, "/"))
	}
	host, _, _ := sm.AdvertisedHost(r)
	return sm.withUserInfo(fmt.Sprintf("rtsp://%s:%d", hostForURL(host), sm.RTSPPort()))
}

// ConsumerHLSBaseURL resolves the HLS base URL other devices use.
func (sm *StreamManager) ConsumerHLSBaseURL(r *http.Request) string {
	sm.mu.RLock()
	consumerBase := sm.consumerHLSBase
	sm.mu.RUnlock()
	if consumerBase != "" {
		return sm.withUserInfo(strings.TrimRight(consumerBase, "/"))
	}
	host, _, _ := sm.AdvertisedHost(r)
	return sm.withUserInfo(fmt.Sprintf("http://%s:%d", hostForURL(host), sm.HLSPort()))
}

// SetStreamNames replaces the camera ID -> friendly stream path map.
func (sm *StreamManager) SetStreamNames(names map[string]string) {
	cp := make(map[string]string, len(names))
	for k, v := range names {
		cp[k] = v
	}
	sm.mu.Lock()
	defer sm.mu.Unlock()
	sm.streamNames = cp
}

// StreamName returns a camera's friendly stream path ("" if none yet).
func (sm *StreamManager) StreamName(uuid string) string {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	return sm.streamNames[uuid]
}

// StreamNames returns a copy of the camera ID -> friendly stream path map.
func (sm *StreamManager) StreamNames() map[string]string {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	cp := make(map[string]string, len(sm.streamNames))
	for k, v := range sm.streamNames {
		cp[k] = v
	}
	return cp
}

// streamPath is the path advertised for a camera: its friendly name, or its
// ID (which always keeps working).
func (sm *StreamManager) streamPath(uuid string) string {
	if n := sm.StreamName(uuid); n != "" {
		return n
	}
	return uuid
}

// ConsumerRTSPURL is the RTSP URL other devices use for a camera.
func (sm *StreamManager) ConsumerRTSPURL(uuid string, r *http.Request) string {
	return fmt.Sprintf("%s/%s", sm.ConsumerRTSPBaseURL(r), sm.streamPath(uuid))
}

// ConsumerRTSPURLByID is the camera-ID form of the RTSP URL, which keeps
// working alongside the named form.
func (sm *StreamManager) ConsumerRTSPURLByID(uuid string, r *http.Request) string {
	return fmt.Sprintf("%s/%s", sm.ConsumerRTSPBaseURL(r), uuid)
}

// ConsumerHLSURL is the HLS URL other devices use for a camera.
func (sm *StreamManager) ConsumerHLSURL(uuid string, r *http.Request) string {
	return fmt.Sprintf("%s/%s/index.m3u8", sm.ConsumerHLSBaseURL(r), sm.streamPath(uuid))
}

// ViewerHLSURL is the HLS URL for BombeCam's own viewer in the browser: the
// gateway relays it, so it is same-origin and MediaMTX's HLS server can stay
// closed to other web pages.
func (sm *StreamManager) ViewerHLSURL(uuid string) string {
	return "/api/v1/cameras/" + url.PathEscape(uuid) + "/hls/index.m3u8"
}

func deriveConsumerBase(r *http.Request, defaultBase, defaultScheme string, defaultPort int) string {
	// 1. If incoming request has an authentic host (non-empty and not synthetic test host)
	if r != nil && r.Host != "" && !isSyntheticTestHost(r.Host) {
		host := r.Host
		hostname, _, err := net.SplitHostPort(host)
		if err != nil {
			hostname = host
		}
		if hostname != "" {
			// Enclose IPv6 literals in brackets if needed
			if strings.Contains(hostname, ":") && !strings.HasPrefix(hostname, "[") {
				hostname = "[" + hostname + "]"
			}
			return fmt.Sprintf("%s://%s:%d", defaultScheme, hostname, defaultPort)
		}
	}

	// 2. If defaultBase is set and does NOT contain internal container hostnames (e.g. "mediamtx")
	if defaultBase != "" && !strings.Contains(defaultBase, "mediamtx") {
		return strings.TrimRight(defaultBase, "/")
	}

	// 3. Fallback to loopback interface
	return fmt.Sprintf("%s://127.0.0.1:%d", defaultScheme, defaultPort)
}

func isSyntheticTestHost(host string) bool {
	h := strings.ToLower(strings.TrimSpace(host))
	if stripped, _, err := net.SplitHostPort(h); err == nil {
		h = stripped
	}
	return h == "example.com"
}

// SetRunner overrides the camera runner (e.g. for unit testing).
func (sm *StreamManager) SetRunner(runner CameraRunner) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	sm.runner = runner
}

// SetLoginRenewer installs the function that signs in again when a stream
// start fails because the Osaio sign-in expired (see session_renew.go).
func (sm *StreamManager) SetLoginRenewer(fn func(old *bridge.Cloud) *bridge.Cloud) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	sm.loginRenewer = fn
}

// renewLogin decides whether err means the sign-in expired and, if so,
// returns a newly signed-in cloud (nil otherwise).
func (sm *StreamManager) renewLogin(c *bridge.Cloud, err error) *bridge.Cloud {
	sm.mu.RLock()
	fn := sm.loginRenewer
	sm.mu.RUnlock()
	if fn == nil || c == nil || !loginLooksExpired(c, err) {
		return nil
	}
	return fn(c)
}

// loginLooksExpired: Osaio answered "unauthorized", or refused the stream
// and also refuses the (harmless) camera list with the same sign-in. A
// camera that is merely offline still gets its list.
func loginLooksExpired(c *bridge.Cloud, err error) bool {
	if errors.Is(err, bridge.ErrInvalidCredentials) {
		return true
	}
	if !strings.Contains(err.Error(), "code=") {
		return false
	}
	_, lerr := c.DeviceList()
	return lerr != nil && (errors.Is(lerr, bridge.ErrInvalidCredentials) || strings.Contains(lerr.Error(), "code="))
}

// SetCloudResolver installs a per-camera cloud resolver. When set, the stream
// loop uses the cloud of the Osaio login that owns each camera, so cameras from
// different logins stream through the correct session. A nil return means the
// camera's login is not signed in yet: the camera waits for it.
func (sm *StreamManager) SetCloudResolver(fn func(uuid string) *bridge.Cloud) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	sm.cloudResolver = fn
}

// cloudForUUID returns the cloud of the login that owns a camera (nil while
// it is not signed in). Without a resolver every camera uses the one cloud.
func (sm *StreamManager) cloudForUUID(uuid string) *bridge.Cloud {
	sm.mu.RLock()
	resolver := sm.cloudResolver
	def := sm.cloud
	sm.mu.RUnlock()
	if resolver != nil {
		return resolver(uuid)
	}
	return def
}

// SetCloud updates the cloud reference if initialized after StreamManager creation.
func (sm *StreamManager) SetCloud(c *bridge.Cloud) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	sm.cloud = c
}

// Enroll authoritatively enrolls camera IDs, starting streams for newly enrolled cameras
// and stopping streams for unenrolled cameras. All IDs must exist in discovered inventory.
func (sm *StreamManager) Enroll(cameraIDs []string, inventory []bridge.Device) ([]string, error) {
	if len(cameraIDs) == 0 {
		return nil, fmt.Errorf("no camera IDs provided: camera_ids array must not be empty")
	}

	sm.mu.Lock()
	defer sm.mu.Unlock()

	// 1. Validate that all requested camera IDs exist in account inventory
	invMap := make(map[string]bridge.Device)
	for _, d := range inventory {
		invMap[d.UUID] = d
	}

	for _, id := range cameraIDs {
		if _, exists := invMap[id]; !exists {
			return nil, fmt.Errorf("camera ID '%s' not found in authenticated account inventory", id)
		}
	}

	desired := make(map[string]bool)
	for _, id := range cameraIDs {
		desired[id] = true
	}

	// 2. Stop and remove cameras no longer in desired enrollment
	for uuid, mc := range sm.cameras {
		if !desired[uuid] {
			if mc.CancelStream != nil {
				mc.CancelStream()
			}
			delete(sm.cameras, uuid)
		}
	}

	// 3. Start newly enrolled cameras with dynamic port reservation
	allocatedPorts := make(map[int]bool, len(sm.cameras))
	for _, mc := range sm.cameras {
		port := mc.GetRTPPort()
		if port > 0 {
			allocatedPorts[port] = true
		}
	}

	basePort := sm.portBase
	if basePort <= 0 {
		basePort = 5000
	}
	if basePort%2 != 0 {
		basePort++
	}

	idx := 0
	for _, id := range cameraIDs {
		mc, alreadyEnrolled := sm.cameras[id]
		if !alreadyEnrolled {
			dev := invMap[id]
			camIP := ""
			if idx < len(sm.cameraIPs) && strings.TrimSpace(sm.cameraIPs[idx]) != "" {
				camIP = strings.TrimSpace(sm.cameraIPs[idx])
			}

			// Allocate lowest available even port >= basePort
			rtpPort := basePort
			for allocatedPorts[rtpPort] {
				rtpPort += 2
				if rtpPort > 65534 {
					return nil, fmt.Errorf("exhausted available RTP port range: no available even ports <= 65534")
				}
			}
			allocatedPorts[rtpPort] = true

			ctx, cancel := context.WithCancel(context.Background())
			session := NewCameraSession(dev.UUID, dev.Name, sm.strictManual)

			publishBase := sm.publishRTSPBaseLocked()
			consumerRTSP := sm.consumerRTSPBase
			consumerHLS := sm.consumerHLSBase
			mc = &ManagedCamera{
				UUID:             dev.UUID,
				Name:             dev.Name,
				Model:            dev.Type,
				IP:               camIP,
				RTSPBase:         publishBase,
				PublishRTSPBase:  publishBase,
				ConsumerRTSPBase: consumerRTSP,
				HLSBase:          sm.hlsBase,
				ConsumerHLSBase:  consumerHLS,
				RTPPort:          rtpPort,
				Session:          session,
				CancelStream:     cancel,
				Freshness:        time.Now().UTC(),
				sm:               sm,
			}
			sm.cameras[id] = mc

			if sm.runner != nil {
				go sm.runner(ctx, mc, rtpPort)
			}
		} else if mc.CancelStream == nil {
			dev := invMap[id]
			ctx, cancel := context.WithCancel(context.Background())
			session := NewCameraSession(dev.UUID, dev.Name, sm.strictManual)
			mc.Session = session
			mc.CancelStream = cancel
			mc.Freshness = time.Now().UTC()
			if sm.runner != nil {
				go sm.runner(ctx, mc, mc.RTPPort)
			}
		}
		idx++
	}

	enrolled := make([]string, 0, len(sm.cameras))
	for id := range sm.cameras {
		enrolled = append(enrolled, id)
	}
	return enrolled, nil
}

// GetCamera retrieves an enrolled camera by exact UUID.
func (sm *StreamManager) GetCamera(uuid string) (*ManagedCamera, bool) {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	mc, ok := sm.cameras[uuid]
	return mc, ok
}

// cameraSessionByName resolves a camera the way the legacy routes name it: by
// UUID or by name, exactly or as a fragment. Returns nil when no enrolled
// camera matches.
func cameraSessionByName(streamMgr *StreamManager, ref string) *CameraSession {
	if streamMgr == nil {
		return nil
	}
	mc, ok := streamMgr.ResolveCamera(ref)
	if !ok {
		return nil
	}
	mc.mu.RLock()
	defer mc.mu.RUnlock()
	return mc.Session
}

// ResolveCamera finds an enrolled camera by exact UUID, then by name, then by
// a case-insensitive fragment of either. It is the only way a request names a
// camera: the legacy routes took a `cam` query parameter that was a UUID or a
// name, and callers should not each reproduce the matching rules.
func (sm *StreamManager) ResolveCamera(ref string) (*ManagedCamera, bool) {
	if ref == "" {
		return nil, false
	}
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	if mc, ok := sm.cameras[ref]; ok {
		return mc, true
	}
	for _, mc := range sm.cameras {
		if strings.EqualFold(mc.Name, ref) {
			return mc, true
		}
	}
	lower := strings.ToLower(ref)
	for uuid, mc := range sm.cameras {
		if strings.Contains(strings.ToLower(uuid), lower) || strings.Contains(strings.ToLower(mc.Name), lower) {
			return mc, true
		}
	}
	for uuid, streamName := range sm.streamNames {
		if strings.EqualFold(streamName, ref) {
			if mc, ok := sm.cameras[uuid]; ok {
				return mc, true
			}
		}
	}
	return nil, false
}

// CameraSession returns the session of an enrolled camera, or nil when the
// camera is not enrolled or has none.
func (sm *StreamManager) CameraSession(uuid string) *CameraSession {
	mc, ok := sm.GetCamera(uuid)
	if !ok || mc == nil {
		return nil
	}
	mc.mu.RLock()
	defer mc.mu.RUnlock()
	return mc.Session
}

// Sessions returns every enrolled camera's session, skipping cameras without one.
func (sm *StreamManager) Sessions() []*CameraSession {
	cams := sm.GetAllCameras()
	out := make([]*CameraSession, 0, len(cams))
	for _, mc := range cams {
		mc.mu.RLock()
		sess := mc.Session
		mc.mu.RUnlock()
		if sess != nil {
			out = append(out, sess)
		}
	}
	return out
}

// GetAllCameras returns a snapshot slice of all currently enrolled cameras.
func (sm *StreamManager) GetAllCameras() []*ManagedCamera {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	res := make([]*ManagedCamera, 0, len(sm.cameras))
	for _, mc := range sm.cameras {
		res = append(res, mc)
	}
	return res
}

// ActiveStreamCount returns the count of cameras actively streaming.
func (sm *StreamManager) ActiveStreamCount() int {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	count := 0
	for _, mc := range sm.cameras {
		mc.mu.RLock()
		if mc.Streaming {
			count++
		}
		mc.mu.RUnlock()
	}
	return count
}

// CameraCount returns the total count of enrolled cameras.
func (sm *StreamManager) CameraCount() int {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	return len(sm.cameras)
}

// PortBase returns the base port configured for RTP stream allocation.
func (sm *StreamManager) PortBase() int {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	return sm.portBase
}

// RTSPBase returns the configured RTSP base URL.
func (sm *StreamManager) RTSPBase() string {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	return sm.rtspBase
}

// SetHLSBase sets MediaMTX's HLS base URL on this machine.
func (sm *StreamManager) SetHLSBase(base string) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	sm.hlsBase = base
}

// FFmpegPath returns the FFmpeg found on this machine ("" if none).
func (sm *StreamManager) FFmpegPath() string {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	return sm.ffmpegPath
}

// HLSBase returns the configured HLS base URL.
func (sm *StreamManager) HLSBase() string {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	return sm.hlsBase
}

// SetWebRTCBase sets the WebRTC/WHEP base URL.
func (sm *StreamManager) SetWebRTCBase(base string) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	sm.webrtcBase = base
}

// WebRTCBase returns the configured WebRTC/WHEP base URL (default: http://127.0.0.1:8889).
func (sm *StreamManager) WebRTCBase() string {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	if sm.webrtcBase != "" {
		return sm.webrtcBase
	}
	return "http://127.0.0.1:8889"
}

// Unlock resets the recovery budget to 3 and authorizes stream reconnect.
func (sm *StreamManager) Unlock(uuid string) (*ManagedCamera, error) {
	sm.mu.RLock()
	mc, ok := sm.cameras[uuid]
	sm.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("camera not found")
	}
	mc.Session.Unlock()
	return mc, nil
}

// SetStrictManual updates the strict manual mode setting on the camera session.
func (sm *StreamManager) SetStrictManual(uuid string, manual bool) error {
	sm.mu.RLock()
	mc, ok := sm.cameras[uuid]
	sm.mu.RUnlock()
	if !ok {
		return fmt.Errorf("camera not found")
	}
	mc.Session.SetStrictManual(manual)
	return nil
}

// SetStreamActive updates the live streaming state, viewer, and signaling for an enrolled camera.
func (sm *StreamManager) SetStreamActive(uuid string, active bool, v *bridge.Viewer, sig *bridge.Signaling, transport string) {
	sm.mu.RLock()
	mc, ok := sm.cameras[uuid]
	sm.mu.RUnlock()
	if !ok {
		return
	}

	mc.mu.Lock()
	mc.Streaming = active
	mc.Viewer = v
	mc.Signaling = sig
	if transport != "" {
		mc.Transport = transport
	}
	mc.Freshness = time.Now().UTC()
	mc.mu.Unlock()

	if active && v != nil && sig != nil {
		registerSig(bridge.Device{UUID: mc.UUID, Name: mc.Name, Type: mc.Model}, sig, v)
	} else {
		unregisterSig(uuid)
	}
}

// UpdateTransport updates the transport description for an enrolled camera.
func (sm *StreamManager) UpdateTransport(uuid string, transport string) {
	sm.mu.RLock()
	mc, ok := sm.cameras[uuid]
	sm.mu.RUnlock()
	if !ok {
		return
	}
	mc.mu.Lock()
	mc.Transport = transport
	mc.Freshness = time.Now().UTC()
	mc.mu.Unlock()
}

// CloseAll stops all active camera streams.
// Unenroll stops and removes a single camera's stream, leaving all other
// cameras (including those owned by other logins) running. Returns true if the
// camera was enrolled.
func (sm *StreamManager) Unenroll(uuid string) bool {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	mc, ok := sm.cameras[uuid]
	if !ok {
		return false
	}
	if mc.CancelStream != nil {
		mc.CancelStream()
	}
	delete(sm.cameras, uuid)
	return true
}

func (sm *StreamManager) CloseAll() {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	for _, mc := range sm.cameras {
		if mc.CancelStream != nil {
			mc.CancelStream()
		}
	}
	sm.cameras = make(map[string]*ManagedCamera)
}

// StopStreaming halts all active camera streaming runners and closes RTSP publications,
// but preserves enrolled camera metadata and strictly does not alter firewall state.
func (sm *StreamManager) StopStreaming() {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	for _, mc := range sm.cameras {
		if mc.CancelStream != nil {
			mc.CancelStream()
			mc.CancelStream = nil
		}
		mc.mu.Lock()
		mc.Streaming = false
		mc.Transport = "Stopped"
		mc.Freshness = time.Now().UTC()
		mc.mu.Unlock()
	}
}

// StartStreaming restarts the camera runners that StopStreaming halted.
func (sm *StreamManager) StartStreaming() int {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	started := 0
	for _, mc := range sm.cameras {
		if mc.CancelStream != nil {
			continue
		}
		ctx, cancel := context.WithCancel(context.Background())
		session := NewCameraSession(mc.UUID, mc.Name, sm.strictManual)
		mc.mu.Lock()
		mc.Session = session
		mc.Transport = "Starting"
		mc.Freshness = time.Now().UTC()
		rtpPort := mc.RTPPort
		mc.mu.Unlock()
		mc.CancelStream = cancel
		if sm.runner != nil {
			go sm.runner(ctx, mc, rtpPort)
		}
		started++
	}
	return started
}

// IsStopped reports whether the camera was stopped from the UI/API.
func (sm *StreamManager) IsStopped(uuid string) bool {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	mc, ok := sm.cameras[uuid]
	return ok && mc.CancelStream == nil
}

// DefaultRetryMaxWait is the longest wait between automatic stream retries
// after three failed starts in a row.
const DefaultRetryMaxWait = 2 * time.Minute

// lockoutCooldown is the wait before the n-th automatic retry round (n >= 1):
// 30 s, doubling, capped at maxWait. Each round tries up to three times, so
// even at the cap Osaio sees only a few start requests every couple of
// minutes per camera.
func lockoutCooldown(n int, maxWait time.Duration) time.Duration {
	if maxWait <= 0 {
		maxWait = DefaultRetryMaxWait
	}
	if n < 1 {
		n = 1
	}
	d := 30 * time.Second
	for i := 1; i < n && d < maxWait; i++ {
		d *= 2
	}
	if d > maxWait {
		d = maxWait
	}
	return d
}

// device is how the camera is described to the vendor bridge.
func (mc *ManagedCamera) device() bridge.Device {
	mc.mu.RLock()
	defer mc.mu.RUnlock()
	return bridge.Device{UUID: mc.UUID, Name: mc.Name, Type: mc.Model}
}

// deviceLocked is device for a caller already holding mc.mu.
func (mc *ManagedCamera) deviceLocked() bridge.Device {
	return bridge.Device{UUID: mc.UUID, Name: mc.Name, Type: mc.Model}
}

// SetRetryMaxWait sets the longest automatic retry wait (0 = default).
func (sm *StreamManager) SetRetryMaxWait(d time.Duration) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	sm.retryMaxWait = d
}

// RetryMaxWait returns the longest automatic retry wait.
func (sm *StreamManager) RetryMaxWait() time.Duration {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	if sm.retryMaxWait <= 0 {
		return DefaultRetryMaxWait
	}
	return sm.retryMaxWait
}

// defaultRunner implements the real camera stream loop with recovery budget and lockout handling.
func (sm *StreamManager) defaultRunner(ctx context.Context, mc *ManagedCamera, rtpPort int) {
	phoneCode := fmt.Sprintf("%032x", rand.Int63())
	session := mc.Session
	lockouts := 0

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		session.mu.Lock()
		isLocked := session.State == SessionStateLockout
		session.mu.Unlock()

		if isLocked {
			lockouts++
			fmt.Printf("[%s] [LOCKOUT] recovery budget exhausted (3 attempts failed).\n", mc.Name)
			if session.IsStrictManual() {
				fmt.Printf("[%s] [LOCKOUT] strict manual mode: waiting for an explicit retry from the UI / API.\n", mc.Name)
				if err := session.WaitUnlockContext(ctx); err != nil {
					return
				}
				fmt.Printf("[%s] [LOCKOUT] explicit unlock received. Resetting budget and resuming stream...\n", mc.Name)
			} else {
				// Back off instead of stopping for good: a vendor-cloud hiccup or
				// a PC waking from sleep must not leave the camera dark until
				// someone notices. 30 s, 1 min, then every 2 min (recorders and
				// the controls, which ride this session, come back within
				// minutes); "Retry now" skips the wait.
				cooldown := lockoutCooldown(lockouts, sm.RetryMaxWait())
				fmt.Printf("[%s] [LOCKOUT] retrying automatically in %v (or press Retry in the UI).\n", mc.Name, cooldown)
				select {
				case <-ctx.Done():
					return
				case <-session.unlockCh:
					fmt.Printf("[%s] [LOCKOUT] retry requested. Resuming stream...\n", mc.Name)
				case <-time.After(cooldown):
					session.Unlock()
					<-session.unlockCh // drain the signal Unlock queued
					fmt.Printf("[%s] [LOCKOUT] cooldown over. Resuming stream...\n", mc.Name)
				}
			}
		}

		c := sm.cloudForUUID(mc.UUID)

		if c == nil {
			select {
			case <-time.After(2 * time.Second):
				continue
			case <-ctx.Done():
				return
			}
		}

		err := sm.streamOnce(ctx, mc, c, phoneCode, rtpPort)
		if err != nil {
			select {
			case <-ctx.Done():
				return
			default:
			}
			canRetry := session.RecordFailure()
			if sm.LocalControlTest() {
				session.mu.Lock()
				session.State, session.RecoveryBudget = SessionStateLockout, 0
				session.mu.Unlock()
				canRetry = false
				fmt.Printf("[%s] [LOCAL TEST] stream ended; waiting for manual restart: %v\n", mc.Name, err)
			}
			if canRetry {
				session.mu.Lock()
				rem := session.RecoveryBudget
				session.mu.Unlock()
				fmt.Printf("[%s] stream ended: %v (budget remaining: %d/3, restarting in 3s)...\n", mc.Name, err, rem)
				select {
				case <-time.After(3 * time.Second):
				case <-ctx.Done():
					return
				}
			}
		} else {
			select {
			case <-ctx.Done():
				return
			default:
			}
			session.RecordSuccess()
			lockouts = 0
			fmt.Printf("[%s] stream session ended cleanly (restarting in 2s)...\n", mc.Name)
			select {
			case <-time.After(2 * time.Second):
			case <-ctx.Done():
				return
			}
		}
	}
}

// streamOnce establishes a single WebRTC media session and MediaMTX publication.
func (sm *StreamManager) streamOnce(ctx context.Context, mc *ManagedCamera, c *bridge.Cloud, phoneCode string, rtpPort int) error {
	session := mc.Session
	dev := mc.device()
	rtspURL := fmt.Sprintf("%s/%s", strings.TrimRight(sm.PublishRTSPBase(), "/"), dev.UUID)
	// Startup may pre-seed mc.IP from the profile while the runner loop runs.
	mc.mu.RLock()
	activeIP := mc.IP
	mc.mu.RUnlock()
	var mediaConfirmed atomic.Bool
	var severed atomic.Bool

	vc, err := c.VideoCall(dev.UUID)
	if err != nil {
		fmt.Printf("[%s] videocall busy/failed: %v, retrying in 3s...\n", dev.Name, err)
		select {
		case <-time.After(3 * time.Second):
		case <-ctx.Done():
			return ctx.Err()
		}
		vc, err = c.VideoCall(dev.UUID)
		if err != nil {
			fmt.Printf("[%s] videocall retry 2 failed: %v, retrying in 5s...\n", dev.Name, err)
			select {
			case <-time.After(5 * time.Second):
			case <-ctx.Done():
				return ctx.Err()
			}
			vc, err = c.VideoCall(dev.UUID)
		}
		if err != nil {
			// The Osaio sign-in was only checked at startup: if it has
			// expired meanwhile, sign in again with the saved login.
			if nc := sm.renewLogin(c, err); nc != nil {
				c = nc
				vc, err = c.VideoCall(dev.UUID)
			}
		}
		if err != nil {
			return fmt.Errorf("videocall: %w", err)
		}
	}

	sig, err := bridge.Connect(c.WsURL, c.APIToken, c.UID, phoneCode)
	if err != nil {
		return fmt.Errorf("ws: %w", err)
	}

	callID := strconv.FormatInt(1000000000000000+rand.Int63n(999999999999999), 10)
	v, err := bridge.NewViewer(sig, vc, dev.UUID, dev.Type, callID, sm.ffmpegPath, rtspURL, rtpPort)
	if err != nil {
		_ = sig.Close()
		return fmt.Errorf("viewer: %w", err)
	}
	defer v.Close()
	v.SetPublisher(sm.Publisher())

	if cc := GatewayControlChannel(); cc != nil {
		v.SetControlChannel(cc)
	}

	sm.SetStreamActive(dev.UUID, true, v, sig, "Vendor WebSocket")
	defer sm.SetStreamActive(dev.UUID, false, nil, nil, "Disconnected")

	sm.mu.RLock()
	subnets := make([]*net.IPNet, len(sm.allowedSubnets))
	copy(subnets, sm.allowedSubnets)
	sm.mu.RUnlock()

	if len(subnets) > 0 {
		var cidrs []string
		for _, s := range subnets {
			cidrs = append(cidrs, s.String())
		}
		_ = v.SetAllowedSubnets(cidrs)
	}

	candFilter, _ := netstack.NewICECandidateFilter(nil, nil)

	// recordLANIP remembers the camera's address on this network (in memory
	// and in the profile). The Firewall tab looks up the camera's MAC
	// address by it, so it must be the address the media really comes from.
	var ipMu sync.Mutex
	recordLANIP := func(ipStr string) {
		if net.ParseIP(ipStr) == nil {
			return
		}
		ipMu.Lock()
		if ipStr == activeIP {
			ipMu.Unlock()
			return
		}
		activeIP = ipStr
		ipMu.Unlock()
		fmt.Printf("[%s] camera's LAN address: %s\n", dev.Name, ipStr)
		mc.mu.Lock()
		mc.IP = ipStr
		mc.mu.Unlock()
		if pm := sm.ProfileManager(); pm != nil {
			go func(camUUID string) {
				// Skip when no profile exists (e.g. after "Delete all"): this
				// write must never recreate the save file.
				if pm.GetProfile() == nil {
					return
				}
				_, _ = pm.Update(context.Background(), func(p *profile.Profile) error {
					if cam, ok := p.Cameras[camUUID]; ok {
						cam.IPAddress = ipStr
						p.Cameras[camUUID] = cam
					}
					return nil
				})
			}(dev.UUID)
		}
	}

	v.OnCandidateHostIP = func(discoveredIP string) {
		if mediaConfirmed.Load() {
			return
		}
		ip := net.ParseIP(discoveredIP)
		if ip == nil {
			return
		}

		matched := false
		if len(subnets) > 0 {
			for _, s := range subnets {
				if s.Contains(ip) {
					matched = true
					break
				}
			}
		} else if candFilter != nil {
			matched = candFilter.IsAllowedIP(ip)
		} else {
			matched = netstack.IsRFC1918PrivateIP(ip)
		}

		if !matched {
			return
		}

		recordLANIP(discoveredIP)
	}

	sm.UpdateTransport(dev.UUID, "connecting")
	v.OnMediaPath = func(kind, remote string) {
		sm.UpdateTransport(dev.UUID, map[string]string{
			"lan":      "Direct (LAN)",
			"internet": "Direct (internet)",
			"relay":    "Relayed via Osaio",
		}[kind])
		if kind == "lan" {
			// The camera often announces its LAN address only inside the SDP
			// answer, never as a separate candidate, so take it from the pair
			// the media actually uses ("192.168.8.23:44845 (host)").
			recordLANIP(lanIPFromRemote(remote))
		}
	}

	v.OnMediaActive = func() {
		if mediaConfirmed.Swap(true) {
			return
		}
		clearSameLoginViewer(dev.UUID)
		session.RecordSuccess()
		sm.UpdateTransport(dev.UUID, "Direct (LAN)")

		// The local-control hardware test closes BombeCam's own signaling
		// connection once LAN media flows. Normally it stays open: it carries
		// the cloud-relayed controls. (The camera firewall is separate and
		// lives on the router.)
		if sm.LocalControlTest() {
			if !severed.Swap(true) {
				fmt.Printf("[%s] [LOCAL TEST] local media confirmed; closing BombeCam's signaling connection\n", dev.Name)
				_ = sig.Close()
			}
		} else {
			fmt.Printf("[%s] local media confirmed; signaling stays open for controls\n", dev.Name)
		}
	}

	sig.SetOnMsg(v.OnMessage)

	if err := v.Start(); err != nil {
		return fmt.Errorf("start: %w", err)
	}

	// Query initial camera telemetry for IR state
	go func() {
		select {
		case <-v.Done():
			return
		case <-ctx.Done():
			return
		case <-time.After(1 * time.Second):
		}
		if !severed.Load() {
			_ = sig.Send("atr.get", []string{"IrLedMode", "LedOnOff", "MotionDetectSW", "SoundDetectSW"})
		}
	}()

	switchTicker := time.NewTicker(20 * time.Second)
	defer switchTicker.Stop()

	watchdog := time.NewTicker(2 * time.Second)
	defer watchdog.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-v.Done():
			return fmt.Errorf("viewer closed")
		case <-switchTicker.C:
			if !severed.Load() {
				_ = sig.Send(bridge.CmdSwitch, map[string]any{"SessionId": vc.SessionID, "Action": 0, "Quality": 2, "Timestamp": 0, "call_id": callID})
			}
		case <-watchdog.C:
			busy := !mediaConfirmed.Load() && v.SameLoginViewerActive(sameLoginWindow)
			if busy {
				noteSameLoginViewer(dev.UUID)
			}
			if v.HasStalled() {
				if busy {
					return fmt.Errorf("no video: the Osaio app is watching this camera with BombeCam's own Osaio login")
				}
				return fmt.Errorf("video stream stalled (no packets)")
			}
		}
	}
}

// lanIPFromRemote extracts the IP from a media-path description such as
// "192.168.8.23:44845 (host)".
func lanIPFromRemote(remote string) string {
	hostPort := strings.TrimSpace(strings.SplitN(remote, " ", 2)[0])
	host, _, err := net.SplitHostPort(hostPort)
	if err != nil {
		host = hostPort
	}
	if ip := net.ParseIP(host); ip != nil && ip.To4() != nil {
		return ip.String()
	}
	return ""
}
