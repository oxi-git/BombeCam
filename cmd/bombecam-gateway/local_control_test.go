package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Fever-r/BombeCam/pkg/bridge"
	"github.com/gorilla/websocket"
)

func TestLocalControlsNeverFallBackToAvailableCloud(t *testing.T) {
	var sent atomic.Int64
	ws := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
		c, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		for {
			_, _, err := c.ReadMessage()
			if err != nil {
				return
			}
			sent.Add(1)
		}
	}))
	defer ws.Close()
	sig, err := bridge.Connect("ws"+strings.TrimPrefix(ws.URL, "http"), "fixture-token", "fixture-user", "fixture-phone")
	if err != nil {
		t.Fatal(err)
	}
	defer sig.Close()
	sm := NewStreamManager(nil, "rtsp://127.0.0.1:8554", "http://127.0.0.1:8888", nil, false, "")
	sm.SetRunner(func(ctx context.Context, _ *ManagedCamera, _ int) { <-ctx.Done() })
	defer sm.CloseAll()
	if _, err := sm.Enroll([]string{"fixture-camera"}, []bridge.Device{{UUID: "fixture-camera", Name: "Fixture", Type: "WS03"}}); err != nil {
		t.Fatal(err)
	}
	mc, _ := sm.GetCamera("fixture-camera")
	mc.Signaling = sig
	ch := bridge.NewMQTTControlChannel(nil, 25*time.Millisecond)
	defer ch.Close()
	previous := GatewayControlChannel()
	SetGatewayControlChannel(ch)
	defer SetGatewayControlChannel(previous)
	cases := []string{
		`{"action":"ir","mode":"off"}`, `{"action":"led","value":1}`,
		`{"action":"light","value":1}`, `{"action":"ptz","direction":1,"duration_ms":10}`,
		`{"action":"motion","value":1}`, `{"action":"sound","value":1}`,
	}
	for _, body := range cases {
		rr := httptest.NewRecorder()
		handleCameraControl(rr, httptest.NewRequest("POST", "/", strings.NewReader(body)), "fixture-camera", sm)
		if rr.Code != http.StatusServiceUnavailable {
			t.Fatalf("missing camera report for %s returned %d: %s", body, rr.Code, rr.Body.String())
		}
	}
	rr := httptest.NewRecorder()
	handleDedicatedPTZ(rr, httptest.NewRequest("POST", "/", strings.NewReader(`{"direction":1,"duration_ms":10}`)), "fixture-camera", sm)
	if rr.Code != 503 {
		t.Fatalf("dedicated PTZ status=%d", rr.Code)
	}
	for _, handler := range []func(http.ResponseWriter, *http.Request, string, *StreamManager){handleTalkStart, handleTalkStop} {
		rr = httptest.NewRecorder()
		handler(rr, httptest.NewRequest("POST", "/", nil), "fixture-camera", sm)
		if rr.Code != 503 {
			t.Fatalf("talk without local reply returned %d", rr.Code)
		}
	}
	// Draining the open socket after all handlers catches an accidental fallback.
	time.Sleep(30 * time.Millisecond)
	if sent.Load() != 0 {
		t.Fatalf("sent %d commands to cloud in local mode", sent.Load())
	}
}

func TestLocalShadowNeverLeaksCloudOrRequestedValues(t *testing.T) {
	sm := NewStreamManager(nil, "rtsp://127.0.0.1:8554", "http://127.0.0.1:8888", nil, false, "")
	sm.SetRunner(func(ctx context.Context, _ *ManagedCamera, _ int) { <-ctx.Done() })
	defer sm.CloseAll()
	_, _ = sm.Enroll([]string{"fixture-camera"}, []bridge.Device{{UUID: "fixture-camera", Name: "Fixture", Type: "WS03"}})
	mc, _ := sm.GetCamera("fixture-camera")
	mc.UpdateObservedIR("auto")
	mc.UpdateObservedLED("on")
	ch := bridge.NewMQTTControlChannel(nil, 20*time.Millisecond)
	defer ch.Close()
	previous := GatewayControlChannel()
	SetGatewayControlChannel(ch)
	defer SetGatewayControlChannel(previous)
	_ = ch.Daemon().DispatchDesired(context.Background(), "fixture-camera", map[string]any{"IrLedMode": bridge.IRModeAuto, "LedOnOff": 1})
	rr := httptest.NewRecorder()
	handleCameraShadow(rr, httptest.NewRequest("GET", "/", nil), "fixture-camera", sm)
	var response struct {
		State struct {
			Reported map[string]any `json:"reported"`
			Desired  map[string]any `json:"desired"`
		} `json:"state"`
		Control bridge.LocalControlStatus `json:"control"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.State.Reported) > 0 || len(response.State.Desired) > 0 {
		t.Fatalf("fabricated local telemetry: %s", rr.Body.String())
	}
	if response.Control.CloudFallback || response.Control.Route != "local_mqtt" {
		t.Fatalf("wrong control mode: %+v", response.Control)
	}
	status := mc.GetStatus(ch)
	if status.ObservedIR != "unknown" || status.ObservedLED != "unknown" {
		t.Fatalf("cloud cache leaked into local status: %+v", status)
	}
}
