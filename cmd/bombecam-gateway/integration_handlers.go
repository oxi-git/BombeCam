package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Fever-r/BombeCam/pkg/bridge"
	"github.com/Fever-r/BombeCam/pkg/profile"
)

// FrigateDocsVersion is the Frigate release whose documentation the
// generated configuration was checked against (docs.frigate.video).
const FrigateDocsVersion = "0.18"

var (
	frigateInvalidCharsRegex      = regexp.MustCompile(`[^a-zA-Z0-9_]+`)
	frigateConsecutiveUnderscores = regexp.MustCompile(`_+`)
	yamlPlainSafe                 = regexp.MustCompile(`^[A-Za-z0-9:/._@%~+=\-]+$`)
)

// sanitizeFrigateIdentifier converts arbitrary camera names/UUIDs to valid Frigate camera keys matching ^[a-zA-Z0-9_]+$.
func sanitizeFrigateIdentifier(name, uuid string) string {
	candidate := strings.ToLower(strings.TrimSpace(name))
	if candidate == "" {
		candidate = strings.ToLower(strings.TrimSpace(uuid))
	}
	candidate = frigateInvalidCharsRegex.ReplaceAllString(candidate, "_")
	candidate = frigateConsecutiveUnderscores.ReplaceAllString(candidate, "_")
	candidate = strings.Trim(candidate, "_")
	if candidate == "" {
		sanitizedUUID := frigateInvalidCharsRegex.ReplaceAllString(strings.ToLower(strings.TrimSpace(uuid)), "_")
		sanitizedUUID = frigateConsecutiveUnderscores.ReplaceAllString(sanitizedUUID, "_")
		sanitizedUUID = strings.Trim(sanitizedUUID, "_")
		if sanitizedUUID == "" {
			return "camera_default"
		}
		return "camera_" + sanitizedUUID
	}
	if candidate[0] >= '0' && candidate[0] <= '9' {
		return "cam_" + candidate
	}
	return candidate
}

// extractPortFromURL extracts the port from a URL string or falls back to a scheme default.
func extractPortFromURL(rawURL string, defaultPort int) int {
	if rawURL == "" {
		return defaultPort
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return defaultPort
	}
	portStr := u.Port()
	if portStr != "" {
		if p, err := strconv.Atoi(portStr); err == nil && p > 0 {
			return p
		}
	}
	if strings.EqualFold(u.Scheme, "rtsp") {
		return 8554
	}
	if strings.EqualFold(u.Scheme, "https") {
		return 443
	}
	if strings.EqualFold(u.Scheme, "http") {
		return 80
	}
	return defaultPort
}

// getGatewayBaseURL determines the base HTTP/HTTPS URL and port for the gateway.
func getGatewayBaseURL(r *http.Request) (string, int) {
	scheme := "http"
	if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		scheme = "https"
	}
	host := r.Host
	if host == "" {
		host = "127.0.0.1:8654"
	}
	baseURL := fmt.Sprintf("%s://%s", scheme, host)
	port := extractPortFromURL(baseURL, 8654)
	return baseURL, port
}

// yamlValue writes a scalar plainly when that is unambiguous, else quoted.
func yamlValue(s string) string {
	if yamlPlainSafe.MatchString(s) {
		return s
	}
	b, _ := json.Marshal(s) // a JSON string is a valid YAML double-quoted scalar
	return string(b)
}

// ----------------------------------------------------------------------------
// Cameras as other programs see them
// ----------------------------------------------------------------------------

// integrationCamera is one camera with the URLs other devices use.
type integrationCamera struct {
	ID          string
	Name        string
	Model       string
	Key         string // Frigate camera name = go2rtc stream name = stream name
	StreamName  string // friendly RTSP path ("" before one is assigned)
	Streaming   bool
	RTSPURL     string // friendly path when there is one
	RTSPURLByID string
	HLSURL      string
	SnapshotURL string // "" when snapshots for the network are off
	Video       *bridge.VideoInfo
	RTPPort     int
}

// collectIntegrationCameras lists the enrolled cameras (all, or the one
// matching filter by ID, name or stream name), sorted by camera ID.
func collectIntegrationCameras(sm *StreamManager, r *http.Request, filter string) ([]integrationCamera, bool) {
	all := sm.GetAllCameras()
	sort.Slice(all, func(i, j int) bool { return all[i].UUID < all[j].UUID })
	filter = strings.TrimSpace(filter)
	var out []integrationCamera
	usedKeys := map[string]bool{}
	for _, mc := range all {
		mc.mu.RLock()
		id, name, model, streaming, rtpPort, viewer := mc.UUID, mc.Name, mc.Model, mc.Streaming, mc.RTPPort, mc.Viewer
		mc.mu.RUnlock()
		streamName := sm.StreamName(id)
		if filter != "" && !strings.EqualFold(filter, id) && !strings.EqualFold(filter, name) && !strings.EqualFold(filter, streamName) {
			continue
		}
		if rtpPort == 0 {
			rtpPort = sm.PortBase()
		}
		key := streamName
		if key == "" {
			key = sanitizeFrigateIdentifier(name, id)
		}
		for base, i := key, 2; usedKeys[key]; i++ {
			key = fmt.Sprintf("%s_%d", base, i)
		}
		usedKeys[key] = true
		c := integrationCamera{
			ID: id, Name: name, Model: model, Key: key, StreamName: streamName, Streaming: streaming,
			RTSPURL:     sm.ConsumerRTSPURL(id, r),
			RTSPURLByID: sm.ConsumerRTSPURLByID(id, r),
			HLSURL:      sm.ConsumerHLSURL(id, r),
			SnapshotURL: snapshotURLFor(sm, r, id),
			RTPPort:     rtpPort,
		}
		if viewer != nil {
			if vi, ok := viewer.VideoInfo(); ok {
				c.Video = &vi
			}
		}
		out = append(out, c)
	}
	return out, filter == "" || len(out) > 0
}

// wantFormat reads ?format= or the Accept header ("yaml", "text" or "json").
func wantFormat(r *http.Request) string {
	format := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("format")))
	if format == "yml" {
		format = "yaml"
	}
	if format == "" {
		accept := strings.ToLower(r.Header.Get("Accept"))
		switch {
		case strings.Contains(accept, "application/x-yaml") || strings.Contains(accept, "text/yaml"):
			format = "yaml"
		case strings.Contains(accept, "text/plain"):
			format = "text"
		default:
			format = "json"
		}
	}
	return format
}

// ----------------------------------------------------------------------------
// Frigate
// ----------------------------------------------------------------------------

type frigateFFmpegInput struct {
	Path  string   `json:"path"`
	Roles []string `json:"roles"`
}

type frigateFFmpeg struct {
	Inputs     []frigateFFmpegInput `json:"inputs"`
	OutputArgs map[string]string    `json:"output_args"`
}

type frigateDetect struct {
	Enabled bool `json:"enabled"`
	FPS     int  `json:"fps"`
	// Width/Height are left out: Frigate uses the stream's own size.
	Width  int `json:"width,omitempty"`
	Height int `json:"height,omitempty"`
}

type frigateCameraConfig struct {
	UUID          string            `json:"uuid"`
	Name          string            `json:"name"`
	Model         string            `json:"model"`
	StreamName    string            `json:"stream_name"`
	RTSPURL       string            `json:"rtsp_url"`
	RTSPURLByID   string            `json:"rtsp_url_by_id"`
	HLSURL        string            `json:"hls_url"`
	Streaming     bool              `json:"streaming"`
	Go2RTCSources []string          `json:"go2rtc_sources"`
	FFmpeg        frigateFFmpeg     `json:"ffmpeg"`
	Detect        frigateDetect     `json:"detect"`
	Video         *bridge.VideoInfo `json:"video,omitempty"`
}

type streamPortMappings struct {
	RTSP    int `json:"rtsp"`
	HLS     int `json:"hls"`
	Gateway int `json:"gateway"`
	RTP     int `json:"rtp"`
}

type globalPortMappings struct {
	RTSP     int `json:"rtsp"`
	HLS      int `json:"hls"`
	WebRTC   int `json:"webrtc"`
	Gateway  int `json:"gateway"`
	Snapshot int `json:"snapshot,omitempty"`
}

type frigateStreamEntry struct {
	UUID         string             `json:"uuid"`
	Name         string             `json:"name"`
	Model        string             `json:"model"`
	Streaming    bool               `json:"streaming"`
	RTSPURL      string             `json:"rtsp_url"`
	HLSURL       string             `json:"hls_url"`
	PortMappings streamPortMappings `json:"port_mappings"`
}

type frigateIntegrationResponse struct {
	Integration string `json:"integration"`
	// Version is the Frigate documentation release the snippets follow.
	Version      string                         `json:"version"`
	GeneratedAt  string                         `json:"generated_at"`
	Cameras      map[string]frigateCameraConfig `json:"cameras"`
	Streams      []frigateStreamEntry           `json:"streams"`
	PortMappings globalPortMappings             `json:"port_mappings"`
	// ConfigYAML is for adding the cameras to an existing Frigate config
	// (no mqtt: or other global sections); ConfigYAMLNew is a complete
	// minimal config for a fresh Frigate.
	ConfigYAML    string `json:"config_yaml"`
	ConfigYAMLNew string `json:"config_yaml_new"`
}

// frigateOptions shape the generated YAML.
type frigateOptions struct {
	Roles []string
	// BrowserAudio: BombeCam publishes a G.711 copy of the sound (FFmpeg on
	// this PC), which go2rtc hands to WebRTC viewers as is. Without it,
	// go2rtc makes Opus with Frigate's own FFmpeg.
	BrowserAudio bool
	NewConfig    bool
}

func frigateConfigFor(c integrationCamera, o frigateOptions) frigateCameraConfig {
	sources := []string{c.RTSPURL}
	if !o.BrowserAudio {
		sources = append(sources, "ffmpeg:"+c.Key+"#audio=opus")
	}
	return frigateCameraConfig{
		UUID: c.ID, Name: c.Name, Model: c.Model, StreamName: c.StreamName,
		RTSPURL: c.RTSPURL, RTSPURLByID: c.RTSPURLByID, HLSURL: c.HLSURL, Streaming: c.Streaming,
		Go2RTCSources: sources,
		FFmpeg: frigateFFmpeg{
			Inputs:     []frigateFFmpegInput{{Path: c.RTSPURL, Roles: o.Roles}},
			OutputArgs: map[string]string{"record": "preset-record-generic-audio-copy"},
		},
		Detect: frigateDetect{Enabled: true, FPS: 5},
		Video:  c.Video,
	}
}

// smallerDetectSize suggests a detect size of at most 1280 pixels wide with
// the camera's aspect ratio (multiples of 8), or 0,0 if the stream is small.
func smallerDetectSize(w, h int) (int, int) {
	if w <= 1280 || h <= 0 {
		return 0, 0
	}
	nh := h * 1280 / w
	nh -= nh % 8
	return 1280, nh
}

// generateFrigateYAML builds the Frigate configuration for the cameras (keyed
// by Frigate camera name). Each camera gets a go2rtc stream of the same name,
// so Frigate's live view uses MSE/WebRTC with sound without a live: block.
func generateFrigateYAML(cameras map[string]frigateCameraConfig, o frigateOptions) string {
	var sb strings.Builder
	keys := make([]string, 0, len(cameras))
	for k := range cameras {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	if o.NewConfig {
		sb.WriteString("# Frigate configuration made by BombeCam (checked against Frigate " + FrigateDocsVersion + ").\n")
		sb.WriteString("# Home Assistant's Frigate integration needs MQTT: set enabled to true and\n")
		sb.WriteString("# add host: <your MQTT broker's address>.\n")
		sb.WriteString("mqtt:\n  enabled: false\n\n")
		// Frigate keeps only alerts and detections unless told otherwise
		sb.WriteString("record:\n  enabled: true\n  motion:\n    days: 7 # keep footage with motion for a week\n\n")
	} else {
		sb.WriteString("# BombeCam cameras for Frigate (checked against Frigate " + FrigateDocsVersion + ").\n")
		sb.WriteString("# Add these to your Frigate config. If it already has a go2rtc: or cameras:\n")
		sb.WriteString("# section, put these entries under the existing one: each key may appear once.\n")
	}
	if len(keys) == 0 {
		sb.WriteString("# No cameras have been added to BombeCam yet.\n")
		return sb.String()
	}

	sb.WriteString("go2rtc:\n  streams:\n")
	for _, k := range keys {
		fmt.Fprintf(&sb, "    %s:\n", k)
		for _, src := range cameras[k].Go2RTCSources {
			fmt.Fprintf(&sb, "      - %s\n", yamlValue(src))
		}
	}

	sb.WriteString("\ncameras:\n")
	for _, k := range keys {
		cam := cameras[k]
		fmt.Fprintf(&sb, "  %s:", k)
		if cam.Name != "" && cam.Name != k {
			fmt.Fprintf(&sb, " # %s", strings.ReplaceAll(cam.Name, "\n", " "))
		}
		sb.WriteString("\n    ffmpeg:\n      inputs:\n")
		for _, in := range cam.FFmpeg.Inputs {
			fmt.Fprintf(&sb, "        - path: %s\n          roles:\n", yamlValue(in.Path))
			for _, role := range in.Roles {
				fmt.Fprintf(&sb, "            - %s\n", role)
			}
		}
		sb.WriteString("      output_args:\n")
		// the camera's sound is already AAC: keep it instead of re-encoding
		fmt.Fprintf(&sb, "        record: %s\n", cam.FFmpeg.OutputArgs["record"])
		sb.WriteString("    detect:\n      enabled: true\n")
		fmt.Fprintf(&sb, "      fps: %d\n", cam.Detect.FPS)
		if v := cam.Video; v != nil && v.Width > 0 {
			if w, h := smallerDetectSize(v.Width, v.Height); w > 0 {
				fmt.Fprintf(&sb, "      # The camera sends %dx%d; to detect on a smaller picture (less CPU):\n", v.Width, v.Height)
				fmt.Fprintf(&sb, "      # width: %d\n      # height: %d\n", w, h)
			}
		}
		sb.WriteString("    record:\n      enabled: true\n")
	}
	return sb.String()
}

func parseRoles(r *http.Request) []string {
	var roles []string
	for _, part := range strings.Split(r.URL.Query().Get("roles"), ",") {
		if p := strings.TrimSpace(part); p != "" {
			roles = append(roles, p)
		}
	}
	if len(roles) == 0 {
		roles = []string{"detect", "record"}
	}
	return roles
}

// browserAudioAvailable reports whether BombeCam adds the G.711 copy of the
// sound (it needs FFmpeg on this PC and the built-in publisher).
func browserAudioAvailable(sm *StreamManager) bool {
	sm.mu.RLock()
	ff := sm.ffmpegPath
	sm.mu.RUnlock()
	return ff != "" && sm.Publisher() == bridge.PublisherNative
}

func (sm *StreamManager) portMappings(r *http.Request) globalPortMappings {
	_, gatewayPort := getGatewayBaseURL(r)
	pm := globalPortMappings{
		RTSP:    extractPortFromURL(sm.ConsumerRTSPBaseURL(r), sm.RTSPPort()),
		HLS:     extractPortFromURL(sm.ConsumerHLSBaseURL(r), sm.HLSPort()),
		WebRTC:  portOfURL(sm.WebRTCBase(), 8889),
		Gateway: gatewayPort,
	}
	if s := currentIntegrationSettings(); s.Snapshots {
		pm.Snapshot = snapshotPort(s)
	}
	return pm
}

// handleFrigateIntegration handles GET /api/v1/integrations/frigate
// (?format=yaml, ?variant=new, ?roles=detect,record, ?camera=<id|name>).
func handleFrigateIntegration(w http.ResponseWriter, r *http.Request, streamMgr *StreamManager, optPM ...profile.ProfileManager) {
	if !validateOperatorRequest(w, r) {
		return
	}

	if r.Method != http.MethodGet {
		http.Error(w, "GET required", http.StatusMethodNotAllowed)
		return
	}
	cams, found := collectIntegrationCameras(streamMgr, r, r.URL.Query().Get("camera"))
	if !found {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "camera not found"})
		return
	}
	opts := frigateOptions{Roles: parseRoles(r), BrowserAudio: browserAudioAvailable(streamMgr)}
	_, gatewayPort := getGatewayBaseURL(r)

	camerasMap := make(map[string]frigateCameraConfig, len(cams))
	streams := make([]frigateStreamEntry, 0, len(cams))
	for _, c := range cams {
		camerasMap[c.Key] = frigateConfigFor(c, opts)
		streams = append(streams, frigateStreamEntry{
			UUID: c.ID, Name: c.Name, Model: c.Model, Streaming: c.Streaming, RTSPURL: c.RTSPURL, HLSURL: c.HLSURL,
			PortMappings: streamPortMappings{
				RTSP: extractPortFromURL(c.RTSPURL, 8554), HLS: extractPortFromURL(c.HLSURL, 8888), Gateway: gatewayPort, RTP: c.RTPPort,
			},
		})
	}
	existing := generateFrigateYAML(camerasMap, opts)
	newOpts := opts
	newOpts.NewConfig = true
	fresh := generateFrigateYAML(camerasMap, newOpts)

	if f := wantFormat(r); f == "yaml" || f == "text" {
		w.Header().Set("Content-Type", "text/yaml; charset=utf-8")
		if strings.EqualFold(r.URL.Query().Get("variant"), "new") {
			_, _ = w.Write([]byte(fresh))
		} else {
			_, _ = w.Write([]byte(existing))
		}
		return
	}
	writeJSON(w, http.StatusOK, frigateIntegrationResponse{
		Integration:   "frigate",
		Version:       FrigateDocsVersion,
		GeneratedAt:   time.Now().UTC().Format(time.RFC3339),
		Cameras:       camerasMap,
		Streams:       streams,
		PortMappings:  streamMgr.portMappings(r),
		ConfigYAML:    existing,
		ConfigYAMLNew: fresh,
	})
}

// ----------------------------------------------------------------------------
// Home Assistant
// ----------------------------------------------------------------------------

type haGatewayInfo struct {
	BaseURL  string `json:"base_url"`
	RTSPBase string `json:"rtsp_base"`
	HLSBase  string `json:"hls_base"`
}

// haGenericCamera holds the values to type into Home Assistant's Generic
// Camera form (Settings > Devices & services > Add integration).
type haGenericCamera struct {
	StillImageURL string `json:"still_image_url"`
	StreamSource  string `json:"stream_source"`
	RTSPTransport string `json:"rtsp_transport"`
	FrameRate     int    `json:"framerate"`
	VerifySSL     bool   `json:"verify_ssl"`
}

// haControls says how Home Assistant reaches the camera controls: through
// MQTT discovery. BombeCam's REST control API is only on this PC.
type haControls struct {
	MQTTDiscovery bool   `json:"mqtt_discovery"`
	Note          string `json:"note"`
}

type haCameraConfig struct {
	UUID                string             `json:"uuid"`
	Name                string             `json:"name"`
	Model               string             `json:"model"`
	StreamName          string             `json:"stream_name"`
	Streaming           bool               `json:"streaming"`
	RTSPURL             string             `json:"rtsp_url"`
	RTSPURLByID         string             `json:"rtsp_url_by_id"`
	LiveStreamURL       string             `json:"live_stream_url"`
	StillImageURL       *string            `json:"still_image_url"`
	StillImageAvailable bool               `json:"still_image_available"`
	RecordingAvailable  bool               `json:"recording_available"`
	LatestRecordingURL  *string            `json:"latest_recording_url"`
	VideoCodec          string             `json:"video_codec"`
	AudioCodec          string             `json:"audio_codec"`
	PortMappings        streamPortMappings `json:"port_mappings"`
	GenericCamera       haGenericCamera    `json:"generic_camera"`
	Controls            haControls         `json:"controls"`
	SetupSteps          string             `json:"setup_steps"`
}

type haIntegrationResponse struct {
	Integration  string           `json:"integration"`
	GeneratedAt  string           `json:"generated_at"`
	Gateway      haGatewayInfo    `json:"gateway"`
	TotalCameras int              `json:"total_cameras"`
	Cameras      []haCameraConfig `json:"cameras"`
	// SetupSteps: plain-text Generic Camera steps for every camera.
	SetupSteps string `json:"setup_steps"`
}

func haControlsInfo() haControls {
	s := currentIntegrationSettings()
	if s.MQTT.Enabled {
		return haControls{MQTTDiscovery: true, Note: "Controls and status appear in Home Assistant through MQTT discovery (device \"BombeCam <camera>\")."}
	}
	return haControls{Note: "Turn on Home Assistant MQTT on BombeCam's Frigate / Home Assistant page to get the camera controls in Home Assistant."}
}

// haSetupSteps is the Generic Camera walk-through for one camera.
func haSetupSteps(c integrationCamera, g haGenericCamera) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "Home Assistant: add %q (Generic Camera)\n", c.Name)
	sb.WriteString("1. In Home Assistant, open Settings > Devices & services.\n")
	sb.WriteString("2. Click \"Add integration\", search for \"Generic Camera\" and select it.\n")
	sb.WriteString("3. Fill in:\n")
	fmt.Fprintf(&sb, "   Stream source URL:        %s\n", g.StreamSource)
	if g.StillImageURL != "" {
		fmt.Fprintf(&sb, "   Still image URL:          %s\n", g.StillImageURL)
	} else {
		sb.WriteString("   Still image URL:          leave empty (Home Assistant takes pictures from the stream)\n")
	}
	sb.WriteString("   Username and password:    leave empty")
	if strings.Contains(g.StreamSource, "@") {
		sb.WriteString(" (the URLs already carry them)")
	}
	sb.WriteString("\n   More options:             keep the defaults (RTSP transport protocol is already TCP)\n")
	sb.WriteString("4. Submit. When the preview shows the picture, tick \"Everything looks good.\" and submit again.\n")
	sb.WriteString("If the live view stutters or restarts: open the camera's Configure > More options and turn on \"Use wallclock as timestamps\".\n")
	return sb.String()
}

// handleHomeAssistantIntegration handles GET /api/v1/integrations/homeassistant.
// Home Assistant deprecated YAML setup of Generic Camera, so this returns the
// values for its UI (and ?format=text the step-by-step text).
func handleHomeAssistantIntegration(w http.ResponseWriter, r *http.Request, streamMgr *StreamManager, optPM ...profile.ProfileManager) {
	if !validateOperatorRequest(w, r) {
		return
	}

	if r.Method != http.MethodGet {
		http.Error(w, "GET required", http.StatusMethodNotAllowed)
		return
	}
	cams, found := collectIntegrationCameras(streamMgr, r, r.URL.Query().Get("camera"))
	if !found {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "camera not found"})
		return
	}
	gatewayBase, gatewayPort := getGatewayBaseURL(r)
	controls := haControlsInfo()
	out := make([]haCameraConfig, 0, len(cams))
	var all strings.Builder
	for i, c := range cams {
		g := haGenericCamera{StillImageURL: c.SnapshotURL, StreamSource: c.RTSPURL, RTSPTransport: "tcp", FrameRate: 2}
		var still *string
		if c.SnapshotURL != "" {
			s := c.SnapshotURL
			still = &s
		}
		steps := haSetupSteps(c, g)
		if i > 0 {
			all.WriteString("\n")
		}
		all.WriteString(steps)
		cam := haCameraConfig{
			UUID: c.ID, Name: c.Name, Model: c.Model, StreamName: c.StreamName, Streaming: c.Streaming,
			RTSPURL: c.RTSPURL, RTSPURLByID: c.RTSPURLByID, LiveStreamURL: c.HLSURL,
			StillImageURL: still, StillImageAvailable: still != nil,
			VideoCodec: "h264", AudioCodec: "aac",
			PortMappings: streamPortMappings{
				RTSP: extractPortFromURL(c.RTSPURL, 8554), HLS: extractPortFromURL(c.HLSURL, 8888), Gateway: gatewayPort, RTP: c.RTPPort,
			},
			GenericCamera: g,
			Controls:      controls,
			SetupSteps:    steps,
		}
		if rel := latestRecording(c.ID); rel != "" {
			cam.RecordingAvailable = true
			u := strings.TrimRight(gatewayBase, "/") + "/api/v1/recordings/download?path=" + url.QueryEscape(rel)
			cam.LatestRecordingURL = &u
		}
		out = append(out, cam)
	}
	if len(cams) == 0 {
		all.WriteString("No cameras have been added to BombeCam yet.\n")
	}
	if f := wantFormat(r); f == "yaml" || f == "text" {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte(all.String()))
		return
	}
	writeJSON(w, http.StatusOK, haIntegrationResponse{
		Integration: "homeassistant",
		GeneratedAt: time.Now().UTC().Format(time.RFC3339),
		Gateway: haGatewayInfo{
			BaseURL:  gatewayBase,
			RTSPBase: streamMgr.ConsumerRTSPBaseURL(r),
			HLSBase:  streamMgr.ConsumerHLSBaseURL(r),
		},
		TotalCameras: len(out),
		Cameras:      out,
		SetupSteps:   all.String(),
	})
}

// ----------------------------------------------------------------------------
// Streams
// ----------------------------------------------------------------------------

type streamsStreamEntry struct {
	UUID             string             `json:"uuid"`
	Name             string             `json:"name"`
	Model            string             `json:"model"`
	StreamName       string             `json:"stream_name"`
	Streaming        bool               `json:"streaming"`
	PublicationReady bool               `json:"publication_ready"`
	RTSPURL          string             `json:"rtsp_url"`
	RTSPURLByID      string             `json:"rtsp_url_by_id"`
	HLSURL           string             `json:"hls_url"`
	SnapshotURL      string             `json:"snapshot_url,omitempty"`
	Transport        string             `json:"transport"`
	VideoCodec       string             `json:"video_codec"`
	AudioCodec       string             `json:"audio_codec"`
	Width            int                `json:"width,omitempty"`
	Height           int                `json:"height,omitempty"`
	FPS              float64            `json:"fps,omitempty"`
	PortMappings     streamPortMappings `json:"port_mappings"`
	FreshnessTS      string             `json:"freshness_ts"`
	IP               string             `json:"ip,omitempty"`
	MAC              string             `json:"mac,omitempty"`
}

type streamsIntegrationResponse struct {
	TotalCameras  int                  `json:"total_cameras"`
	ActiveStreams int                  `json:"active_streams"`
	PortMappings  globalPortMappings   `json:"port_mappings"`
	Streams       []streamsStreamEntry `json:"streams"`
}

// handleStreamsIntegration handles GET /api/v1/integrations/streams
func handleStreamsIntegration(w http.ResponseWriter, r *http.Request, streamMgr *StreamManager, optPM ...profile.ProfileManager) {
	if !validateOperatorRequest(w, r) {
		return
	}

	if r.Method != http.MethodGet {
		http.Error(w, "GET required", http.StatusMethodNotAllowed)
		return
	}
	activeOnly := strings.EqualFold(r.URL.Query().Get("active_only"), "true") || r.URL.Query().Get("active_only") == "1"
	cams, _ := collectIntegrationCameras(streamMgr, r, "")
	_, gatewayPort := getGatewayBaseURL(r)

	resp := streamsIntegrationResponse{TotalCameras: len(cams), PortMappings: streamMgr.portMappings(r), Streams: []streamsStreamEntry{}}
	for _, c := range cams {
		if c.Streaming {
			resp.ActiveStreams++
		}
		if activeOnly && !c.Streaming {
			continue
		}
		mc, ok := streamMgr.GetCamera(c.ID)
		if !ok {
			continue
		}
		mc.mu.RLock()
		transport := mc.Transport
		freshness := ""
		if !mc.Freshness.IsZero() {
			freshness = mc.Freshness.UTC().Format(time.RFC3339)
		}
		camIP := mc.IP
		mc.mu.RUnlock()
		if transport == "" {
			transport = "unknown"
		}
		camMAC := ""
		if camIP != "" {
			camMAC = lookupMAC(camIP)
		}
		e := streamsStreamEntry{
			UUID: c.ID, Name: c.Name, Model: c.Model, StreamName: c.StreamName,
			Streaming: c.Streaming, PublicationReady: c.Streaming,
			RTSPURL: c.RTSPURL, RTSPURLByID: c.RTSPURLByID, HLSURL: c.HLSURL, SnapshotURL: c.SnapshotURL,
			Transport: transport, VideoCodec: "h264", AudioCodec: "aac",
			PortMappings: streamPortMappings{
				RTSP: extractPortFromURL(c.RTSPURL, 8554), HLS: extractPortFromURL(c.HLSURL, 8888), Gateway: gatewayPort, RTP: c.RTPPort,
			},
			FreshnessTS: freshness, IP: camIP, MAC: camMAC,
		}
		if c.Video != nil {
			e.Width, e.Height, e.FPS = c.Video.Width, c.Video.Height, c.Video.FPS
			if e.FPS == 0 {
				e.FPS = c.Video.SPSFPS
			}
		}
		resp.Streams = append(resp.Streams, e)
	}
	writeJSON(w, http.StatusOK, resp)
}
