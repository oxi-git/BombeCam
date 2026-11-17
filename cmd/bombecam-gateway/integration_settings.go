package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Fever-r/BombeCam/internal/mediamtx"
	"github.com/Fever-r/BombeCam/pkg/profile"
)

// The "Use with Frigate / Home Assistant" settings live in the encrypted
// profile (profile.IntegrationSettings). This file keeps the running copy,
// applies changes, and serves GET/POST /api/v1/integrations/settings.

const (
	defaultSnapshotPort = 8655
	defaultStreamUser   = "bombecam"
)

var (
	integrationUpdateMu sync.Mutex
	integMu             sync.RWMutex
	integSettings       profile.IntegrationSettings
	// integPortsFromFlags: ports came from command-line flags (or MediaMTX
	// is not managed by BombeCam), so the UI cannot change them.
	integPortsLocked   bool
	integPortsLockNote string
)

func currentIntegrationSettings() profile.IntegrationSettings {
	integMu.RLock()
	defer integMu.RUnlock()
	return integSettings
}

func storeIntegrationSettings(s profile.IntegrationSettings) {
	integMu.Lock()
	defer integMu.Unlock()
	integSettings = s
}

func setPortsLocked(locked bool, note string) {
	integMu.Lock()
	defer integMu.Unlock()
	integPortsLocked, integPortsLockNote = locked, note
}

func portsLocked() (bool, string) {
	integMu.RLock()
	defer integMu.RUnlock()
	return integPortsLocked, integPortsLockNote
}

func snapshotPort(s profile.IntegrationSettings) int {
	if s.SnapshotPort > 0 {
		return s.SnapshotPort
	}
	return defaultSnapshotPort
}

// mediaPorts returns the MediaMTX ports the settings ask for (defaults for 0).
func mediaPorts(s profile.IntegrationSettings) (rtsp, hls, webrtc, ice int) {
	rtsp, hls, webrtc, ice = s.RTSPPort, s.HLSPort, s.WebRTCPort, s.WebRTCICEPort
	if rtsp <= 0 {
		rtsp = mediamtx.DefaultRTSPPort
	}
	if hls <= 0 {
		hls = mediamtx.DefaultHTTPPort
	}
	if webrtc <= 0 {
		webrtc = mediamtx.DefaultWebRTCPort
	}
	if ice <= 0 {
		ice = mediamtx.DefaultWebRTCICEPort
	}
	return
}

// applyMediaSettings copies ports and the stream password into a MediaMTX
// configuration.
func applyMediaSettings(c *mediamtx.Config, s profile.IntegrationSettings) {
	c.RTSPPort, c.HTTPPort, c.WebRTCPort, c.WebRTCICEPort = mediaPorts(s)
	c.ReadUser, c.ReadPass = "", ""
	if s.StreamAuth && s.StreamUser != "" && !s.StreamPassword.IsEmpty() {
		c.ReadUser, c.ReadPass = s.StreamUser, s.StreamPassword.Expose()
	}
}

// mediaSettingsDiffer reports whether a change needs MediaMTX restarted.
func mediaSettingsDiffer(a, b profile.IntegrationSettings) bool {
	ar, ah, aw, ai := mediaPorts(a)
	br, bh, bw, bi := mediaPorts(b)
	if ar != br || ah != bh || aw != bw || ai != bi {
		return true
	}
	if a.StreamAuth != b.StreamAuth {
		return true
	}
	return a.StreamAuth && (a.StreamUser != b.StreamUser || !a.StreamPassword.Equal(b.StreamPassword))
}

// setStreamManagerPorts points the stream manager at MediaMTX's ports.
func setStreamManagerPorts(sm *StreamManager, s profile.IntegrationSettings) {
	rtsp, hls, webrtc, _ := mediaPorts(s)
	sm.SetPublishRTSPBase(fmt.Sprintf("rtsp://127.0.0.1:%d", rtsp))
	sm.SetHLSBase(fmt.Sprintf("http://127.0.0.1:%d", hls))
	sm.SetWebRTCBase(fmt.Sprintf("http://127.0.0.1:%d", webrtc))
}

// applyIntegrationRuntime makes the running gateway use s (everything except
// restarting MediaMTX, which reconfigureMedia does).
func applyIntegrationRuntime(sm *StreamManager, s profile.IntegrationSettings) {
	storeIntegrationSettings(s)
	if sm != nil {
		sm.SetNVRAddress(s.NVRAddress)
		if s.StreamAuth && s.StreamUser != "" && !s.StreamPassword.IsEmpty() {
			sm.SetStreamCredentials(s.StreamUser, s.StreamPassword.Expose())
		} else {
			sm.SetStreamCredentials("", "")
		}
		snapshots.Configure(sm, s)
	}
	kickIntegrations()
}

// reconfigureMedia restarts BombeCam's MediaMTX with the ports and stream
// password in s, and restarts the camera streams if the RTSP port moved.
func reconfigureMedia(ctx context.Context, sm *StreamManager, s profile.IntegrationSettings) error {
	sup, _ := currentMediaRuntime()
	if sup == nil {
		return fmt.Errorf("MediaMTX is not managed by BombeCam here (Docker or -no-mediamtx-supervisor); change its configuration (deploy/mediamtx.yml) instead")
	}
	oldRTSP := sm.RTSPPort()
	if err := sup.Reconfigure(ctx, func(c *mediamtx.Config) { applyMediaSettings(c, s) }); err != nil {
		return err
	}
	setStreamManagerPorts(sm, s)
	if newRTSP, _, _, _ := mediaPorts(s); newRTSP != oldRTSP {
		// publishers hold the old RTSP address: start the camera sessions again
		sm.StopStreaming()
		sm.StartStreaming()
	}
	kickIntegrations()
	return nil
}

// generatePassword returns a random password of letters and digits (no
// characters that need escaping in URLs or YAML).
func generatePassword(n int) string {
	const alphabet = "abcdefghijkmnopqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	b := make([]byte, n)
	for i := range b {
		k, err := rand.Int(rand.Reader, big.NewInt(int64(len(alphabet))))
		if err != nil {
			panic(err)
		}
		b[i] = alphabet[k.Int64()]
	}
	return string(b)
}

// loadIntegrationSettings applies the profile's settings at startup.
func loadIntegrationSettings(sm *StreamManager, prof *profile.Profile) {
	if prof == nil {
		applyIntegrationRuntime(sm, profile.IntegrationSettings{})
		return
	}
	applyIntegrationRuntime(sm, prof.Integrations)
}

// ----------------------------------------------------------------------------
// GET/POST /api/v1/integrations/settings
// ----------------------------------------------------------------------------

type integrationPorts struct {
	RTSP      int `json:"rtsp"`
	HLS       int `json:"hls"`
	WebRTC    int `json:"webrtc"`
	WebRTCUDP int `json:"webrtc_udp"`
	Snapshot  int `json:"snapshot"`
}

type integrationMQTTView struct {
	Enabled         bool   `json:"enabled"`
	Host            string `json:"host"`
	Port            int    `json:"port"`
	Username        string `json:"username"`
	HasPassword     bool   `json:"has_password"`
	DiscoveryPrefix string `json:"discovery_prefix"`
	Status          string `json:"status"`
	Detail          string `json:"detail,omitempty"`
}

type integrationCameraView struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	StreamName  string `json:"stream_name"`
	Streaming   bool   `json:"streaming"`
	RTSPURL     string `json:"rtsp_url"`
	RTSPURLByID string `json:"rtsp_url_by_id"`
	HLSURL      string `json:"hls_url"`
	SnapshotURL string `json:"snapshot_url,omitempty"`
	Width       int    `json:"width,omitempty"`
	Height      int    `json:"height,omitempty"`
}

type integrationSettingsView struct {
	HasProfile          bool             `json:"has_profile"`
	Addresses           []LANAddress     `json:"addresses"`
	NVRAddress          string           `json:"nvr_address"`
	AdvertisedAddress   string           `json:"advertised_address"`
	AddressSource       string           `json:"address_source"`
	ChosenMissing       bool             `json:"chosen_missing"`
	Override            string           `json:"override,omitempty"`
	StreamAuth          bool             `json:"stream_auth"`
	RequestedStreamAuth bool             `json:"requested_stream_auth"`
	StreamAuthVerified  bool             `json:"stream_auth_verified"`
	StreamAuthEditable  bool             `json:"stream_auth_editable"`
	StreamAuthDetail    string           `json:"stream_auth_detail,omitempty"`
	StreamUser          string           `json:"stream_user,omitempty"`
	StreamPassword      string           `json:"stream_password,omitempty"`
	Snapshots           bool             `json:"snapshots"`
	SnapshotStatus      string           `json:"snapshot_status"`
	FFmpeg              bool             `json:"ffmpeg"`
	BrowserAudio        bool             `json:"browser_audio"`
	Ports               integrationPorts `json:"ports"`
	// RTSPUDPPorts are the UDP ports MediaMTX uses for RTSP over UDP (RTP,
	// RTCP): 8000/8001 unless another program holds them. Read-only.
	RTSPUDPPorts      []int                   `json:"rtsp_udp_ports,omitempty"`
	PortsEditable     bool                    `json:"ports_editable"`
	PortsNote         string                  `json:"ports_note,omitempty"`
	MediaServerOK     bool                    `json:"media_server_ok"`
	MediaServerDetail string                  `json:"media_server_detail,omitempty"`
	StreamNamesError  string                  `json:"stream_names_error,omitempty"`
	MQTT              integrationMQTTView     `json:"mqtt"`
	Network           networkProfileView      `json:"network"`
	Cameras           []integrationCameraView `json:"cameras"`
	Warnings          []string                `json:"warnings,omitempty"`
}

func buildIntegrationSettingsView(sm *StreamManager, r *http.Request, pm profile.ProfileManager) integrationSettingsView {
	s := currentIntegrationSettings()
	host, source, missing := sm.AdvertisedHost(r)
	rtsp, hls, webrtc, ice := mediaPorts(s)
	locked, note := portsLocked()
	v := integrationSettingsView{
		HasProfile:          pm != nil && pm.GetProfile() != nil,
		Addresses:           lanAddressesFunc(),
		NVRAddress:          s.NVRAddress,
		AdvertisedAddress:   host,
		AddressSource:       source,
		ChosenMissing:       missing,
		Override:            sm.ConsumerRTSPBase(),
		RequestedStreamAuth: s.StreamAuth,
		Snapshots:           s.Snapshots,
		SnapshotStatus:      snapshots.Status(),
		FFmpeg:              sm.FFmpegPath() != "",
		BrowserAudio:        browserAudioAvailable(sm),
		Ports:               integrationPorts{RTSP: rtsp, HLS: hls, WebRTC: webrtc, WebRTCUDP: ice, Snapshot: snapshotPort(s)},
		PortsEditable:       !locked,
		PortsNote:           note,
		StreamNamesError:    lastAliasError(),
		MQTT:                mqttView(s.MQTT),
		Network:             currentNetworkProfile(),
	}
	if sup, _ := currentMediaRuntime(); sup != nil {
		if rtp := sup.Config().RTPPort; rtp > 0 {
			v.RTSPUDPPorts = []int{rtp, rtp + 1}
		}
	}
	if locked {
		// show what is really in use
		v.Ports.RTSP, v.Ports.HLS, v.Ports.WebRTC = sm.RTSPPort(), sm.HLSPort(), portOfURL(sm.WebRTCBase(), 8889)
	}
	if v.Addresses == nil {
		v.Addresses = []LANAddress{}
	}
	if sup, _ := currentMediaRuntime(); sup != nil {
		v.StreamAuthEditable = sup.CanReconfigure() == nil
		cfg := sup.Config()
		expected := cfg
		applyMediaSettings(&expected, s)
		if cfg.ReadUser != expected.ReadUser || cfg.ReadPass != expected.ReadPass {
			v.StreamAuthDetail = "The running media configuration differs from the saved stream-password preference."
		} else if err := sup.VerifyReadPolicy(r.Context()); err != nil {
			v.StreamAuthDetail = err.Error()
		} else {
			v.StreamAuthVerified = true
			v.StreamAuth = s.StreamAuth
		}
	} else {
		v.StreamAuthDetail = "The external media server's stream-password policy is unverified; manage it in its own configuration."
	}
	if !v.StreamAuthVerified && s.StreamAuth {
		v.Warnings = append(v.Warnings, "Stream password protection is unverified: "+v.StreamAuthDetail)
	}
	if s.StreamAuth {
		v.StreamUser, v.StreamPassword = s.StreamUser, s.StreamPassword.Expose()
	}
	v.MediaServerOK, v.MediaServerDetail = mediaServerHealth()
	cams, _ := collectIntegrationCameras(sm, r, "")
	for _, c := range cams {
		cv := integrationCameraView{ID: c.ID, Name: c.Name, StreamName: c.StreamName, Streaming: c.Streaming,
			RTSPURL: c.RTSPURL, RTSPURLByID: c.RTSPURLByID, HLSURL: c.HLSURL, SnapshotURL: c.SnapshotURL}
		if c.Video != nil {
			cv.Width, cv.Height = c.Video.Width, c.Video.Height
		}
		v.Cameras = append(v.Cameras, cv)
	}
	if v.Cameras == nil {
		v.Cameras = []integrationCameraView{}
	}
	if missing {
		v.Warnings = append(v.Warnings, fmt.Sprintf("The address you chose (%s) is not on this PC right now, so %s is shown instead. Choose again below.", s.NVRAddress, host))
	}
	if source == addrFromLoopback {
		v.Warnings = append(v.Warnings, "This PC has no network address, so other devices cannot reach the streams yet. Connect it to your network.")
	}
	if v.Network.Public {
		v.Warnings = append(v.Warnings, "Windows treats this network as Public, which blocks other devices from reaching the streams. See \"If other devices can't connect\" below.")
	}
	return v
}

func handleIntegrationSettings(w http.ResponseWriter, r *http.Request, sm *StreamManager) {

	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, buildIntegrationSettingsView(sm, r, sm.ProfileManager()))
	case http.MethodPost:
		handleIntegrationSettingsUpdate(w, r, sm)
	default:
		http.Error(w, "GET or POST required", http.StatusMethodNotAllowed)
	}
}

type integrationSettingsUpdate struct {
	NVRAddress         *string           `json:"nvr_address"`
	StreamAuth         *bool             `json:"stream_auth"`
	RegeneratePassword bool              `json:"regenerate_password"`
	Snapshots          *bool             `json:"snapshots"`
	Ports              *integrationPorts `json:"ports"`
	MQTT               *struct {
		Enabled         *bool   `json:"enabled"`
		Host            *string `json:"host"`
		Port            *int    `json:"port"`
		Username        *string `json:"username"`
		Password        *string `json:"password"`
		ClearPassword   bool    `json:"clear_password"`
		DiscoveryPrefix *string `json:"discovery_prefix"`
	} `json:"mqtt"`
}

func validPort(p int) bool { return p == 0 || (p >= 1024 && p <= 65535) }

// handleIntegrationSettingsUpdate handles POST /api/v1/integrations/settings.
func handleIntegrationSettingsUpdate(w http.ResponseWriter, r *http.Request, sm *StreamManager) {
	pm, ok := privacyGuard(w, r, sm)
	if !ok {
		return
	}
	var req integrationSettingsUpdate
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_request", "message": "Send the settings as JSON."})
		return
	}
	bad := func(msg string) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_setting", "message": msg})
	}

	integrationUpdateMu.Lock()
	defer integrationUpdateMu.Unlock()
	old := currentIntegrationSettings()
	next := old
	if req.NVRAddress != nil {
		addr := strings.TrimSpace(*req.NVRAddress)
		if addr != "" {
			found := false
			for _, a := range lanAddressesFunc() {
				found = found || a.IP == addr
			}
			if !found {
				bad(fmt.Sprintf("%s is not one of this PC's network addresses.", addr))
				return
			}
		}
		next.NVRAddress = addr
	}
	if req.StreamAuth != nil {
		next.StreamAuth = *req.StreamAuth
	}
	if next.StreamAuth {
		if next.StreamUser == "" {
			next.StreamUser = defaultStreamUser
		}
		if next.StreamPassword.IsEmpty() || req.RegeneratePassword {
			next.StreamPassword = profile.SecretString(generatePassword(16))
		}
	}
	if req.Snapshots != nil {
		next.Snapshots = *req.Snapshots
	}
	if req.Ports != nil {
		p := *req.Ports
		for _, v := range []int{p.RTSP, p.HLS, p.WebRTC, p.WebRTCUDP, p.Snapshot} {
			if !validPort(v) {
				bad("Ports must be between 1024 and 65535.")
				return
			}
		}
		next.RTSPPort, next.HLSPort, next.WebRTCPort, next.WebRTCICEPort, next.SnapshotPort = p.RTSP, p.HLS, p.WebRTC, p.WebRTCUDP, p.Snapshot
		rt, hl, wr, _ := mediaPorts(next)
		sn := snapshotPort(next)
		seen := map[int]bool{}
		for _, v := range []int{rt, hl, wr, sn, 8654, mediamtx.DefaultAPIPort} {
			if seen[v] {
				bad("Each port must be different (8654 and 9997 are taken by BombeCam itself).")
				return
			}
			seen[v] = true
		}
		if locked, note := portsLocked(); locked {
			r0, h0, w0, i0 := mediaPorts(old)
			if _, _, _, i1 := mediaPorts(next); r0 != rt || h0 != hl || w0 != wr || i0 != i1 {
				bad(note)
				return
			}
		}
	}
	if m := req.MQTT; m != nil {
		if m.Enabled != nil {
			next.MQTT.Enabled = *m.Enabled
		}
		if m.Host != nil {
			next.MQTT.Host = strings.TrimSpace(*m.Host)
		}
		if m.Port != nil {
			if *m.Port < 0 || *m.Port > 65535 {
				bad("The MQTT port must be between 1 and 65535.")
				return
			}
			next.MQTT.Port = *m.Port
		}
		if m.Username != nil {
			next.MQTT.Username = strings.TrimSpace(*m.Username)
		}
		if m.Password != nil && *m.Password != "" {
			next.MQTT.Password = profile.SecretString(*m.Password)
		}
		if m.ClearPassword {
			next.MQTT.Password = ""
		}
		if m.DiscoveryPrefix != nil {
			next.MQTT.DiscoveryPrefix = strings.Trim(strings.TrimSpace(*m.DiscoveryPrefix), "/")
		}
		if next.MQTT.Enabled && next.MQTT.Host == "" {
			bad("Enter the address of your MQTT broker (for example the Home Assistant machine).")
			return
		}
		if strings.ContainsAny(next.MQTT.DiscoveryPrefix, "#+ ") {
			bad("The discovery prefix may not contain spaces, # or +.")
			return
		}
	}

	changedMedia := mediaSettingsDiffer(old, next)
	if changedMedia {
		sup, _ := currentMediaRuntime()
		if sup == nil {
			bad("MediaMTX is not managed here; change ports and passwords in its own configuration.")
			return
		}
		if err := sup.CanReconfigure(); err != nil {
			bad(err.Error())
			return
		}
		cfg := sup.Config()
		if sm.RTSPPort() != cfg.RTSPPort || sm.HLSPort() != cfg.HTTPPort || portOfURL(sm.WebRTCBase(), 8889) != cfg.WebRTCPort {
			bad("The configured video server addresses differ from the managed server; change its configuration directly.")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		err := reconfigureMedia(ctx, sm, next)
		cancel()
		if err != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "error": "media_change_failed", "message": err.Error(), "settings": buildIntegrationSettingsView(sm, r, pm)})
			return
		}
	}
	if _, err := pm.Update(r.Context(), func(p *profile.Profile) error { p.Integrations = next; return nil }); err != nil {
		message := "Could not save settings: " + err.Error()
		if changedMedia {
			cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			restoreErr := reconfigureMedia(cleanup, sm, old)
			cancel()
			if restoreErr != nil {
				message += ". Previous media settings could not be restored: " + restoreErr.Error()
			}
		}
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "save_failed", "message": message})
		return
	}
	applyIntegrationRuntime(sm, next)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "restarted_media": changedMedia, "settings": buildIntegrationSettingsView(sm, r, pm)})
}

// handleCameraStreamName handles POST /api/v1/cameras/{id}/stream-name
// {"stream_name": "garage"}: renames the camera's friendly stream path.
func handleCameraStreamName(w http.ResponseWriter, r *http.Request, camID string, sm *StreamManager) {
	pm := sm.ProfileManager()
	if pm == nil || pm.GetProfile() == nil {
		writeJSON(w, http.StatusConflict, map[string]any{"error": "not_configured", "message": "Add your cameras first."})
		return
	}
	var req struct {
		StreamName string `json:"stream_name"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_request", "message": `Send {"stream_name": "..."}.`})
		return
	}
	name := strings.TrimSpace(req.StreamName)
	if err := validateStreamName(name); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_stream_name", "message": err.Error()})
		return
	}
	prof := pm.GetProfile()
	cam, ok := prof.Cameras[camID]
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "camera not found"})
		return
	}
	for id, other := range prof.Cameras {
		if id != camID && (other.StreamConfig.RTSPPath == name || id == name) {
			writeJSON(w, http.StatusConflict, map[string]any{"error": "stream_name_taken", "message": fmt.Sprintf("%q is already used by %s.", name, other.Name)})
			return
		}
	}
	old := cam.StreamConfig.RTSPPath
	if _, err := pm.Update(r.Context(), func(p *profile.Profile) error {
		c := p.Cameras[camID]
		c.StreamConfig.RTSPPath = name
		p.Cameras[camID] = c
		return nil
	}); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "save_failed", "message": err.Error()})
		return
	}
	syncStreamNames(r.Context(), pm, sm)
	kickIntegrations()
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "stream_name": name, "previous": old,
		"rtsp_url": sm.ConsumerRTSPURL(camID, r), "rtsp_url_by_id": sm.ConsumerRTSPURLByID(camID, r),
	})
}
