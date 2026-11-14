package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Fever-r/BombeCam/pkg/bridge"
)

// The camera's SD card is read over P2P, so the cloud endpoints must say that
// plainly instead of calling something that does not exist.
func TestSDCardEndpointsSayUnavailable(t *testing.T) {
	sm, streamMgr, _, _ := setupTestEnvironment()
	sm.SetStateForTest(SessionStatusAuthenticated, "user@example.com", "uid-1", "",
		[]bridge.Device{{UUID: "cam-sd", Name: "Test", Type: "WS03", Online: 1}})
	if _, err := streamMgr.Enroll([]string{"cam-sd"}, []bridge.Device{{UUID: "cam-sd", Name: "Test", Type: "WS03"}}); err != nil {
		t.Fatalf("enroll: %v", err)
	}

	for _, path := range []string{"/api/v1/cameras/cam-sd/sdcard", "/api/v1/cameras/cam-sd/sdcard/recordings"} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.RemoteAddr = "127.0.0.1:50000"
		handleSDCardInfo(rec, req, "cam-sd", streamMgr, sm)
		if rec.Code != http.StatusNotImplemented {
			t.Fatalf("%s: got %d, want 501", path, rec.Code)
		}
		var body map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		msg, _ := body["message"].(string)
		if body["error"] != "sdcard_unavailable" || !strings.Contains(msg, "P2P") {
			t.Fatalf("%s: %v", path, body)
		}
	}
}
