package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Fever-r/BombeCam/pkg/bridge"
)

// The SD card is read on the control channel. A channel that cannot query it
// must say so plainly instead of calling something that does not exist.
func TestSDCardEndpointsSayUnavailable(t *testing.T) {
	sm, streamMgr, _, _ := setupTestEnvironment()
	sm.SetStateForTest(SessionStatusAuthenticated, "user@example.com", "uid-1", "",
		[]bridge.Device{{UUID: "cam-sd", Name: "Test", Type: "WS03", Online: 1}})
	if _, err := streamMgr.Enroll([]string{"cam-sd"}, []bridge.Device{{UUID: "cam-sd", Name: "Test", Type: "WS03"}}); err != nil {
		t.Fatalf("enroll: %v", err)
	}

	handlers := map[string]func(http.ResponseWriter, *http.Request, string, *StreamManager){
		"/api/v1/cameras/cam-sd/sdcard":            handleSDCardInfo,
		"/api/v1/cameras/cam-sd/sdcard/recordings": handleSDCardRecordings,
	}
	for path, handle := range handlers {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.RemoteAddr = "127.0.0.1:50000"
		handle(rec, req, "cam-sd", streamMgr)
		if rec.Code != http.StatusNotImplemented {
			t.Fatalf("%s: got %d, want 501", path, rec.Code)
		}
		var body map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		msg, _ := body["message"].(string)
		if body["error"] != "sdcard_unavailable" || !strings.Contains(msg, "control channel") {
			t.Fatalf("%s: %v", path, body)
		}
	}
}

// sdcardFakeChannel answers the SD card queries; the other ControlChannel
// methods are never reached by these handlers.
type sdcardFakeChannel struct {
	info  map[string]any
	days  map[string]any
	err   error
	calls []string
}

func (f *sdcardFakeChannel) MovePTZ(context.Context, string, int, int) error { return nil }
func (f *sdcardFakeChannel) SetIR(context.Context, string, int) error        { return nil }
func (f *sdcardFakeChannel) GetIR(context.Context, string) (int, error)      { return 0, nil }
func (f *sdcardFakeChannel) SetLED(context.Context, string, bool) error      { return nil }
func (f *sdcardFakeChannel) GetLED(context.Context, string) (bool, error)    { return false, nil }
func (f *sdcardFakeChannel) SetLight(context.Context, string, bool) error    { return nil }
func (f *sdcardFakeChannel) ArmTalk(context.Context, string, bool, string) error {
	return nil
}
func (f *sdcardFakeChannel) SetAttribute(context.Context, string, string, any) error {
	return nil
}
func (f *sdcardFakeChannel) Close() error { return nil }

func (f *sdcardFakeChannel) GetSDCardInfo(_ context.Context, cameraUUID string) (map[string]any, error) {
	f.calls = append(f.calls, "info:"+cameraUUID)
	return f.info, f.err
}

func (f *sdcardFakeChannel) GetRecordDays(_ context.Context, cameraUUID string) (map[string]any, error) {
	f.calls = append(f.calls, "days:"+cameraUUID)
	return f.days, f.err
}

func TestSDCardEndpointsMapReply(t *testing.T) {
	sm, streamMgr, _, _ := setupTestEnvironment()
	sm.SetStateForTest(SessionStatusAuthenticated, "user@example.com", "uid-1", "",
		[]bridge.Device{{UUID: "cam-sd", Name: "Test", Type: "WS03", Online: 1}})
	if _, err := streamMgr.Enroll([]string{"cam-sd"}, []bridge.Device{{UUID: "cam-sd", Name: "Test", Type: "WS03"}}); err != nil {
		t.Fatalf("enroll: %v", err)
	}

	// The SD card is read on the camera's own YRCX socket, so the test answers
	// atr.get there. The reply is nested one level down and carries no used size.
	fake := &fakeVendorWS{reported: map[string]any{
		"data": map[string]any{"Status": "1", "TotalSizeK": float64(31268864),
			"curMonth": "2026-11", "curMonDays": "1,2,3", "preMonth": "2026-10", "preMonDays": "31"},
	}}
	srv := httptest.NewServer(http.HandlerFunc(fake.handler))
	defer srv.Close()
	sig, err := bridge.Connect("ws"+strings.TrimPrefix(srv.URL, "http"), "tok", "uid-1", "39")
	if err != nil {
		t.Fatal(err)
	}
	defer sig.Close()
	sig.BindCamera("cam-sd", "WS03")
	streamMgr.SetStreamActive("cam-sd", true, nil, sig, "test")
	defer streamMgr.SetStreamActive("cam-sd", false, nil, nil, "")

	// A local MQTT control channel is what a firewall-backend deploy configures;
	// it cannot answer an atr.get, and the handlers must still reach the socket.
	SetGatewayControlChannel(&sdcardFakeChannel{})
	defer SetGatewayControlChannel(nil)

	rec := httptest.NewRecorder()
	handleSDCardInfo(rec, httptest.NewRequest(http.MethodGet, "/api/v1/cameras/cam-sd/sdcard", nil), "cam-sd", streamMgr)
	if rec.Code != http.StatusOK {
		t.Fatalf("sdcard: got %d body=%s", rec.Code, rec.Body.String())
	}
	var info map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &info); err != nil {
		t.Fatal(err)
	}
	if info["status"] != "1" || info["total_kb"] != float64(31268864) {
		t.Fatalf("sdcard: unexpected %v", info)
	}
	if _, ok := info["used_kb"]; ok {
		t.Fatalf("sdcard: absent used size must be omitted, got %v", info)
	}
	if info["raw"] == nil {
		t.Fatalf("sdcard: raw reply missing: %v", info)
	}

	rec = httptest.NewRecorder()
	handleSDCardRecordings(rec, httptest.NewRequest(http.MethodGet, "/api/v1/cameras/cam-sd/sdcard/recordings", nil), "cam-sd", streamMgr)
	if rec.Code != http.StatusOK {
		t.Fatalf("recordings: got %d body=%s", rec.Code, rec.Body.String())
	}
	var days map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &days); err != nil {
		t.Fatal(err)
	}
	if days["cur_month"] != "2026-11" || days["cur_days"] != "1,2,3" ||
		days["prev_month"] != "2026-10" || days["prev_days"] != "31" {
		t.Fatalf("recordings: unexpected %v", days)
	}
	if got, ok := fake.find("atr.get", "Status"); ok && got != nil {
		t.Fatalf("atr.get carried the reply body as its payload: %v", got)
	}
}
