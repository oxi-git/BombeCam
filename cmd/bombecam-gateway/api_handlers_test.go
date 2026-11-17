package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"sync"
	"testing"

	"github.com/Fever-r/BombeCam/pkg/bridge"
)

type mockControlChannel struct {
	irMode  int
	ledOn   bool
	lightOn bool
	ptzDir  int
	ptzDur  int
	timeout bool
	ptzMu   sync.Mutex
	ptzLog  [][2]int       // every MovePTZ: direction, duration
	attrs   map[string]any // every SetAttribute: key -> last value
}

func (m *mockControlChannel) MovePTZ(ctx context.Context, cameraUUID string, direction int, durationMs int) error {
	if m.timeout {
		return fmt.Errorf("readback confirmation timeout")
	}
	m.ptzDir = direction
	m.ptzDur = durationMs
	m.ptzMu.Lock()
	m.ptzLog = append(m.ptzLog, [2]int{direction, durationMs})
	m.ptzMu.Unlock()
	return nil
}

func (m *mockControlChannel) ptzCalls() [][2]int {
	m.ptzMu.Lock()
	defer m.ptzMu.Unlock()
	return append([][2]int(nil), m.ptzLog...)
}

func (m *mockControlChannel) SetIR(ctx context.Context, cameraUUID string, mode int) error {
	if m.timeout {
		return fmt.Errorf("readback confirmation timeout")
	}
	m.irMode = mode
	return nil
}

func (m *mockControlChannel) GetIR(ctx context.Context, cameraUUID string) (int, error) {
	return m.irMode, nil
}

func (m *mockControlChannel) SetLED(ctx context.Context, cameraUUID string, on bool) error {
	if m.timeout {
		return fmt.Errorf("readback confirmation timeout")
	}
	m.ledOn = on
	return nil
}

func (m *mockControlChannel) GetLED(ctx context.Context, cameraUUID string) (bool, error) {
	return m.ledOn, nil
}

func (m *mockControlChannel) SetLight(ctx context.Context, cameraUUID string, on bool) error {
	if m.timeout {
		return fmt.Errorf("readback confirmation timeout")
	}
	m.lightOn = on
	return nil
}

func (m *mockControlChannel) ArmTalk(ctx context.Context, cameraUUID string, enable bool, sessionID string) error {
	return nil
}

func (m *mockControlChannel) SetAttribute(ctx context.Context, cameraUUID, key string, value any) error {
	if m.timeout {
		return fmt.Errorf("readback confirmation timeout")
	}
	m.ptzMu.Lock()
	defer m.ptzMu.Unlock()
	if m.attrs == nil {
		m.attrs = map[string]any{}
	}
	m.attrs[key] = value
	return nil
}

func (m *mockControlChannel) attr(key string) (any, bool) {
	m.ptzMu.Lock()
	defer m.ptzMu.Unlock()
	v, ok := m.attrs[key]
	return v, ok
}

func (m *mockControlChannel) Close() error {
	return nil
}

// buildValidADTSFrame generates a valid 11-byte 16kHz mono AAC-LC ADTS frame for testing.
func buildValidADTSFrame() []byte {
	hdr := make([]byte, 11)
	hdr[0] = 0xFF
	hdr[1] = 0xF1 // syncword 0xFFF, ID 0, layer 0, protection absent 1
	hdr[2] = 0x60 // profile 1 (AAC-LC) << 6 | sr index 8 (16000Hz) << 2
	hdr[3] = 0x40 // channel 1 (mono) << 6 | length MSB 0
	hdr[4] = 0x01 // length (11 bytes = 0b0000000001011) -> bits [10:3] = 1
	hdr[5] = 0x7F // length bits [2:0] = 3 << 5 = 0x60 | buffer fullness MSB 0x1F = 0x7F
	hdr[6] = 0xFC // buffer fullness LSB 0x3F << 2 | num frames 0 = 0xFC
	// Bytes 7..10: dummy AAC payload
	hdr[7] = 0x21
	hdr[8] = 0x22
	hdr[9] = 0x23
	hdr[10] = 0x24
	return hdr
}

func setupTestEnvironment() (*SessionManager, *StreamManager, *mockControlChannel, *http.ServeMux) {
	sm := NewSessionManager("39", "test-phone", testServerKey)
	streamMgr := NewStreamManager(nil, "rtsp://127.0.0.1:8554", "http://127.0.0.1:8888", []string{"10.0.0.50"}, false, "ffmpeg")
	streamMgr.SetRunner(func(ctx context.Context, mc *ManagedCamera, rtpPort int) {
		<-ctx.Done()
	})

	mockCC := &mockControlChannel{irMode: 1, ledOn: true}
	SetGatewayControlChannel(mockCC)
	// Same wiring as main: the manager the gateway reads from, and its pool.
	setCurrentStreamManager(streamMgr)
	ensureTestRegistry(streamMgr, sm, sm.Country(), sm.PhoneCode(), testServerKey)

	mux := SetupAPIMux(sm, streamMgr)
	return sm, streamMgr, mockCC, mux
}

func TestAPI_SessionStatus(t *testing.T) {
	sm, _, _, mux := setupTestEnvironment()

	// Initial status
	sm.SetStatus(SessionStatusCredentialsMissing, "missing credentials: use --email/--password or env OSAIO_EMAIL/OSAIO_PASSWORD")

	req := httptest.NewRequest(http.MethodGet, "/api/v1/session/status", nil)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d", rr.Code)
	}

	var resp SessionStatusInfo
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("json decode error: %v", err)
	}
	if resp.Status != SessionStatusCredentialsMissing {
		t.Fatalf("expected status %s, got %s", SessionStatusCredentialsMissing, resp.Status)
	}
	if !strings.Contains(resp.Error, "missing credentials") {
		t.Fatalf("expected error message in response, got %s", resp.Error)
	}

	// Authenticated status
	sm.SetStateForTest(SessionStatusAuthenticated, "user@example.com", "uid-9876", "", nil)

	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d", rr.Code)
	}
	var authResp SessionStatusInfo
	if err := json.Unmarshal(rr.Body.Bytes(), &authResp); err != nil {
		t.Fatalf("json decode error: %v", err)
	}
	// The status sums up the login pool; it names no login.
	if authResp.Status != SessionStatusAuthenticated || authResp.AccountEmail != "" || authResp.VendorUID != "" {
		t.Fatalf("unexpected auth response: %+v", authResp)
	}
	if authResp.LastLoginTS == "" {
		t.Fatal("expected last_login_ts to be set")
	}
}

func TestAPI_InventoryDiscover(t *testing.T) {
	sm, _, _, mux := setupTestEnvironment()

	// 1. Unauthenticated -> 401
	req := httptest.NewRequest(http.MethodGet, "/api/v1/inventory/discover", nil)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 Unauthorized, got %d", rr.Code)
	}

	// 2. Authenticated -> 200 with camera list
	mockDevs := []bridge.Device{
		{UUID: "uuid-front", Name: "Front Porch", Type: "WS03", Online: 1},
		{UUID: "uuid-back", Name: "Backyard", Type: "WS03", Online: 0},
	}
	sm.SetStateForTest(SessionStatusAuthenticated, "user@example.com", "uid-9876", "", mockDevs)

	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d (%s)", rr.Code, rr.Body.String())
	}

	var discResp struct {
		Cameras []struct {
			ID     string `json:"id"`
			Name   string `json:"name"`
			Model  string `json:"model"`
			Online bool   `json:"online"`
			IP     string `json:"ip"`
		} `json:"cameras"`
		FreshnessTS string `json:"freshness_ts"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &discResp); err != nil {
		t.Fatalf("decode discover response error: %v", err)
	}
	if len(discResp.Cameras) != 2 {
		t.Fatalf("expected 2 cameras, got %d", len(discResp.Cameras))
	}
	if discResp.Cameras[0].ID != "uuid-front" || !discResp.Cameras[0].Online {
		t.Fatalf("unexpected camera 0: %+v", discResp.Cameras[0])
	}
	if discResp.Cameras[1].ID != "uuid-back" || discResp.Cameras[1].Online {
		t.Fatalf("unexpected camera 1: %+v", discResp.Cameras[1])
	}
	if discResp.FreshnessTS == "" {
		t.Fatal("expected freshness_ts to be set")
	}
}

func TestAPI_InventoryEnroll(t *testing.T) {
	sm, streamMgr, _, mux := setupTestEnvironment()

	mockDevs := []bridge.Device{
		{UUID: "uuid-front", Name: "Front Porch", Type: "WS03", Online: 1},
		{UUID: "uuid-back", Name: "Backyard", Type: "WS03", Online: 1},
	}
	sm.SetStateForTest(SessionStatusAuthenticated, "user@example.com", "uid-9876", "", mockDevs)

	// 0. Empty camera IDs returns 400
	payloadEmpty := `{"camera_ids": []}`
	reqEmpty := httptest.NewRequest(http.MethodPost, "/api/v1/inventory/enroll", strings.NewReader(payloadEmpty))
	rrEmpty := httptest.NewRecorder()
	mux.ServeHTTP(rrEmpty, reqEmpty)

	if rrEmpty.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 Bad Request for empty camera_ids, got %d (%s)", rrEmpty.Code, rrEmpty.Body.String())
	}
	var emptyErrResp map[string]any
	if err := json.Unmarshal(rrEmpty.Body.Bytes(), &emptyErrResp); err != nil {
		t.Fatalf("decode empty enroll error json: %v", err)
	}
	errStr, _ := emptyErrResp["error"].(string)
	if !strings.Contains(errStr, "no camera IDs provided") {
		t.Fatalf("expected error containing 'no camera IDs provided', got '%v'", emptyErrResp["error"])
	}

	// 1. Invalid camera ID returns 400
	payload := `{"camera_ids": ["uuid-front", "uuid-nonexistent"]}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/inventory/enroll", strings.NewReader(payload))
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 Bad Request, got %d (%s)", rr.Code, rr.Body.String())
	}

	// 2. Valid enrollment returns 200
	payload = `{"camera_ids": ["uuid-front"]}`
	req = httptest.NewRequest(http.MethodPost, "/api/v1/inventory/enroll", strings.NewReader(payload))
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d (%s)", rr.Code, rr.Body.String())
	}

	var enrollResp struct {
		Enrolled      []string `json:"enrolled"`
		ActiveStreams int      `json:"active_streams"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &enrollResp); err != nil {
		t.Fatalf("decode enroll response: %v", err)
	}
	if len(enrollResp.Enrolled) != 1 || enrollResp.Enrolled[0] != "uuid-front" {
		t.Fatalf("unexpected enrolled response: %+v", enrollResp)
	}

	if _, exists := streamMgr.GetCamera("uuid-front"); !exists {
		t.Fatal("expected uuid-front to be enrolled in StreamManager")
	}
}

func TestAPI_CameraStatusAndLockout(t *testing.T) {
	withLAN(t, LANAddress{IP: "192.168.1.20", Interface: "Wi-Fi", Default: true})
	sm, streamMgr, _, mux := setupTestEnvironment()

	mockDevs := []bridge.Device{
		{UUID: "uuid-front", Name: "Front Porch", Type: "WS03", Online: 1},
	}
	sm.SetStateForTest(SessionStatusAuthenticated, "user@example.com", "uid-9876", "", mockDevs)
	_, _ = streamMgr.Enroll([]string{"uuid-front"}, mockDevs)

	// 1. Unknown camera returns 404
	req := httptest.NewRequest(http.MethodGet, "/api/v1/cameras/unknown-id/status", nil)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for unknown camera, got %d", rr.Code)
	}

	// 2. Enrolled camera returns 200 with schema
	req = httptest.NewRequest(http.MethodGet, "/api/v1/cameras/uuid-front/status", nil)
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d", rr.Code)
	}

	var statusResp CameraStatusResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &statusResp); err != nil {
		t.Fatalf("decode status response error: %v", err)
	}
	if statusResp.ID != "uuid-front" || statusResp.Name != "Front Porch" {
		t.Fatalf("unexpected status response: %+v", statusResp)
	}
	if statusResp.RecoveryState != "ACTIVE" || statusResp.RecoveryAttempts != 3 {
		t.Fatalf("unexpected initial recovery state: %+v", statusResp)
	}
	// the address other devices can use, not 127.0.0.1
	if !strings.HasPrefix(statusResp.StreamURL, "rtsp://192.168.1.20:8554/uuid-front") {
		t.Fatalf("unexpected stream_url: %s", statusResp.StreamURL)
	}
	if !strings.HasPrefix(statusResp.HlsURL, "http://192.168.1.20:8888/uuid-front/index.m3u8") {
		t.Fatalf("unexpected hls_url: %s", statusResp.HlsURL)
	}

	// 3. Put camera in LOCKOUT and confirm it REMAINS VISIBLE in GET /api/v1/cameras/{id}/status and legacy /status
	mc, _ := streamMgr.GetCamera("uuid-front")
	mc.Session.RecordFailure()
	mc.Session.RecordFailure()
	mc.Session.RecordFailure()

	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 OK for locked out camera, got %d", rr.Code)
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &statusResp)
	if statusResp.RecoveryState != "LOCKOUT" || statusResp.RecoveryAttempts != 0 {
		t.Fatalf("expected LOCKOUT with 0 attempts, got %s / %d", statusResp.RecoveryState, statusResp.RecoveryAttempts)
	}

	// Check legacy /status endpoint also retains lockout camera
	legReq := httptest.NewRequest(http.MethodGet, "/status", nil)
	legRR := httptest.NewRecorder()
	mux.ServeHTTP(legRR, legReq)
	if legRR.Code != http.StatusOK {
		t.Fatalf("legacy /status failed: %d", legRR.Code)
	}
	if !strings.Contains(legRR.Body.String(), "LOCKOUT") {
		t.Fatalf("legacy /status omitted locked-out camera: %s", legRR.Body.String())
	}
}

func TestAPI_CameraControlDispatch(t *testing.T) {
	sm, streamMgr, mockCC, mux := setupTestEnvironment()

	mockDevs := []bridge.Device{
		{UUID: "uuid-front", Name: "Front Porch", Type: "WS03", Online: 1},
	}
	sm.SetStateForTest(SessionStatusAuthenticated, "user@example.com", "uid-9876", "", mockDevs)
	_, _ = streamMgr.Enroll([]string{"uuid-front"}, mockDevs)

	// 1. Unknown camera returns 404
	req := httptest.NewRequest(http.MethodPost, "/api/v1/cameras/unknown-id/control", strings.NewReader(`{"action":"ir"}`))
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for unknown camera control, got %d", rr.Code)
	}

	// 2. Unsupported action returns 400
	req = httptest.NewRequest(http.MethodPost, "/api/v1/cameras/uuid-front/control", strings.NewReader(`{"action":"dance"}`))
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for unsupported action, got %d", rr.Code)
	}

	// 3. Valid IR control dispatch
	req = httptest.NewRequest(http.MethodPost, "/api/v1/cameras/uuid-front/control", strings.NewReader(`{"action":"ir","params":{"mode":"off"}}`))
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 OK for IR control, got %d (%s)", rr.Code, rr.Body.String())
	}
	if mockCC.irMode != bridge.IRModeOff {
		t.Fatalf("expected mockCC.irMode == %d (off), got %d", bridge.IRModeOff, mockCC.irMode)
	}

	// 4. Valid LED control dispatch
	req = httptest.NewRequest(http.MethodPost, "/api/v1/cameras/uuid-front/control", strings.NewReader(`{"action":"led","params":{"mode":"on"}}`))
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 OK for LED control, got %d", rr.Code)
	}
	if !mockCC.ledOn {
		t.Fatal("expected mockCC.ledOn == true")
	}

	// 5. Valid PTZ control dispatch
	req = httptest.NewRequest(http.MethodPost, "/api/v1/cameras/uuid-front/control", strings.NewReader(`{"action":"ptz","params":{"direction":1,"duration_ms":300}}`))
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 OK for PTZ control, got %d", rr.Code)
	}
	if mockCC.ptzDir != 1 || mockCC.ptzDur != 300 {
		t.Fatalf("expected PTZ dir=1 dur=300, got dir=%d dur=%d", mockCC.ptzDir, mockCC.ptzDur)
	}

	// 6. Timeout error returns 503
	mockCC.timeout = true
	req = httptest.NewRequest(http.MethodPost, "/api/v1/cameras/uuid-front/control", strings.NewReader(`{"action":"ir","params":{"mode":"on"}}`))
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 Service Unavailable on timeout, got %d (%s)", rr.Code, rr.Body.String())
	}
	mockCC.timeout = false
}

// Detection switches and the legacy /mic route go through the control
// channel like every other control; handlers never write vendor messages.
func TestAPI_SettingSwitchesUseControlChannel(t *testing.T) {
	sm, streamMgr, mockCC, mux := setupTestEnvironment()
	defer SetGatewayControlChannel(nil)
	mockDevs := []bridge.Device{{UUID: "uuid-front", Name: "Front Porch", Type: "WS03", Online: 1}}
	sm.SetStateForTest(SessionStatusAuthenticated, "user@example.com", "uid-9876", "", mockDevs)
	_, _ = streamMgr.Enroll([]string{"uuid-front"}, mockDevs)

	for _, c := range []struct{ body, key string }{
		{`{"action":"motion","value":"on"}`, "MotionDetectSW"},
		{`{"action":"sound","value":"off"}`, "SoundDetectSW"},
	} {
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/v1/cameras/uuid-front/control", strings.NewReader(c.body)))
		if rr.Code != http.StatusOK {
			t.Fatalf("%s: got %d (%s)", c.body, rr.Code, rr.Body.String())
		}
		if _, ok := mockCC.attr(c.key); !ok {
			t.Fatalf("%s did not reach the control channel", c.key)
		}
	}

	registerSig(bridge.Device{UUID: "uuid-front", Name: "Front Porch", Type: "WS03"}, nil, nil)
	defer unregisterSig("uuid-front")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/mic?cam=uuid-front&enable=0", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("/mic: got %d (%s)", rr.Code, rr.Body.String())
	}
	if v, ok := mockCC.attr("AudioRecordSw"); !ok || v != 0 {
		t.Fatalf("/mic sent AudioRecordSw=%v (%v); want 0 through the control channel", v, ok)
	}
}

func TestAPI_CameraUnlockAndMode(t *testing.T) {
	sm, streamMgr, _, mux := setupTestEnvironment()

	mockDevs := []bridge.Device{
		{UUID: "uuid-front", Name: "Front Porch", Type: "WS03", Online: 1},
	}
	sm.SetStateForTest(SessionStatusAuthenticated, "user@example.com", "uid-9876", "", mockDevs)
	_, _ = streamMgr.Enroll([]string{"uuid-front"}, mockDevs)

	mc, _ := streamMgr.GetCamera("uuid-front")
	mc.Session.RecordFailure()
	mc.Session.RecordFailure()
	mc.Session.RecordFailure()

	// 1. POST /api/v1/cameras/{id}/unlock
	req := httptest.NewRequest(http.MethodPost, "/api/v1/cameras/uuid-front/unlock", nil)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 OK for unlock, got %d", rr.Code)
	}

	var unlockResp struct {
		Status          string `json:"status"`
		RemainingBudget int    `json:"remaining_budget"`
		CeilingSeconds  int    `json:"ceiling_seconds"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &unlockResp); err != nil {
		t.Fatalf("decode unlock response: %v", err)
	}
	if unlockResp.Status != "unlocked" || unlockResp.RemainingBudget != 3 || unlockResp.CeilingSeconds != 30 {
		t.Fatalf("unexpected unlock response: %+v", unlockResp)
	}

	// 2. POST /api/v1/cameras/{id}/mode
	modeReq := httptest.NewRequest(http.MethodPost, "/api/v1/cameras/uuid-front/mode", strings.NewReader(`{"strict_manual": true}`))
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, modeReq)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 OK for mode, got %d", rr.Code)
	}
	if !mc.Session.IsStrictManual() {
		t.Fatal("expected strict manual to be true")
	}
}

func TestAPI_TalkbackEndpoints(t *testing.T) {
	sm, streamMgr, _, mux := setupTestEnvironment()

	mockDevs := []bridge.Device{
		{UUID: "uuid-front", Name: "Front Porch", Type: "WS03", Online: 1},
	}
	sm.SetStateForTest(SessionStatusAuthenticated, "user@example.com", "uid-9876", "", mockDevs)
	_, _ = streamMgr.Enroll([]string{"uuid-front"}, mockDevs)

	// 1. Talk start / stop
	req := httptest.NewRequest(http.MethodPost, "/api/v1/cameras/uuid-front/talk/start", nil)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 OK for talk/start, got %d", rr.Code)
	}

	req = httptest.NewRequest(http.MethodPost, "/api/v1/cameras/uuid-front/talk/stop", nil)
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 OK for talk/stop, got %d", rr.Code)
	}

	// 2. Talk stream with invalid ADTS audio returns 400
	mc, _ := streamMgr.GetCamera("uuid-front")
	mc.mu.Lock()
	mc.Streaming = true
	// Mock viewer cannot be started without real WebRTC, but test inactive stream first:
	mc.Viewer = nil
	mc.mu.Unlock()

	req = httptest.NewRequest(http.MethodPost, "/api/v1/cameras/uuid-front/talk/stream", bytes.NewReader([]byte("not-audio-data")))
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 for nil viewer, got %d", rr.Code)
	}

	// Verify valid ADTS frame generation matches bridge.ParseADTSHeader requirement
	validFrame := buildValidADTSFrame()
	parsed, err := bridge.ParseADTSHeader(validFrame)
	if err != nil {
		t.Fatalf("validFrame failed ParseADTSHeader: %v", err)
	}
	if parsed.SampleRate != 16000 || parsed.ChannelConfig != 1 {
		t.Fatalf("expected 16kHz mono, got %d Hz / %d ch", parsed.SampleRate, parsed.ChannelConfig)
	}

	frames, err := bridge.ReadAllADTSFrames(bytes.NewReader(validFrame))
	if err != nil {
		t.Fatalf("ReadAllADTSFrames failed on validFrame: %v", err)
	}
	if len(frames) != 1 {
		t.Fatalf("expected 1 frame, got %d", len(frames))
	}
}

func TestAPI_WHEPEndpoint(t *testing.T) {
	sm, streamMgr, _, mux := setupTestEnvironment()

	mockDevs := []bridge.Device{
		{UUID: "uuid-front", Name: "Front Porch", Type: "WS03", Online: 1},
	}
	sm.SetStateForTest(SessionStatusAuthenticated, "user@example.com", "uid-9876", "", mockDevs)
	_, _ = streamMgr.Enroll([]string{"uuid-front"}, mockDevs)

	// 1. Non-existent camera returns 404
	req := httptest.NewRequest(http.MethodPost, "/api/v1/cameras/uuid-unknown/whep", bytes.NewReader([]byte("v=0...")))
	req.Header.Set("Content-Type", "application/sdp")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for unknown camera, got %d", rr.Code)
	}

	// 2. OPTIONS preflight returns 204 No Content with WHEP headers
	reqOpt := httptest.NewRequest(http.MethodOptions, "/api/v1/cameras/uuid-front/whep", nil)
	rrOpt := httptest.NewRecorder()
	mux.ServeHTTP(rrOpt, reqOpt)
	if rrOpt.Code != http.StatusNoContent {
		t.Fatalf("expected 204 No Content for OPTIONS, got %d", rrOpt.Code)
	}
	if rrOpt.Header().Get("Accept-Post") != "application/sdp" {
		t.Fatalf("expected Accept-Post: application/sdp, got %s", rrOpt.Header().Get("Accept-Post"))
	}

	// 3. Mock upstream MediaMTX WHEP server
	mockMediaMTX := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/uuid-front/whep" {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodPost {
			http.Error(w, "POST expected", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/sdp")
		w.Header().Set("Location", "/uuid-front/whep/session-123")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte("v=0\r\no=- 0 0 IN IP4 127.0.0.1\r\ns=-\r\n"))
	}))
	defer mockMediaMTX.Close()

	streamMgr.SetWebRTCBase(mockMediaMTX.URL)

	reqPost := httptest.NewRequest(http.MethodPost, "/api/v1/cameras/uuid-front/whep", bytes.NewReader([]byte("v=0 offer...")))
	reqPost.Header.Set("Content-Type", "application/sdp")
	rrPost := httptest.NewRecorder()
	mux.ServeHTTP(rrPost, reqPost)

	if rrPost.Code != http.StatusCreated {
		t.Fatalf("expected 201 Created from WHEP upstream, got %d", rrPost.Code)
	}
	if rrPost.Header().Get("Content-Type") != "application/sdp" {
		t.Fatalf("expected Content-Type: application/sdp, got %s", rrPost.Header().Get("Content-Type"))
	}
	if !strings.Contains(rrPost.Body.String(), "IN IP4 127.0.0.1") {
		t.Fatalf("expected SDP answer in body, got: %s", rrPost.Body.String())
	}
}

func TestAPI_PCMTranscodeToADTS(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not installed")
	}
	// 32000 bytes = 16000 samples = 1 second of 16kHz s16le mono PCM
	pcm := make([]byte, 32000)
	frames, err := transcodePCMToADTS(ffmpeg, pcm)
	if err != nil {
		t.Fatalf("transcodePCMToADTS failed: %v", err)
	}
	if len(frames) == 0 {
		t.Fatalf("expected at least 1 ADTS frame from 1 second of PCM, got 0")
	}
	for i, f := range frames {
		hdr, err := bridge.ParseADTSHeader(f)
		if err != nil {
			t.Fatalf("frame %d failed ParseADTSHeader: %v", i, err)
		}
		if hdr.SampleRate != 16000 || hdr.ChannelConfig != 1 {
			t.Fatalf("frame %d unexpected params: rate=%d, ch=%d", i, hdr.SampleRate, hdr.ChannelConfig)
		}
	}
}
