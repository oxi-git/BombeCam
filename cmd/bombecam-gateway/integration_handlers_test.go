package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/Fever-r/BombeCam/pkg/bridge"
	"github.com/Fever-r/BombeCam/pkg/profile"
)

// resetIntegrations puts the Frigate / Home Assistant settings back to
// defaults when the test ends (they are process-wide).
func resetIntegrations(t *testing.T) {
	t.Helper()
	t.Cleanup(func() { storeIntegrationSettings(profile.IntegrationSettings{}); setPortsLocked(false, "") })
}

func setupIntegrationsTestEnv(t *testing.T) (*StreamManager, *http.ServeMux, profile.ProfileManager) {
	t.Helper()
	withLAN(t, LANAddress{IP: "192.168.1.20", Interface: "Wi-Fi", Default: true}, LANAddress{IP: "10.0.0.5", Interface: "Ethernet"})
	resetIntegrations(t)
	_, streamMgr, pm, _, mux, _ := setupFullTestEnvironment(t)
	streamMgr.SetPublisher(bridge.PublisherNative) // FFmpeg found + built-in publisher: G.711 browser audio

	// Enroll standard test cameras
	devs := []bridge.Device{
		{
			UUID: "cam-uuid-1",
			Name: "Front Porch (2nd)",
			Type: "WS03",
		},
		{
			UUID: "cam-uuid-2",
			Name: "1_cam",
			Type: "WS04",
		},
	}
	if _, err := streamMgr.Enroll([]string{"cam-uuid-1", "cam-uuid-2"}, devs); err != nil {
		t.Fatalf("failed to enroll test cameras: %v", err)
	}

	// Mark cam-uuid-1 as actively streaming
	streamMgr.SetStreamActive("cam-uuid-1", true, nil, nil, "Local MQTTS (:8883)")

	return streamMgr, mux, pm
}

// 1. Frigate JSON: URLs other devices can use, a go2rtc stream per camera
// named like the camera, the camera's AAC kept for recordings, no mqtt:.
func TestIntegrations_Frigate_JSON(t *testing.T) {
	_, mux, _ := setupIntegrationsTestEnv(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/integrations/frigate", nil)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d: %s", rr.Code, rr.Body.String())
	}
	if ct := rr.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("expected Content-Type application/json, got %q", ct)
	}
	var resp frigateIntegrationResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to unmarshal JSON response: %v", err)
	}
	if resp.Integration != "frigate" || resp.Version != FrigateDocsVersion {
		t.Errorf("integration %q version %q", resp.Integration, resp.Version)
	}
	if len(resp.Cameras) != 2 {
		t.Fatalf("expected 2 cameras in Frigate config, got %d", len(resp.Cameras))
	}
	cam1, ok := resp.Cameras["front_porch_2nd"]
	if !ok {
		t.Fatalf("expected camera key 'front_porch_2nd' not found in %v", resp.Cameras)
	}
	if cam1.UUID != "cam-uuid-1" || !cam1.Streaming {
		t.Errorf("camera 1: %+v", cam1)
	}
	// the PC's LAN address (default route), never 127.0.0.1
	if cam1.RTSPURL != "rtsp://192.168.1.20:8554/cam-uuid-1" {
		t.Errorf("unexpected RTSP URL: %q", cam1.RTSPURL)
	}
	if len(cam1.FFmpeg.Inputs) != 1 || cam1.FFmpeg.Inputs[0].Path != cam1.RTSPURL ||
		strings.Join(cam1.FFmpeg.Inputs[0].Roles, ",") != "detect,record" {
		t.Errorf("unexpected FFmpeg inputs: %+v", cam1.FFmpeg)
	}
	if cam1.FFmpeg.OutputArgs["record"] != "preset-record-generic-audio-copy" {
		t.Errorf("recordings must keep the camera's AAC: %+v", cam1.FFmpeg.OutputArgs)
	}
	if !cam1.Detect.Enabled || cam1.Detect.FPS != 5 || cam1.Detect.Width != 0 || cam1.Detect.Height != 0 {
		t.Errorf("detect: %+v (size must be left to Frigate)", cam1.Detect)
	}
	// FFmpeg on this PC: go2rtc gets G.711 for WebRTC from BombeCam directly
	if len(cam1.Go2RTCSources) != 1 || cam1.Go2RTCSources[0] != cam1.RTSPURL {
		t.Errorf("go2rtc sources %v", cam1.Go2RTCSources)
	}
	if cam2, ok := resp.Cameras["cam_1_cam"]; !ok || cam2.Streaming {
		t.Errorf("camera 2 missing or streaming: %v", resp.Cameras)
	}
	if resp.PortMappings.RTSP != 8554 || resp.PortMappings.HLS != 8888 || resp.PortMappings.WebRTC != 8889 {
		t.Errorf("unexpected global port mappings: %+v", resp.PortMappings)
	}
	if len(resp.Streams) != 2 {
		t.Fatalf("expected 2 streams, got %d", len(resp.Streams))
	}
	for _, s := range resp.Streams {
		if s.PortMappings.RTSP != 8554 || s.PortMappings.HLS != 8888 || s.PortMappings.RTP == 0 {
			t.Errorf("invalid stream port mappings for %s: %+v", s.UUID, s.PortMappings)
		}
	}
	// "add to my config" never touches mqtt:, which Home Assistant's Frigate integration needs
	if strings.Contains(resp.ConfigYAML, "mqtt:") {
		t.Errorf("config_yaml must not contain an mqtt: block:\n%s", resp.ConfigYAML)
	}
	for _, want := range []string{"go2rtc:\n  streams:\n    cam_1_cam:\n", "    front_porch_2nd:\n      - rtsp://192.168.1.20:8554/cam-uuid-1\n", "\ncameras:\n  cam_1_cam: # 1_cam\n"} {
		if !strings.Contains(resp.ConfigYAML, want) {
			t.Errorf("config_yaml lacks %q:\n%s", want, resp.ConfigYAML)
		}
	}
	if strings.Contains(resp.ConfigYAML, "live:") || strings.Contains(resp.ConfigYAML, "stream_name") {
		t.Errorf("the old live: stream_name key is gone in current Frigate:\n%s", resp.ConfigYAML)
	}
	if !strings.HasPrefix(resp.ConfigYAMLNew, "# Frigate configuration made by BombeCam") ||
		!strings.Contains(resp.ConfigYAMLNew, "\nmqtt:\n  enabled: false\n\nrecord:\n  enabled: true\n  motion:\n    days: 7 # keep footage with motion for a week\n\ngo2rtc:\n") {
		t.Errorf("config_yaml_new:\n%s", resp.ConfigYAMLNew)
	}

	// custom roles
	rrCustom := httptest.NewRecorder()
	mux.ServeHTTP(rrCustom, httptest.NewRequest(http.MethodGet, "/api/v1/integrations/frigate?roles=record,audio", nil))
	var respCustom frigateIntegrationResponse
	_ = json.Unmarshal(rrCustom.Body.Bytes(), &respCustom)
	if c, ok := respCustom.Cameras["front_porch_2nd"]; !ok || strings.Join(c.FFmpeg.Inputs[0].Roles, ",") != "record,audio" {
		t.Errorf("expected custom roles [record audio], got %+v", respCustom.Cameras)
	}
}

// 2. Frigate YAML: the exact text for a camera with a friendly stream name.
func TestIntegrations_Frigate_YAML(t *testing.T) {
	streamMgr, mux, _ := setupIntegrationsTestEnv(t)
	streamMgr.SetStreamNames(map[string]string{"cam-uuid-1": "front_porch"})

	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/v1/integrations/frigate?format=yaml&camera=front_porch", nil))
	if rr.Code != http.StatusOK || !strings.HasPrefix(rr.Header().Get("Content-Type"), "text/yaml") {
		t.Fatalf("got %d %q", rr.Code, rr.Header().Get("Content-Type"))
	}
	want := `# BombeCam cameras for Frigate (checked against Frigate ` + FrigateDocsVersion + `).
# Add these to your Frigate config. If it already has a go2rtc: or cameras:
# section, put these entries under the existing one: each key may appear once.
go2rtc:
  streams:
    front_porch:
      - rtsp://192.168.1.20:8554/front_porch

cameras:
  front_porch: # Front Porch (2nd)
    ffmpeg:
      inputs:
        - path: rtsp://192.168.1.20:8554/front_porch
          roles:
            - detect
            - record
      output_args:
        record: preset-record-generic-audio-copy
    detect:
      enabled: true
      fps: 5
    record:
      enabled: true
`
	if got := rr.Body.String(); got != want {
		t.Fatalf("YAML:\n%s\nwant:\n%s", got, want)
	}

	// the full-config variant, and Accept: application/x-yaml
	req := httptest.NewRequest(http.MethodGet, "/api/v1/integrations/frigate?variant=new", nil)
	req.Header.Set("Accept", "application/x-yaml")
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if body := rr.Body.String(); !strings.HasPrefix(rr.Header().Get("Content-Type"), "text/yaml") || !strings.Contains(body, "mqtt:\n  enabled: false") || !strings.Contains(body, "front_porch:") || !strings.Contains(body, "cam_1_cam:") {
		t.Fatalf("new config variant:\n%s", body)
	}

	// without FFmpeg on this PC, go2rtc makes Opus for WebRTC viewers itself
	streamMgr.SetPublisher(bridge.PublisherFFmpeg)
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/v1/integrations/frigate?format=yaml&camera=cam-uuid-1", nil))
	if !strings.Contains(rr.Body.String(), "      - rtsp://192.168.1.20:8554/front_porch\n      - \"ffmpeg:front_porch#audio=opus\"\n") {
		t.Fatalf("expected an Opus source for go2rtc:\n%s", rr.Body.String())
	}
}

func TestFrigateYAML_DetectSizeHintAndQuoting(t *testing.T) {
	cams := map[string]frigateCameraConfig{
		"attic": {Name: "attic", RTSPURL: "rtsp://u:p@10.0.0.5:8554/attic", Go2RTCSources: []string{"rtsp://u:p@10.0.0.5:8554/attic"},
			FFmpeg: frigateFFmpeg{Inputs: []frigateFFmpegInput{{Path: "rtsp://u:p@10.0.0.5:8554/attic", Roles: []string{"detect"}}}, OutputArgs: map[string]string{"record": "preset-record-generic-audio-copy"}},
			Detect: frigateDetect{Enabled: true, FPS: 5}, Video: &bridge.VideoInfo{Width: 2304, Height: 1296}},
		"odd": {Name: "Odd #1", Go2RTCSources: []string{"rtsp://host/a b"}, FFmpeg: frigateFFmpeg{Inputs: []frigateFFmpegInput{{Path: "rtsp://host/a b", Roles: []string{"record"}}}, OutputArgs: map[string]string{"record": "x"}}, Detect: frigateDetect{FPS: 5}},
	}
	y := generateFrigateYAML(cams, frigateOptions{})
	for _, want := range []string{
		"      # The camera sends 2304x1296; to detect on a smaller picture (less CPU):\n      # width: 1280\n      # height: 720\n",
		"        - path: rtsp://u:p@10.0.0.5:8554/attic\n",
		`        - path: "rtsp://host/a b"`,
		"  odd: # Odd #1\n",
	} {
		if !strings.Contains(y, want) {
			t.Errorf("missing %q in:\n%s", want, y)
		}
	}
	if w, h := smallerDetectSize(1920, 1080); w != 1280 || h != 720 {
		t.Errorf("1080p -> %dx%d", w, h)
	}
	if w, _ := smallerDetectSize(1280, 720); w != 0 {
		t.Error("720p needs no hint")
	}
	if y := generateFrigateYAML(nil, frigateOptions{}); !strings.Contains(y, "No cameras have been added") {
		t.Errorf("empty: %s", y)
	}
}

// 3. Identifier sanitization exhaustive tests
func TestIntegrations_Frigate_Sanitization(t *testing.T) {
	tests := []struct {
		name     string
		uuid     string
		expected string
	}{
		{"Front Porch (2nd)", "uuid-1", "front_porch_2nd"},
		{"1_cam", "uuid-2", "cam_1_cam"},
		{"2nd_floor", "uuid-3", "cam_2nd_floor"},
		{"Backyard - PTZ Cam #3!", "uuid-4", "backyard_ptz_cam_3"},
		{"Living Room / High Res", "uuid-5", "living_room_high_res"},
		{"---", "cam-uuid-special", "camera_cam_uuid_special"},
		{"", "1234-5678", "cam_1234_5678"},
		{"", "", "camera_default"},
		{"   spaced camera   ", "uuid-6", "spaced_camera"},
		{"cam_already_clean", "uuid-7", "cam_already_clean"},
		{"A.B.C..D", "uuid-8", "a_b_c_d"},
	}

	validKeyRegex := regexp.MustCompile(`^[a-zA-Z0-9_]+$`)

	for _, tc := range tests {
		actual := sanitizeFrigateIdentifier(tc.name, tc.uuid)
		if actual != tc.expected {
			t.Errorf("sanitizeFrigateIdentifier(%q, %q) = %q; want %q", tc.name, tc.uuid, actual, tc.expected)
		}
		if !validKeyRegex.MatchString(actual) {
			t.Errorf("sanitized key %q does not match regex ^[a-zA-Z0-9_]+$", actual)
		}
		if actual[0] >= '0' && actual[0] <= '9' {
			t.Errorf("sanitized key %q starts with a digit", actual)
		}
	}
}

// 4. Home Assistant: Generic Camera values and UI steps (YAML setup of
// Generic Camera is deprecated), no control endpoints other devices can't reach.
func TestIntegrations_HomeAssistant_JSON(t *testing.T) {
	_, mux, _ := setupIntegrationsTestEnv(t)

	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/v1/integrations/homeassistant", nil))
	if rr.Code != http.StatusOK || !strings.HasPrefix(rr.Header().Get("Content-Type"), "application/json") {
		t.Fatalf("got %d %q: %s", rr.Code, rr.Header().Get("Content-Type"), rr.Body.String())
	}
	var resp haIntegrationResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to unmarshal JSON response: %v", err)
	}
	if resp.Integration != "homeassistant" || resp.Gateway.RTSPBase != "rtsp://192.168.1.20:8554" {
		t.Errorf("header: %+v", resp)
	}
	if len(resp.Cameras) != 2 {
		t.Fatalf("expected 2 cameras, got %d", len(resp.Cameras))
	}
	body := rr.Body.String()
	for _, gone := range []string{"platform: generic", "ptz_services", "control_services", "/api/v1/cameras/cam-uuid-1/ptz", "configuration_yaml"} {
		if strings.Contains(body, gone) {
			t.Errorf("the Home Assistant answer still contains %q", gone)
		}
	}
	for _, cam := range resp.Cameras {
		g := cam.GenericCamera
		if g.StreamSource != cam.RTSPURL || !strings.HasPrefix(cam.RTSPURL, "rtsp://192.168.1.20:8554/") || g.RTSPTransport != "tcp" {
			t.Errorf("generic camera values for %s: %+v", cam.UUID, g)
		}
		if cam.StillImageURL != nil || cam.StillImageAvailable || g.StillImageURL != "" {
			t.Errorf("no snapshot URL while snapshots are off: %+v", cam)
		}
		if cam.VideoCodec != "h264" || cam.AudioCodec != "aac" {
			t.Errorf("unexpected codecs: %s / %s", cam.VideoCodec, cam.AudioCodec)
		}
		for _, want := range []string{"Settings > Devices & services", `"Generic Camera"`, "Stream source URL:        " + cam.RTSPURL, "RTSP transport protocol is already TCP", "Still image URL:          leave empty", `"Everything looks good."`, "Use wallclock as timestamps"} {
			if !strings.Contains(cam.SetupSteps, want) {
				t.Errorf("steps for %s lack %q:\n%s", cam.UUID, want, cam.SetupSteps)
			}
		}
		if cam.Controls.MQTTDiscovery || !strings.Contains(cam.Controls.Note, "MQTT") {
			t.Errorf("controls: %+v", cam.Controls)
		}
	}
	if !strings.Contains(resp.SetupSteps, `add "Front Porch (2nd)"`) || !strings.Contains(resp.SetupSteps, `add "1_cam"`) {
		t.Errorf("setup_steps:\n%s", resp.SetupSteps)
	}

	// with snapshots and MQTT on, the still image URL and the MQTT note appear
	s := currentIntegrationSettings()
	s.Snapshots, s.MQTT.Enabled, s.MQTT.Host = true, true, "10.0.0.2"
	storeIntegrationSettings(s)
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/v1/integrations/homeassistant?camera=cam-uuid-1", nil))
	resp = haIntegrationResponse{}
	_ = json.Unmarshal(rr.Body.Bytes(), &resp)
	if len(resp.Cameras) != 1 || resp.Cameras[0].StillImageURL == nil ||
		*resp.Cameras[0].StillImageURL != "http://192.168.1.20:8655/api/v1/cameras/cam-uuid-1/snapshot.jpg" || !resp.Cameras[0].Controls.MQTTDiscovery {
		t.Fatalf("with snapshots and MQTT: %s", rr.Body.String())
	}
}

// TestHomeAssistantContractPinsAPI parses the raw /api/v1/integrations/
// homeassistant answer as plain JSON and pins the exact field names the
// Home Assistant integration (ha-bombecam) reads in api.py. Renaming any of
// these fields in a struct must fail this test, so the two projects stay in
// step. This is a contract test, not a behaviour one: it does not care what
// the values are, only that the field names survive.
func TestHomeAssistantContractPinsAPI(t *testing.T) {
	_, mux, _ := setupIntegrationsTestEnv(t)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/v1/integrations/homeassistant", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var raw map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &raw); err != nil {
		t.Fatalf("bad JSON: %v", err)
	}

	// Gateway block.
	gateway, ok := raw["gateway"].(map[string]any)
	if !ok {
		t.Fatal(`payload has no "gateway" object`)
	}
	for _, field := range []string{"base_url", "rtsp_base", "hls_base"} {
		if _, exists := gateway[field]; !exists {
			t.Errorf(`gateway lacks the Home Assistant contract field %q`, field)
		}
	}

	// Each camera block: check only that the camera has the fields HA reads.
	rawCams, ok := raw["cameras"].([]any)
	if !ok || len(rawCams) == 0 {
		t.Fatal(`payload has no "cameras" array`)
	}
	for _, rc := range rawCams {
		cam, ok := rc.(map[string]any)
		if !ok {
			t.Fatal("a camera entry is not an object")
		}
		for _, field := range []string{"uuid", "name", "model", "streaming", "rtsp_url", "still_image_url", "stream_name", "recording_available", "latest_recording_url"} {
			if _, exists := cam[field]; !exists {
				t.Errorf(`camera lacks the Home Assistant contract field %q`, field)
			}
		}
	}
}

// TestHomeAssistantContractRecording pins recording_available / latest_recording_url
// once a recorded segment exists for a camera.
func TestHomeAssistantContractRecording(t *testing.T) {
	_, mux, _ := setupIntegrationsTestEnv(t)

	// A real recording for cam-uuid-1 (the publish path is the UUID).
	root := t.TempDir()
	camDir := root + "/cam-uuid-1"
	if err := os.MkdirAll(camDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(camDir+"/2026-11-13_10-00-00.mp4", []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	setRecordingsRoot(root)
	defer setRecordingsRoot("")

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/integrations/homeassistant?camera=cam-uuid-1", nil)
	req.Host = "192.0.2.10:8654"
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var resp haIntegrationResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Cameras) != 1 {
		t.Fatalf("expected 1 camera, got %d", len(resp.Cameras))
	}
	cam := resp.Cameras[0]
	if !cam.RecordingAvailable || cam.LatestRecordingURL == nil {
		t.Fatalf("recording fields missing: %+v", cam)
	}
	want := "http://192.0.2.10:8654/api/v1/recordings/download?path=cam-uuid-1%2F2026-11-13_10-00-00.mp4"
	if *cam.LatestRecordingURL != want {
		t.Errorf("latest_recording_url = %q, want %q", *cam.LatestRecordingURL, want)
	}
}

// TestShadowContractPinsAPI pins the raw /api/v1/cameras/{uuid}/shadow answer
// the Home Assistant integration reads in api.async_get_shadow:
// state.reported with the five attribute names the switches/select use.
func TestShadowContractPinsAPI(t *testing.T) {
	// A fake control channel that answers camera readback through
	// GetAttributes, the path the production SignalingControlChannel uses for
	// the detection switches (the plain mock returns nothing for them).
	fake := &attrFakeControl{reported: map[string]any{
		"IrLedMode": float64(1), "LedOnOff": float64(1), "LightSW": float64(1),
		"MotionDetectSW": float64(1), "SoundDetectSW": float64(0),
	}}

	sm, mux, _ := setupIntegrationsTestEnv(t)
	if _, err := sm.Enroll([]string{"cam-uuid-1"}, []bridge.Device{{UUID: "cam-uuid-1", Name: "Porch", Type: "WS03"}}); err != nil {
		t.Fatalf("enroll: %v", err)
	}
	// After setup so setupFullTestEnvironment's generic mock does not wipe it.
	SetGatewayControlChannel(fake)
	defer SetGatewayControlChannel(nil)

	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/v1/cameras/cam-uuid-1/shadow", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var raw map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &raw); err != nil {
		t.Fatalf("bad JSON: %v", err)
	}
	state, ok := raw["state"].(map[string]any)
	if !ok {
		t.Fatal(`shadow has no "state" object`)
	}
	reported, ok := state["reported"].(map[string]any)
	if !ok {
		t.Fatal(`state has no "reported" object`)
	}
	for _, attr := range []string{"IrLedMode", "LedOnOff", "LightSW", "MotionDetectSW", "SoundDetectSW"} {
		if _, exists := reported[attr]; !exists {
			t.Errorf(`shadow lacks the Home Assistant contract attribute %q`, attr)
		}
	}
}

// attrFakeControl is a ControlChannel that answers readCameraAttributes'
// GetAttributes branch, mimicking the real SignalingControlChannel.
type attrFakeControl struct {
	bridge.ControlChannel
	reported map[string]any
}

func (f *attrFakeControl) GetAttributes(_ context.Context, _ string, _ ...string) (map[string]any, error) {
	return f.reported, nil
}

// 5. Home Assistant as text (?format=text, and ?format=yaml for old callers).
func TestIntegrations_HomeAssistant_Text(t *testing.T) {
	_, mux, _ := setupIntegrationsTestEnv(t)
	for _, q := range []string{"format=text", "format=yaml"} {
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/v1/integrations/homeassistant?"+q, nil))
		if rr.Code != http.StatusOK || !strings.HasPrefix(rr.Header().Get("Content-Type"), "text/plain") {
			t.Fatalf("%s: %d %q", q, rr.Code, rr.Header().Get("Content-Type"))
		}
		body := rr.Body.String()
		if !strings.Contains(body, "Stream source URL:        rtsp://192.168.1.20:8554/cam-uuid-1") || strings.Contains(body, "platform: generic") {
			t.Fatalf("%s:\n%s", q, body)
		}
	}
}

// 6. Streams listing and active_only filtering
func TestIntegrations_Streams_Listing_And_ActiveFilter(t *testing.T) {
	_, mux, _ := setupIntegrationsTestEnv(t)

	// A. Default query: returns all enrolled cameras
	reqAll := httptest.NewRequest(http.MethodGet, "/api/v1/integrations/streams", nil)
	rrAll := httptest.NewRecorder()
	mux.ServeHTTP(rrAll, reqAll)

	if rrAll.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d: %s", rrAll.Code, rrAll.Body.String())
	}
	var respAll streamsIntegrationResponse
	if err := json.Unmarshal(rrAll.Body.Bytes(), &respAll); err != nil {
		t.Fatalf("failed to unmarshal JSON: %v", err)
	}

	if respAll.TotalCameras != 2 {
		t.Errorf("expected total_cameras 2, got %d", respAll.TotalCameras)
	}
	if respAll.ActiveStreams != 1 {
		t.Errorf("expected active_streams 1, got %d", respAll.ActiveStreams)
	}
	if len(respAll.Streams) != 2 {
		t.Fatalf("expected 2 streams in listing, got %d", len(respAll.Streams))
	}
	for _, s := range respAll.Streams {
		if s.PortMappings.RTSP != 8554 || s.PortMappings.HLS != 8888 || s.PortMappings.RTP == 0 {
			t.Errorf("invalid port mappings for stream %s: %+v", s.UUID, s.PortMappings)
		}
	}

	// B. active_only=true: returns only active streaming cameras
	reqActive := httptest.NewRequest(http.MethodGet, "/api/v1/integrations/streams?active_only=true", nil)
	rrActive := httptest.NewRecorder()
	mux.ServeHTTP(rrActive, reqActive)

	if rrActive.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d: %s", rrActive.Code, rrActive.Body.String())
	}
	var respActive streamsIntegrationResponse
	if err := json.Unmarshal(rrActive.Body.Bytes(), &respActive); err != nil {
		t.Fatalf("failed to unmarshal JSON: %v", err)
	}

	if respActive.TotalCameras != 2 {
		t.Errorf("expected total_cameras 2, got %d", respActive.TotalCameras)
	}
	if respActive.ActiveStreams != 1 {
		t.Errorf("expected active_streams 1, got %d", respActive.ActiveStreams)
	}
	if len(respActive.Streams) != 1 {
		t.Fatalf("expected exactly 1 active stream, got %d", len(respActive.Streams))
	}
	if respActive.Streams[0].UUID != "cam-uuid-1" {
		t.Errorf("expected active stream to be cam-uuid-1, got %s", respActive.Streams[0].UUID)
	}
	if !respActive.Streams[0].Streaming || !respActive.Streams[0].PublicationReady {
		t.Errorf("expected streaming and publication_ready to be true for cam-uuid-1")
	}
}

// 7. Camera filter query parameter (?camera=uuid or name) and 404 for unknown camera
func TestIntegrations_CameraFilter(t *testing.T) {
	_, mux, _ := setupIntegrationsTestEnv(t)

	// Filter on Frigate by UUID
	reqFrigateUUID := httptest.NewRequest(http.MethodGet, "/api/v1/integrations/frigate?camera=cam-uuid-1", nil)
	rrFrigateUUID := httptest.NewRecorder()
	mux.ServeHTTP(rrFrigateUUID, reqFrigateUUID)
	if rrFrigateUUID.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d: %s", rrFrigateUUID.Code, rrFrigateUUID.Body.String())
	}
	var respF frigateIntegrationResponse
	_ = json.Unmarshal(rrFrigateUUID.Body.Bytes(), &respF)
	if len(respF.Cameras) != 1 {
		t.Errorf("expected 1 filtered camera in Frigate, got %d", len(respF.Cameras))
	}

	// Filter on Home Assistant by Name
	reqHAName := httptest.NewRequest(http.MethodGet, "/api/v1/integrations/homeassistant?camera=Front%20Porch%20(2nd)", nil)
	rrHAName := httptest.NewRecorder()
	mux.ServeHTTP(rrHAName, reqHAName)
	if rrHAName.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d: %s", rrHAName.Code, rrHAName.Body.String())
	}
	var respHA haIntegrationResponse
	_ = json.Unmarshal(rrHAName.Body.Bytes(), &respHA)
	if len(respHA.Cameras) != 1 || respHA.Cameras[0].UUID != "cam-uuid-1" {
		t.Errorf("expected 1 filtered camera cam-uuid-1, got %v", respHA.Cameras)
	}

	// 404 for unknown camera on Frigate
	reqF404 := httptest.NewRequest(http.MethodGet, "/api/v1/integrations/frigate?camera=non-existent-uuid", nil)
	rrF404 := httptest.NewRecorder()
	mux.ServeHTTP(rrF404, reqF404)
	if rrF404.Code != http.StatusNotFound {
		t.Errorf("expected 404 for unknown camera on Frigate, got %d", rrF404.Code)
	}

	// 404 for unknown camera on Home Assistant
	reqHA404 := httptest.NewRequest(http.MethodGet, "/api/v1/integrations/homeassistant?camera=non-existent-uuid", nil)
	rrHA404 := httptest.NewRecorder()
	mux.ServeHTTP(rrHA404, reqHA404)
	if rrHA404.Code != http.StatusNotFound {
		t.Errorf("expected 404 for unknown camera on Home Assistant, got %d", rrHA404.Code)
	}
}

// 8. 405 Method Not Allowed verification on all endpoints
func TestIntegrations_MethodNotAllowed(t *testing.T) {
	_, mux, _ := setupIntegrationsTestEnv(t)

	endpoints := []string{
		"/api/v1/integrations/frigate",
		"/api/v1/integrations/homeassistant",
		"/api/v1/integrations/streams",
	}

	invalidMethods := []string{
		http.MethodPost,
		http.MethodPut,
		http.MethodDelete,
		http.MethodPatch,
	}

	for _, ep := range endpoints {
		for _, method := range invalidMethods {
			req := httptest.NewRequest(method, ep, nil)
			rr := httptest.NewRecorder()
			mux.ServeHTTP(rr, req)

			if rr.Code != http.StatusMethodNotAllowed {
				t.Errorf("expected 405 Method Not Allowed for %s %s, got %d", method, ep, rr.Code)
			}
		}
	}
}

// 9. Zero secret leakage verification
func TestIntegrations_ZeroLeakage(t *testing.T) {
	_, mux, pm := setupIntegrationsTestEnv(t)

	// Save profile containing sensitive canary credentials
	canaryPassword := "CANARY_SECRET_PASSWORD_12345"
	canaryAuthToken := "CANARY_AUTH_TOKEN_99999"
	canaryRefreshToken := "CANARY_REFRESH_TOKEN_88888"
	canaryPhoneCode := "CANARY_PHONE_CODE_77777"

	prof := &profile.Profile{
		Version: 1,
		Privacy: profile.PrivacySettings{BlockCloudVideo: profile.BoolPtr(true)},
		Credentials: profile.CloudCredentials{
			AccountEmail: "user@example.com",
			Password:     profile.SecretString(canaryPassword),
			AuthToken:    profile.SecretString(canaryAuthToken),
			RefreshToken: profile.SecretString(canaryRefreshToken),
			PhoneCode:    canaryPhoneCode,
		},
	}
	if err := pm.SaveProfile(context.Background(), prof); err != nil {
		t.Fatalf("failed to save canary profile: %v", err)
	}

	endpoints := []string{
		"/api/v1/integrations/frigate",
		"/api/v1/integrations/frigate?format=yaml",
		"/api/v1/integrations/homeassistant",
		"/api/v1/integrations/homeassistant?format=yaml",
		"/api/v1/integrations/streams",
	}

	canaries := []string{
		canaryPassword,
		canaryAuthToken,
		canaryRefreshToken,
		canaryPhoneCode,
	}

	forbiddenWords := []string{
		"\"password\"",
		"\"authtoken\"",
		"\"token\"",
		"\"phonecode\"",
		"\"refresh_token\"",
	}

	for _, ep := range endpoints {
		req := httptest.NewRequest(http.MethodGet, ep, nil)
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, req)

		if rr.Code != http.StatusOK {
			t.Fatalf("failed to query %s: %d", ep, rr.Code)
		}

		body := rr.Body.String()
		lowerBody := strings.ToLower(body)

		for _, canary := range canaries {
			if strings.Contains(body, canary) {
				t.Fatalf("CRITICAL SECURITY LEAK: %s exposed canary credential %q", ep, canary)
			}
		}

		for _, word := range forbiddenWords {
			if strings.Contains(lowerBody, word) {
				t.Fatalf("POTENTIAL LEAK: %s contains forbidden credential key %s", ep, word)
			}
		}
	}
}

// 10. Concurrency stress test: 50+ concurrent requests during camera enrollment & status toggling
func TestIntegrations_ConcurrencyStress(t *testing.T) {
	streamMgr, mux, _ := setupIntegrationsTestEnv(t)

	var wg sync.WaitGroup
	numWorkers := 60
	requestsPerWorker := 30
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Background worker actively mutating camera state and enrolling new cameras
	wg.Add(1)
	go func() {
		defer wg.Done()
		toggle := false
		for i := 0; i < requestsPerWorker*2; i++ {
			select {
			case <-ctx.Done():
				return
			default:
				toggle = !toggle
				streamMgr.SetStreamActive("cam-uuid-1", toggle, nil, nil, "Local MQTTS (:8883)")
				streamMgr.UpdateTransport("cam-uuid-2", fmt.Sprintf("Transport-%d", i))
			}
		}
	}()

	endpoints := []string{
		"/api/v1/integrations/frigate",
		"/api/v1/integrations/frigate?format=yaml",
		"/api/v1/integrations/frigate?camera=cam-uuid-1",
		"/api/v1/integrations/homeassistant",
		"/api/v1/integrations/homeassistant?format=yaml",
		"/api/v1/integrations/homeassistant?camera=cam-uuid-2",
		"/api/v1/integrations/streams",
		"/api/v1/integrations/streams?active_only=true",
	}

	for w := 0; w < numWorkers; w++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			for r := 0; r < requestsPerWorker; r++ {
				ep := endpoints[(workerID+r)%len(endpoints)]
				req := httptest.NewRequest(http.MethodGet, ep, nil)
				rr := httptest.NewRecorder()
				mux.ServeHTTP(rr, req)

				if rr.Code != http.StatusOK {
					t.Errorf("worker %d req %d on %s failed with code %d", workerID, r, ep, rr.Code)
					return
				}
			}
		}(w)
	}

	wg.Wait()
}

// 11. Home Assistant controls arrive over MQTT and run through the same
// handlers as the viewer's buttons.
func TestIntegrations_MQTTCommandsReachTheCamera(t *testing.T) {
	resetIntegrations(t)
	_, streamMgr, _, mockCC, _, _ := setupFullTestEnvironment(t)
	devs := []bridge.Device{{UUID: "cam-uuid-1", Name: "Front Porch (2nd)", Type: "WS03"}}
	if _, err := streamMgr.Enroll([]string{"cam-uuid-1"}, devs); err != nil {
		t.Fatal(err)
	}
	b := &haBridgeState{sm: streamMgr}
	mockCC.irMode, mockCC.ledOn = bridge.IRModeAuto, true

	b.handleCommand("bombecam/cam-uuid-1/night_vision/set", "Off")
	if mockCC.irMode != bridge.IRModeOff {
		t.Errorf("night vision Off: irMode %d", mockCC.irMode)
	}
	b.handleCommand("bombecam/cam-uuid-1/night_vision/set", "Auto")
	if mockCC.irMode != bridge.IRModeAuto {
		t.Errorf("night vision Auto: irMode %d", mockCC.irMode)
	}
	b.handleCommand("bombecam/cam-uuid-1/status_light/set", "OFF")
	if mockCC.ledOn {
		t.Error("status light OFF did not reach the camera")
	}
	b.handleCommand("bombecam/cam-uuid-1/status_light/set", "ON")
	if !mockCC.ledOn {
		t.Error("status light ON did not reach the camera")
	}
	before := len(mockCC.ptzCalls())
	for _, d := range []string{"left", "right", "up", "down"} {
		b.handleCommand("bombecam/cam-uuid-1/ptz/set", d)
	}
	calls := mockCC.ptzCalls()[before:]
	if len(calls) != 4 || calls[0] != [2]int{1, 400} || calls[1] != [2]int{2, 400} || calls[2] != [2]int{3, 400} || calls[3] != [2]int{4, 400} {
		t.Errorf("PTZ pulses %v, want left/right/up/down for 400 ms", calls)
	}
	// junk is ignored
	b.handleCommand("bombecam/cam-uuid-1/night_vision/set", "Disco")
	b.handleCommand("bombecam/unknown/status_light/set", "ON")
	b.handleCommand("other/cam-uuid-1/status_light/set", "ON")
	if n := len(mockCC.ptzCalls()); n != before+4 {
		t.Errorf("unexpected PTZ calls: %d", n)
	}
}

func TestIntegrations_DecoupledEndpoints_FrigateAndHA(t *testing.T) {
	streamMgr, mux, _ := setupIntegrationsTestEnv(t)
	// Internal publish is set to docker container name
	streamMgr.SetPublishRTSPBase("rtsp://mediamtx:8554")

	// 1. Frigate request from LAN client 192.168.1.50:8080
	reqF := httptest.NewRequest(http.MethodGet, "http://192.168.1.50:8080/api/v1/integrations/frigate", nil)
	reqF.Host = "192.168.1.50:8080"
	rrF := httptest.NewRecorder()
	mux.ServeHTTP(rrF, reqF)

	if rrF.Code != http.StatusOK {
		t.Fatalf("expected 200 OK from Frigate, got %d", rrF.Code)
	}
	bodyF := rrF.Body.String()
	if strings.Contains(bodyF, "mediamtx") {
		t.Fatalf("LEAK: internal Docker hostname 'mediamtx' leaked in Frigate response: %s", bodyF)
	}
	if !strings.Contains(bodyF, "rtsp://192.168.1.50:8554/cam-uuid-1") {
		t.Fatalf("expected advertised LAN RTSP URL in Frigate response, got: %s", bodyF)
	}

	// 2. Home Assistant request from LAN client 192.168.1.50:8080
	reqHA := httptest.NewRequest(http.MethodGet, "http://192.168.1.50:8080/api/v1/integrations/homeassistant", nil)
	reqHA.Host = "192.168.1.50:8080"
	rrHA := httptest.NewRecorder()
	mux.ServeHTTP(rrHA, reqHA)

	if rrHA.Code != http.StatusOK {
		t.Fatalf("expected 200 OK from HA, got %d", rrHA.Code)
	}
	bodyHA := rrHA.Body.String()
	if strings.Contains(bodyHA, "mediamtx") {
		t.Fatalf("LEAK: internal Docker hostname 'mediamtx' leaked in Home Assistant response: %s", bodyHA)
	}
	if !strings.Contains(bodyHA, "rtsp://192.168.1.50:8554/cam-uuid-1") {
		t.Fatalf("expected advertised LAN RTSP URL in HA response, got: %s", bodyHA)
	}

	// 3. Camera Stream Descriptor (/api/v1/cameras/{id}/stream)
	reqStream := httptest.NewRequest(http.MethodGet, "http://192.168.1.50:8080/api/v1/cameras/cam-uuid-1/stream", nil)
	reqStream.Host = "192.168.1.50:8080"
	rrStream := httptest.NewRecorder()
	mux.ServeHTTP(rrStream, reqStream)

	if rrStream.Code != http.StatusOK {
		t.Fatalf("expected 200 OK from /stream, got %d", rrStream.Code)
	}
	var streamResp map[string]any
	_ = json.Unmarshal(rrStream.Body.Bytes(), &streamResp)
	if streamResp["rtsp_url"] != "rtsp://192.168.1.50:8554/cam-uuid-1" {
		t.Errorf("expected rtsp_url 'rtsp://192.168.1.50:8554/cam-uuid-1', got %v", streamResp["rtsp_url"])
	}
	// other devices get the LAN URL; the viewer gets the gateway's relayed copy
	if streamResp["hls_url_lan"] != "http://192.168.1.50:8888/cam-uuid-1/index.m3u8" {
		t.Errorf("expected hls_url_lan 'http://192.168.1.50:8888/cam-uuid-1/index.m3u8', got %v", streamResp["hls_url_lan"])
	}
	if streamResp["hls_url"] != "/api/v1/cameras/cam-uuid-1/hls/index.m3u8" {
		t.Errorf("expected the relayed hls_url, got %v", streamResp["hls_url"])
	}
	if streamResp["rtsp_url_lan"] != streamResp["rtsp_url"] || streamResp["rtsp_url_by_id"] != "rtsp://192.168.1.50:8554/cam-uuid-1" {
		t.Errorf("rtsp_url_lan %v rtsp_url_by_id %v", streamResp["rtsp_url_lan"], streamResp["rtsp_url_by_id"])
	}

	// 4. Camera Status (/api/v1/cameras/{id}/status)
	reqStatus := httptest.NewRequest(http.MethodGet, "http://192.168.1.50:8080/api/v1/cameras/cam-uuid-1/status", nil)
	reqStatus.Host = "192.168.1.50:8080"
	rrStatus := httptest.NewRecorder()
	mux.ServeHTTP(rrStatus, reqStatus)

	if rrStatus.Code != http.StatusOK {
		t.Fatalf("expected 200 OK from /status, got %d", rrStatus.Code)
	}
	var statusResp CameraStatusResponse
	_ = json.Unmarshal(rrStatus.Body.Bytes(), &statusResp)
	if statusResp.StreamURL != "rtsp://192.168.1.50:8554/cam-uuid-1" {
		t.Errorf("expected status StreamURL 'rtsp://192.168.1.50:8554/cam-uuid-1', got %v", statusResp.StreamURL)
	}
	if statusResp.HlsURL != "http://192.168.1.50:8888/cam-uuid-1/index.m3u8" {
		t.Errorf("expected status HlsURL 'http://192.168.1.50:8888/cam-uuid-1/index.m3u8', got %v", statusResp.HlsURL)
	}
}
