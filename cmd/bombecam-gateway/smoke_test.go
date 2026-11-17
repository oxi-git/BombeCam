package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gorilla/websocket"

	"github.com/Fever-r/BombeCam/pkg/auth"
	"github.com/Fever-r/BombeCam/pkg/bridge"
	"github.com/Fever-r/BombeCam/pkg/policy"
	"github.com/Fever-r/BombeCam/pkg/profile"
)

// TestSmoke_CoreFunctionality is an integrated headless runtime smoke check
// verifying onboarding, talkback audio, and operator session issuance against synthetic mock upstreams.
func TestSmoke_CoreFunctionality(t *testing.T) {
	tmpDir := t.TempDir()
	profilePath := filepath.Join(tmpDir, "profile.enc")
	keyPath := filepath.Join(tmpDir, "profile.key")

	pm, err := profile.NewDefaultManager(profilePath, "test-master-key-0123456789abcdef0123456789abcdef", keyPath)
	if err != nil {
		t.Fatalf("failed to create profile manager: %v", err)
	}
	SetActiveProfileManager(pm)

	// Synthetic mock cloud backend
	mockCloud, mockCloudURL := setupMockCloudServer(t, nil, nil)
	defer mockCloud.Close()

	sm := NewSessionManager("1", "test-phone-code", testServerKey)
	sm.Cloud().Web = mockCloudURL
	sm.Cloud().GlobalBase = mockCloudURL

	streamMgr := NewStreamManager(sm.Cloud(), "rtsp://127.0.0.1:8554", "http://127.0.0.1:8888", nil, false, "ffmpeg")

	om := auth.NewOperatorManager(auth.OperatorConfig{
		KeyFile:      filepath.Join(tmpDir, "api_tokens.json"),
		AllowedHosts: []string{"127.0.0.1", "127.0.0.1:8654", "localhost", "localhost:8654"},
	})
	SetGatewayOperatorManager(om)
	t.Cleanup(func() { SetGatewayOperatorManager(nil) })

	streamMgr.SetProfileManager(pm)
	mux := SetupAPIMux(sm, streamMgr)
	handler := SecurityBoundaryHandler(mux)
	server := httptest.NewServer(handler)
	defer server.Close()

	client := server.Client()

	// Generate programmatic API token for authenticating integration requests
	apiToken, err := om.GenerateAPIToken()
	if err != nil {
		t.Fatalf("failed to generate api token: %v", err)
	}

	// =========================================================================
	// 1. Onboarding never touches the camera firewall
	// =========================================================================
	t.Run("Onboarding_DoesNotTouchRouter", func(t *testing.T) {
		setupBody := map[string]any{
			"account_email": "smokeuser@test.org",
			"password":      "SecretPassword123!",
			"country":       "1",
		}
		data, _ := json.Marshal(setupBody)
		req, _ := http.NewRequest(http.MethodPost, server.URL+"/api/v1/onboarding/setup", bytes.NewReader(data))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+apiToken)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("setup request failed: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("expected HTTP 200 for onboarding, got %d", resp.StatusCode)
		}
		var setupResp map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&setupResp); err != nil {
			t.Fatalf("decode setup response failed: %v", err)
		}
		if setupResp["status"] != "configured" {
			t.Errorf("expected status=configured, got %v", setupResp["status"])
		}
		if _, ok := setupResp["privacy_mode"]; ok {
			t.Errorf("retired privacy_mode field still reported: %v", setupResp)
		}
		prof, err := pm.Load(context.Background())
		if err != nil || prof == nil {
			t.Fatalf("failed to load saved profile: %v", err)
		}
		if prof.Privacy.BlockCloudVideo != nil {
			t.Errorf("onboarding must not set Block cloud video, got %+v", prof.Privacy)
		}

		reqP, _ := http.NewRequest(http.MethodGet, server.URL+"/api/v1/privacy", nil)
		reqP.Header.Set("Authorization", "Bearer "+apiToken)
		respP, err := client.Do(reqP)
		if err != nil {
			t.Fatalf("privacy status failed: %v", err)
		}
		defer respP.Body.Close()
		var priv map[string]any
		_ = json.NewDecoder(respP.Body).Decode(&priv)
		if respP.StatusCode != http.StatusOK || priv["block_cloud_video"] != nil ||
			fmt.Sprint(priv["headline"]) != policy.Headline {
			t.Fatalf("unexpected privacy status %d: %+v", respP.StatusCode, priv)
		}
	})

	// =========================================================================
	// 2. Audio Talkback Streaming (ADTS AAC)
	// =========================================================================
	t.Run("WebTalkback_16kHz_Mono_ADTS_Stream", func(t *testing.T) {
		camID := "cam-smoke-test-1"
		mockDevs := []bridge.Device{
			{UUID: camID, Name: "Smoke Cam", Type: "WS03", Online: 1},
		}
		sm.SetStateForTest(SessionStatusAuthenticated, "smoke@test.org", "uid-smoke", "", mockDevs)
		_, _ = streamMgr.Enroll([]string{camID}, mockDevs)

		mc, ok := streamMgr.GetCamera(camID)
		if !ok {
			t.Fatalf("camera enrollment failed for %s", camID)
		}

		// Active streaming state with real mock viewer containing active AAC audio track
		_, cleanupViewer := setupRealMockViewerForTalkback(t, mc)
		defer cleanupViewer()
		startReq, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("%s/api/v1/cameras/%s/talk/start", server.URL, camID), nil)
		startReq.Header.Set("Authorization", "Bearer "+apiToken)
		startResp, err := client.Do(startReq)
		if err != nil {
			t.Fatal(err)
		}
		startResp.Body.Close()
		if startResp.StatusCode != http.StatusOK {
			t.Fatalf("explicit Talk start failed: %d", startResp.StatusCode)
		}

		// Canonical 16kHz mono AAC-LC ADTS frame matching web/app.js createFallbackADTSFrame()
		// 7-byte ADTS header + 4-byte AAC silence payload (total 11 bytes)
		canonicalFrame := []byte{
			0xFF, 0xF1, 0x60, 0x40, 0x01, 0x7F, 0xFC,
			0x01, 0x18, 0x20, 0x07,
		}

		// 2A. Valid 16kHz mono ADTS frame is accepted with HTTP 200 OK
		streamURL := fmt.Sprintf("%s/api/v1/cameras/%s/talk/stream", server.URL, camID)
		reqValid, _ := http.NewRequest(http.MethodPost, streamURL, bytes.NewReader(canonicalFrame))
		reqValid.Header.Set("Content-Type", "audio/aac")
		reqValid.Header.Set("Authorization", "Bearer "+apiToken)

		respValid, err := client.Do(reqValid)
		if err != nil {
			t.Fatalf("talk stream request failed: %v", err)
		}
		defer respValid.Body.Close()

		if respValid.StatusCode != http.StatusOK {
			t.Fatalf("expected HTTP 200 OK for valid ADTS talkback stream, got %d", respValid.StatusCode)
		}

		var streamResp map[string]any
		if err := json.NewDecoder(respValid.Body).Decode(&streamResp); err != nil {
			t.Fatalf("decode stream response failed: %v", err)
		}
		if streamResp["ok"] != true {
			t.Errorf("expected ok=true, got %v", streamResp["ok"])
		}
		if streamResp["frames_streamed"] != float64(1) {
			t.Errorf("expected frames_streamed=1, got %v", streamResp["frames_streamed"])
		}
		if streamResp["bytes_streamed"] != float64(11) {
			t.Errorf("expected bytes_streamed=11, got %v", streamResp["bytes_streamed"])
		}

		// 2B. Burst of multiple valid ADTS frames accepted cleanly
		burst := make([]byte, 0, len(canonicalFrame)*10)
		for i := 0; i < 10; i++ {
			burst = append(burst, canonicalFrame...)
		}
		reqBurst, _ := http.NewRequest(http.MethodPost, streamURL, bytes.NewReader(burst))
		reqBurst.Header.Set("Content-Type", "audio/aac")
		reqBurst.Header.Set("Authorization", "Bearer "+apiToken)

		respBurst, err := client.Do(reqBurst)
		if err != nil {
			t.Fatalf("talk stream burst request failed: %v", err)
		}
		defer respBurst.Body.Close()

		if respBurst.StatusCode != http.StatusOK {
			t.Fatalf("expected HTTP 200 OK for burst ADTS talkback stream, got %d", respBurst.StatusCode)
		}

		// 2C. Legacy MediaRecorder WebM/EBML payload is rejected with HTTP 400
		webmHeader := []byte{0x1A, 0x45, 0xDF, 0xA3, 0x9F, 0x42, 0x86, 0x81, 0x01, 0x42, 0xF7, 0x81, 0x01}
		reqWebm, _ := http.NewRequest(http.MethodPost, streamURL, bytes.NewReader(webmHeader))
		reqWebm.Header.Set("Content-Type", "audio/webm")
		reqWebm.Header.Set("Authorization", "Bearer "+apiToken)

		respWebm, err := client.Do(reqWebm)
		if err != nil {
			t.Fatalf("talk stream webm request failed: %v", err)
		}
		defer respWebm.Body.Close()

		if respWebm.StatusCode != http.StatusBadRequest {
			t.Fatalf("expected HTTP 400 Bad Request for legacy WebM payload, got %d", respWebm.StatusCode)
		}
	})

	// =========================================================================
	// 3. Scoped Operator Session & Token Issuance
	// =========================================================================
	t.Run("OperatorToken_StrictCallerAuth", func(t *testing.T) {
		tokenURL := server.URL + "/api/v1/operator/token"

		// 3A. Unauthenticated caller without session or credentials -> HTTP 401
		reqUnauth, _ := http.NewRequest(http.MethodPost, tokenURL, nil)
		respUnauth, err := client.Do(reqUnauth)
		if err != nil {
			t.Fatalf("token unauth request failed: %v", err)
		}
		defer respUnauth.Body.Close()

		if respUnauth.StatusCode != http.StatusUnauthorized {
			t.Fatalf("expected HTTP 401 for unauthenticated caller on /api/v1/operator/token, got %d", respUnauth.StatusCode)
		}

		// 3B. Visitor session caller (from GET /api/v1/auth/csrf) without credentials -> HTTP 401
		csrfReq, _ := http.NewRequest(http.MethodGet, server.URL+"/api/v1/auth/csrf", nil)
		csrfResp, err := client.Do(csrfReq)
		if err != nil {
			t.Fatalf("csrf get request failed: %v", err)
		}
		defer csrfResp.Body.Close()

		var csrfData map[string]any
		_ = json.NewDecoder(csrfResp.Body).Decode(&csrfData)
		csrfToken, _ := csrfData["csrf_token"].(string)

		var visitorCookie *http.Cookie
		for _, c := range csrfResp.Cookies() {
			if c.Name == auth.SessionCookieName {
				visitorCookie = c
				break
			}
		}
		if visitorCookie == nil {
			t.Fatal("missing session cookie from /auth/csrf response")
		}

		reqVisitor, _ := http.NewRequest(http.MethodPost, tokenURL, nil)
		reqVisitor.AddCookie(visitorCookie)
		reqVisitor.Header.Set("X-CSRF-Token", csrfToken)

		respVisitor, err := client.Do(reqVisitor)
		if err != nil {
			t.Fatalf("visitor token request failed: %v", err)
		}
		defer respVisitor.Body.Close()

		if respVisitor.StatusCode != http.StatusUnauthorized {
			t.Fatalf("expected HTTP 401 for stranger/visitor session without elevation, got %d", respVisitor.StatusCode)
		}

		// 3C. Logged-in vendor state does NOT authorize stranger sessions
		sm.SetStateForTest(SessionStatusAuthenticated, "vendor@osaio.net", "uid-vendor-123", "token-vendor-456", nil)
		reqVendorCheck, _ := http.NewRequest(http.MethodPost, tokenURL, nil)
		reqVendorCheck.AddCookie(visitorCookie)
		reqVendorCheck.Header.Set("X-CSRF-Token", csrfToken)

		respVendorCheck, err := client.Do(reqVendorCheck)
		if err != nil {
			t.Fatalf("vendor check token request failed: %v", err)
		}
		defer respVendorCheck.Body.Close()

		if respVendorCheck.StatusCode != http.StatusUnauthorized {
			t.Fatalf("expected HTTP 401: vendor cloud login must not grant operator tokens to stranger sessions, got %d", respVendorCheck.StatusCode)
		}

		// 3D. The administrator's name and password -> HTTP 200 OK + elevates session
		seedAdmin(t, pm, "admin", "AdminPassword123!", false)
		bootstrapCreds := map[string]any{
			"name":     "admin",
			"password": "AdminPassword123!",
		}
		credsData, _ := json.Marshal(bootstrapCreds)
		reqElevate, _ := http.NewRequest(http.MethodPost, tokenURL, bytes.NewReader(credsData))
		reqElevate.Header.Set("Content-Type", "application/json")
		reqElevate.AddCookie(visitorCookie)
		reqElevate.Header.Set("X-CSRF-Token", csrfToken)

		respElevate, err := client.Do(reqElevate)
		if err != nil {
			t.Fatalf("token elevation request failed: %v", err)
		}
		defer respElevate.Body.Close()

		if respElevate.StatusCode != http.StatusOK {
			t.Fatalf("expected HTTP 200 OK for valid bootstrap credentials, got %d", respElevate.StatusCode)
		}

		var tokResp map[string]any
		if err := json.NewDecoder(respElevate.Body).Decode(&tokResp); err != nil {
			t.Fatalf("decode token response failed: %v", err)
		}
		if tokResp["ok"] != true {
			t.Errorf("expected ok=true, got %v", tokResp["ok"])
		}
		tokenStr, ok := tokResp["token"].(string)
		if !ok || !strings.HasPrefix(tokenStr, "bc_tok_") {
			t.Errorf("expected valid operator token with prefix 'bc_tok_', got %v", tokResp["token"])
		}

		// 3E. Subsequent request from now-elevated session -> HTTP 200 OK without needing credentials again
		reqSubsequent, _ := http.NewRequest(http.MethodPost, tokenURL, nil)
		reqSubsequent.AddCookie(visitorCookie)
		reqSubsequent.Header.Set("X-CSRF-Token", csrfToken)

		respSubsequent, err := client.Do(reqSubsequent)
		if err != nil {
			t.Fatalf("subsequent token request failed: %v", err)
		}
		defer respSubsequent.Body.Close()

		if respSubsequent.StatusCode != http.StatusOK {
			t.Fatalf("expected HTTP 200 OK for elevated operator session, got %d", respSubsequent.StatusCode)
		}
	})
}

// setupRealMockViewerForTalkback initializes a mock WebSocket signaling server,
// a connected bridge.Signaling instance, and a real bridge.Viewer with an active AAC audio track.
func setupRealMockViewerForTalkback(t *testing.T, mc *ManagedCamera) (*bridge.Viewer, func()) {
	upgrader := websocket.Upgrader{}
	wsServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		for {
			_, msg, err := c.ReadMessage()
			if err != nil {
				break
			}
			var m map[string]any
			if err := json.Unmarshal(msg, &m); err == nil {
				method, _ := m["method"].(string)
				if strings.Contains(method, "service.Talk") {
					resp := map[string]any{
						"method": "response.TalkResp",
						"msg_id": m["msg_id"],
						"data":   map[string]any{"ret": 0},
					}
					raw, _ := json.Marshal(resp)
					_ = c.WriteMessage(websocket.TextMessage, raw)
				}
			}
		}
	}))

	wsURL := "ws" + strings.TrimPrefix(wsServer.URL, "http")
	sig, err := bridge.Connect(wsURL, "token-test", "uid-test", "39")
	if err != nil {
		wsServer.Close()
		t.Fatalf("ws connect failed: %v", err)
	}

	vc := &bridge.VideoCall{SessionID: "fixture-talk-session"}
	viewer, err := bridge.NewViewer(sig, vc, mc.UUID, "WS03", "call-smoke-"+mc.UUID, "ffmpeg", "rtsp://127.0.0.1:8554/"+mc.UUID, 8554)
	if err != nil {
		_ = sig.Close()
		wsServer.Close()
		t.Fatalf("NewViewer failed: %v", err)
	}

	sig.OnMsg = viewer.OnMessage

	mc.mu.Lock()
	mc.Streaming = true
	mc.Viewer = viewer
	mc.mu.Unlock()

	cleanup := func() {
		viewer.Close()
		_ = sig.Close()
		wsServer.Close()
	}

	return viewer, cleanup
}
