package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/Fever-r/BombeCam/pkg/auth"
	"github.com/Fever-r/BombeCam/pkg/bridge"
)

const (
	// lanGatewayHost is the gateway's own address as a LAN client sees it.
	lanGatewayHost = "192.168.1.20:8654"
	// lanClientAddr is a Home Assistant host on the same network.
	lanClientAddr = "192.168.1.12:51234"
)

// lanPTZEnv builds a gateway whose security boundary is active, with one
// enrolled WS03 and a recording control channel. Requests sent through the
// returned handler carry a non-loopback RemoteAddr, exactly like a LAN client.
func lanPTZEnv(t *testing.T, allowedHosts ...string) (*mockControlChannel, *auth.OperatorManager, http.Handler) {
	t.Helper()

	previousOM := GatewayOperatorManager()
	previousCC := GatewayControlChannel()
	t.Cleanup(func() {
		SetGatewayOperatorManager(previousOM)
		SetGatewayControlChannel(previousCC)
	})

	om := auth.NewOperatorManager(auth.OperatorConfig{
		KeyFile:      filepath.Join(t.TempDir(), "api_tokens.json"),
		AllowedHosts: allowedHosts,
	})
	SetGatewayOperatorManager(om)

	sm := NewSessionManager("1", "test-code", testServerKey)
	streamMgr := NewStreamManager(nil, "rtsp://127.0.0.1:8554", "http://127.0.0.1:8888", nil, false, "ffmpeg")
	streamMgr.SetRunner(func(ctx context.Context, _ *ManagedCamera, _ bridge.Device, _ int) { <-ctx.Done() })
	t.Cleanup(func() { streamMgr.CloseAll() })

	if _, err := streamMgr.Enroll([]string{"ws03-ptz"}, []bridge.Device{{
		UUID: "ws03-ptz", Name: "WS03", Type: "WS03", Online: 1,
	}}); err != nil {
		t.Fatalf("enroll fixture camera: %v", err)
	}

	mock := &mockControlChannel{irMode: 1, ledOn: true}
	SetGatewayControlChannel(mock)

	return mock, om, SecurityBoundaryHandler(SetupAPIMux(sm, streamMgr))
}

func lanPTZRequest(t *testing.T, host, body, token string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/cameras/ws03-ptz/ptz", strings.NewReader(body))
	req.Host = host
	req.RemoteAddr = lanClientAddr
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return req
}

// A bearer token is the programmatic credential a LAN client (Home Assistant)
// uses: no cookie, no CSRF header, and the command reaches the control channel.
func TestPTZ_LANBearerClientReachesControlChannel(t *testing.T) {
	mock, om, handler := lanPTZEnv(t)

	token, err := om.GenerateAPIToken()
	if err != nil {
		t.Fatalf("generate API token: %v", err)
	}

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, lanPTZRequest(t, lanGatewayHost, `{"direction": "left", "duration_ms": 400}`, token))

	if rr.Code != http.StatusOK {
		t.Fatalf("LAN PTZ with bearer token: got %d (%s)", rr.Code, rr.Body.String())
	}
	var resp struct {
		OK            bool   `json:"ok"`
		Camera        string `json:"camera"`
		UUID          string `json:"uuid"`
		Direction     int    `json:"direction"`
		DirectionName string `json:"direction_name"`
		DurationMs    int    `json:"duration_ms"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode PTZ response: %v", err)
	}
	if !resp.OK || resp.UUID != "ws03-ptz" || resp.Direction != 1 || resp.DirectionName != "left" || resp.DurationMs != 400 {
		t.Fatalf("unexpected PTZ response: %+v", resp)
	}
	if calls := mock.ptzCalls(); !reflect.DeepEqual(calls, [][2]int{{1, 400}}) {
		t.Fatalf("control channel calls = %v; want one left/400ms pulse", calls)
	}
}

// Without a bearer token the same LAN request stays locked: this is the 401 a
// Home Assistant setup hits when it has no API token yet.
func TestPTZ_LANWithoutCredentials_Unauthorized(t *testing.T) {
	mock, _, handler := lanPTZEnv(t)

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, lanPTZRequest(t, lanGatewayHost, `{"direction": 1, "duration_ms": 400}`, ""))

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("LAN PTZ without credentials: got %d (%s)", rr.Code, rr.Body.String())
	}
	var resp struct {
		Error   string `json:"error"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode error response: %v", err)
	}
	if resp.Error != "unauthorized" || resp.Message != "Local operator authentication required" {
		t.Fatalf("unexpected 401 body: %+v", resp)
	}
	if calls := mock.ptzCalls(); len(calls) != 0 {
		t.Fatalf("control channel received %v from an unauthenticated request", calls)
	}
}

// A host name is accepted only when the administrator lists it; an IP literal
// (the common Home Assistant URL) already passes the guard.
func TestPTZ_ConfiguredAllowedHost(t *testing.T) {
	t.Run("configured LAN name passes", func(t *testing.T) {
		mock, om, handler := lanPTZEnv(t, "bombecam.lan", "192.168.1.20")
		token, err := om.GenerateAPIToken()
		if err != nil {
			t.Fatalf("generate API token: %v", err)
		}

		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, lanPTZRequest(t, "bombecam.lan:8654", `{"direction": "up"}`, token))

		if rr.Code != http.StatusOK {
			t.Fatalf("PTZ through an allowed LAN name: got %d (%s)", rr.Code, rr.Body.String())
		}
		if calls := mock.ptzCalls(); !reflect.DeepEqual(calls, [][2]int{{3, 400}}) {
			t.Fatalf("control channel calls = %v; want one up/400ms pulse", calls)
		}
	})

	t.Run("unlisted LAN name stays rejected", func(t *testing.T) {
		mock, om, handler := lanPTZEnv(t)
		token, err := om.GenerateAPIToken()
		if err != nil {
			t.Fatalf("generate API token: %v", err)
		}

		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, lanPTZRequest(t, "bombecam.lan:8654", `{"direction": 1}`, token))

		if rr.Code != http.StatusForbidden {
			t.Fatalf("PTZ through an unlisted LAN name: got %d (%s)", rr.Code, rr.Body.String())
		}
		var resp struct {
			Error string `json:"error"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode error response: %v", err)
		}
		if resp.Error != "forbidden_host" {
			t.Fatalf("unexpected 403 body: %s", rr.Body.String())
		}
		if calls := mock.ptzCalls(); len(calls) != 0 {
			t.Fatalf("control channel received %v from a rejected host", calls)
		}
	})
}

// The README's setup flow works as written: mint an API token with the
// administrator's password, then call PTZ from the LAN with that token.
func TestPTZ_DocumentedTokenFlowFromLAN(t *testing.T) {
	_, streamMgr, pm, _, _, handler := setupOperatorTokenEnv(t)
	seedAdmin(t, pm, "admin", "CorrectBootstrapPass123!", false)

	previousCC := GatewayControlChannel()
	t.Cleanup(func() { SetGatewayControlChannel(previousCC) })
	mock := &mockControlChannel{}
	SetGatewayControlChannel(mock)

	streamMgr.SetRunner(func(ctx context.Context, _ *ManagedCamera, _ bridge.Device, _ int) { <-ctx.Done() })
	if _, err := streamMgr.Enroll([]string{"ws03-ptz"}, []bridge.Device{{
		UUID: "ws03-ptz", Name: "WS03", Type: "WS03", Online: 1,
	}}); err != nil {
		t.Fatalf("enroll fixture camera: %v", err)
	}

	mint := httptest.NewRequest(http.MethodPost, "/api/v1/operator/token", nil)
	mint.Host = lanGatewayHost
	mint.RemoteAddr = lanClientAddr
	mint.SetBasicAuth("admin", "CorrectBootstrapPass123!")
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, mint)
	if rr.Code != http.StatusOK {
		t.Fatalf("mint API token from LAN: got %d (%s)", rr.Code, rr.Body.String())
	}
	var minted struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &minted); err != nil {
		t.Fatalf("decode token response: %v", err)
	}
	if !strings.HasPrefix(minted.Token, "bc_tok_") {
		t.Fatalf("minted token = %q; want a bc_tok_ token", minted.Token)
	}

	rr = httptest.NewRecorder()
	handler.ServeHTTP(rr, lanPTZRequest(t, lanGatewayHost, `{"direction": "down", "duration_ms": 600}`, minted.Token))
	if rr.Code != http.StatusOK {
		t.Fatalf("PTZ with the minted token: got %d (%s)", rr.Code, rr.Body.String())
	}
	if calls := mock.ptzCalls(); !reflect.DeepEqual(calls, [][2]int{{4, 600}}) {
		t.Fatalf("control channel calls = %v; want one down/600ms pulse", calls)
	}
}

// Enabling the PTZ path must not open the rest of the API: other routes keep
// asking for credentials from the same LAN client.
func TestSecurity_LANStillRequiresSignInElsewhere(t *testing.T) {
	mock, _, handler := lanPTZEnv(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/inventory/discover", nil)
	req.Host = lanGatewayHost
	req.RemoteAddr = lanClientAddr
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated LAN inventory request: got %d (%s)", rr.Code, rr.Body.String())
	}
	if calls := mock.ptzCalls(); len(calls) != 0 {
		t.Fatalf("unexpected control channel calls: %v", calls)
	}
}

func TestSplitAllowedHosts(t *testing.T) {
	got := splitAllowedHosts(" bombecam.lan , ,homeassistant.local, 192.168.1.20 ")
	want := []string{"bombecam.lan", "homeassistant.local", "192.168.1.20"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("splitAllowedHosts = %v; want %v", got, want)
	}
	if got := splitAllowedHosts("   ,  "); len(got) != 0 {
		t.Fatalf("blank list = %v; want empty", got)
	}
}
