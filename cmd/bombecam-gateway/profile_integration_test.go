package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Fever-r/BombeCam/pkg/profile"
)

func setupMockCloudServer(t *testing.T, loginCounter, deviceListCounter *int) (*httptest.Server, string) {
	var serverURL string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v2/account/get-baseurl":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"code": 1000,
				"data": map[string]any{
					"region": "US",
					"web":    serverURL,
					"ws":     "wss://mock.osaio.net/ws",
				},
			})
		case "/v2/login/login":
			if loginCounter != nil {
				*loginCounter++
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"code": 1000,
				"data": map[string]any{
					"uid":       "mock-uid-reauth",
					"api_token": "mock-token-refreshed-999",
				},
			})
		case "/v2/device/list":
			if deviceListCounter != nil {
				*deviceListCounter++
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"code": 1000,
				"data": map[string]any{
					"data": []map[string]any{
						{
							"uuid":       "mock-uuid-001",
							"name":       "Front Yard Cam",
							"type":       "WS03",
							"model_type": 1,
							"online":     1,
						},
					},
				},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	serverURL = server.URL
	return server, serverURL
}

func TestGateway_FirstRunDetection(t *testing.T) {
	tmpDir := t.TempDir()
	profilePath := filepath.Join(tmpDir, "nonexistent.enc")
	keyPath := filepath.Join(tmpDir, "profile.key")

	*profilePathFlag = profilePath
	*profileKeyFlag = ""
	*profileKeyFileFlag = keyPath

	sm := NewSessionManager("1", "phone-1", testServerKey)
	stm := NewStreamManager(sm.Cloud(), "rtsp://127.0.0.1:8554", "http://127.0.0.1:8888", nil, false, "ffmpeg")
	stm.SetSessionRegistry(NewSessionRegistry(sm, sm.Country(), sm.PhoneCode(), testServerKey))
	setCurrentStreamManager(stm)
	t.Cleanup(func() { setCurrentStreamManager(nil) })

	runGatewayStartup(sm, stm, "", "", "", "", "", "127.0.0.1:8654")

	status := sm.GetStatus()
	if status.Status != SessionStatusCredentialsMissing {
		t.Fatalf("expected status %s, got %s", SessionStatusCredentialsMissing, status.Status)
	}
	if !strings.Contains(status.Error, "first-run") {
		t.Fatalf("expected first-run error message, got: %s", status.Error)
	}
}

func TestGateway_FirstRunBootstrap(t *testing.T) {
	tmpDir := t.TempDir()
	profilePath := filepath.Join(tmpDir, "profile.enc")
	keyPath := filepath.Join(tmpDir, "profile.key")

	*profilePathFlag = profilePath
	*profileKeyFlag = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	*profileKeyFileFlag = keyPath

	server, serverURL := setupMockCloudServer(t, nil, nil)
	defer server.Close()

	sm := NewSessionManager("1", "phone-1", testServerKey)
	sm.Cloud().Web = serverURL
	sm.Cloud().GlobalBase = serverURL
	stm := NewStreamManager(sm.Cloud(), "rtsp://127.0.0.1:8554", "http://127.0.0.1:8888", nil, false, "ffmpeg")
	stm.SetSessionRegistry(NewSessionRegistry(sm, sm.Country(), sm.PhoneCode(), testServerKey))
	setCurrentStreamManager(stm)
	t.Cleanup(func() { setCurrentStreamManager(nil) })
	defer stm.CloseAll()

	runGatewayStartup(sm, stm, "newuser@example.com", "SecretPass123", "1", "mock-uuid-001", "", "127.0.0.1:8654")
	login, ok := currentStreamManager().Registry().Get("newuser@example.com")
	if !ok {
		t.Fatal("login newuser@example.com has no session after start-up")
	}

	// 1. Verify session authenticated
	if !login.IsAuthenticated() {
		t.Fatalf("expected session to be authenticated, status: %+v", currentStreamManager().Registry().Status())
	}

	// 2. Verify encrypted profile file created on disk
	fi, err := os.Stat(profilePath)
	if err != nil || fi.Size() == 0 {
		t.Fatalf("expected profile file created at %s, err: %v", profilePath, err)
	}

	rawBytes, err := os.ReadFile(profilePath)
	if err != nil {
		t.Fatalf("failed to read profile file: %v", err)
	}
	rawStr := string(rawBytes)
	// Assert no plaintext credentials in on-disk file
	if strings.Contains(rawStr, "SecretPass123") {
		t.Fatalf("SECURITY VIOLATION: plaintext password found in profile file: %s", rawStr)
	}
	if !strings.Contains(rawStr, "BOMBE_ENC_V1") {
		t.Fatalf("expected BOMBE_ENC_V1 envelope magic header in %s", rawStr)
	}

	// 3. Verify camera enrolled in stream manager
	if _, ok := stm.GetCamera("mock-uuid-001"); !ok {
		t.Fatalf("expected camera mock-uuid-001 enrolled in StreamManager")
	}
}

func TestGateway_RestartRestoration_FromCachedToken(t *testing.T) {
	ctx := context.Background()
	tmpDir := t.TempDir()
	profilePath := filepath.Join(tmpDir, "profile.enc")
	key := []byte("01234567890123456789012345678901") // 32 bytes

	var loginCalls, devListCalls int
	server, serverURL := setupMockCloudServer(t, &loginCalls, &devListCalls)
	defer server.Close()

	// Pre-create encrypted profile on disk
	pm := profile.NewManager(profilePath, key)
	initialProfile := &profile.Profile{
		Version:   1,
		CreatedAt: time.Now().UTC(),
		Privacy: profile.PrivacySettings{
			BlockCloudVideo: profile.BoolPtr(true), RouterAddress: "192.168.8.1:22",
			RouterHostKey: "SHA256:pinned", AppliedCameras: []string{"aa:bb:cc:dd:ee:01"},
		},
		Credentials: profile.CloudCredentials{
			AccountEmail:   "restart@example.com",
			Password:       profile.SecretString("SavedPassword#123"),
			VendorUID:      "mock-uid-cached",
			AuthToken:      profile.SecretString("mock-cached-token-123"),
			TokenExpiresAt: time.Now().UTC().Add(12 * time.Hour), // Valid unexpired token
			Region:         serverURL,
			Country:        "1",
		},
		Cameras: map[string]profile.CameraProfile{
			"mock-uuid-001": {
				UUID:       "mock-uuid-001",
				Name:       "Front Yard Cam",
				Model:      "WS03",
				IPAddress:  "192.168.1.150",
				EnrolledAt: time.Now().UTC(),
				Online:     true,
			},
		},
		Connection: profile.ConnectionParameters{
			Timezone:       "UTC",
			TimezoneOffset: 0.0,
		},
	}
	if err := pm.Save(ctx, initialProfile); err != nil {
		t.Fatalf("failed to pre-seed profile: %v", err)
	}

	// Configure flags for simulated restart
	*profilePathFlag = profilePath
	*profileKeyFlag = string(key)
	*profileKeyFileFlag = ""

	// Fresh gateway instance
	sm := NewSessionManager("1", "phone-restarted", testServerKey)
	sm.Cloud().Web = serverURL
	sm.Cloud().GlobalBase = serverURL
	stm := NewStreamManager(sm.Cloud(), "rtsp://127.0.0.1:8554", "http://127.0.0.1:8888", nil, false, "ffmpeg")
	stm.SetSessionRegistry(NewSessionRegistry(sm, sm.Country(), sm.PhoneCode(), testServerKey))
	setCurrentStreamManager(stm)
	t.Cleanup(func() { setCurrentStreamManager(nil) })
	defer stm.CloseAll()

	// Call restart startup with NO CLI credentials
	runGatewayStartup(sm, stm, "", "", "", "", "", "127.0.0.1:8654")
	login, ok := currentStreamManager().Registry().Get("restart@example.com")
	if !ok {
		t.Fatal("login restart@example.com has no session after start-up")
	}

	// 1. Fast-path check: login should NOT be called because token is unexpired and validated
	if loginCalls > 0 {
		t.Fatalf("expected fast-path token restoration without login, but login was called %d times", loginCalls)
	}

	// 2. Authentication restored
	if !login.IsAuthenticated() {
		t.Fatalf("expected session to be authenticated via cached token, status: %+v", currentStreamManager().Registry().Status())
	}
	if login.Cloud().UID != "mock-uid-cached" {
		t.Fatalf("expected UID mock-uid-cached, got %s", login.Cloud().UID)
	}
	if login.Cloud().APIToken != "mock-cached-token-123" {
		t.Fatalf("expected APIToken mock-cached-token-123, got %s", login.Cloud().APIToken)
	}

	// 3. The applied Block cloud video setting survives a restart untouched
	// (the router keeps the rules itself; the gateway never re-applies them).
	reloaded, err := profile.NewManager(profilePath, key).Load(ctx)
	if err != nil || reloaded.Privacy.BlockCloudVideo == nil || !*reloaded.Privacy.BlockCloudVideo ||
		reloaded.Privacy.RouterHostKey != "SHA256:pinned" {
		t.Fatalf("privacy settings not preserved across restart: %+v (%v)", reloaded, err)
	}

	// 4. Enrolled camera restored from profile
	mc, ok := stm.GetCamera("mock-uuid-001")
	if !ok {
		t.Fatalf("expected camera mock-uuid-001 to be restored in StreamManager")
	}
	if mc.IP != "192.168.1.150" {
		t.Fatalf("expected restored IP 192.168.1.150, got %s", mc.IP)
	}
}

func TestGateway_RestartRestoration_ExpiredTokenFallback(t *testing.T) {
	ctx := context.Background()
	tmpDir := t.TempDir()
	profilePath := filepath.Join(tmpDir, "profile.enc")
	key := []byte("01234567890123456789012345678901")

	var loginCalls int
	server, serverURL := setupMockCloudServer(t, &loginCalls, nil)
	defer server.Close()

	// Pre-seed profile with EXPIRED token but valid password
	pm := profile.NewManager(profilePath, key)
	initialProfile := &profile.Profile{
		Version:   1,
		CreatedAt: time.Now().UTC(),
		Credentials: profile.CloudCredentials{
			AccountEmail:   "reauth@example.com",
			Password:       profile.SecretString("ValidSavedPassword#123"),
			VendorUID:      "mock-uid-old",
			AuthToken:      profile.SecretString("expired-token"),
			TokenExpiresAt: time.Now().UTC().Add(-1 * time.Hour), // Expired!
			Region:         serverURL,
			Country:        "1",
		},
		Cameras: map[string]profile.CameraProfile{
			"mock-uuid-001": {
				UUID:       "mock-uuid-001",
				Name:       "Backyard Cam",
				Model:      "WS03",
				EnrolledAt: time.Now().UTC(),
				Online:     true,
			},
		},
	}
	if err := pm.Save(ctx, initialProfile); err != nil {
		t.Fatalf("failed to pre-seed profile: %v", err)
	}

	*profilePathFlag = profilePath
	*profileKeyFlag = string(key)
	*profileKeyFileFlag = ""

	sm := NewSessionManager("1", "phone-restarted", testServerKey)
	sm.Cloud().Web = serverURL
	sm.Cloud().GlobalBase = serverURL
	stm := NewStreamManager(sm.Cloud(), "rtsp://127.0.0.1:8554", "http://127.0.0.1:8888", nil, false, "ffmpeg")
	stm.SetSessionRegistry(NewSessionRegistry(sm, sm.Country(), sm.PhoneCode(), testServerKey))
	setCurrentStreamManager(stm)
	t.Cleanup(func() { setCurrentStreamManager(nil) })
	defer stm.CloseAll()

	runGatewayStartup(sm, stm, "", "", "", "", "", "127.0.0.1:8654")
	login, ok := currentStreamManager().Registry().Get("reauth@example.com")
	if !ok {
		t.Fatal("login reauth@example.com has no session after start-up")
	}

	// 1. Fallback login must have been called
	if loginCalls != 1 {
		t.Fatalf("expected exactly 1 login call on expired token fallback, got %d", loginCalls)
	}

	// 2. Authenticated with refreshed token
	if !login.IsAuthenticated() {
		t.Fatalf("expected session to be authenticated after fallback login")
	}
	if login.Cloud().APIToken != "mock-token-refreshed-999" {
		t.Fatalf("expected refreshed token mock-token-refreshed-999, got %s", login.Cloud().APIToken)
	}

	// 4. Refreshed token persisted back to disk
	reloaded, err := pm.Load(ctx)
	if err != nil {
		t.Fatalf("failed to reload profile: %v", err)
	}
	if reloaded.Accounts[0].AuthToken.Expose() != "mock-token-refreshed-999" {
		t.Fatalf("refreshed token not persisted to profile: %s", reloaded.Accounts[0].AuthToken.Expose())
	}
}

func TestGateway_TamperedProfileRejection(t *testing.T) {
	tmpDir := t.TempDir()
	profilePath := filepath.Join(tmpDir, "profile.enc")

	// Write garbage to profile file
	_ = os.WriteFile(profilePath, []byte("NOT_VALID_ENCRYPTED_JSON_PAYLOAD"), 0600)

	*profilePathFlag = profilePath
	*profileKeyFlag = "some-key"
	*profileKeyFileFlag = ""

	sm := NewSessionManager("1", "phone-1", testServerKey)
	stm := NewStreamManager(sm.Cloud(), "rtsp://127.0.0.1:8554", "http://127.0.0.1:8888", nil, false, "ffmpeg")
	stm.SetSessionRegistry(NewSessionRegistry(sm, sm.Country(), sm.PhoneCode(), testServerKey))
	setCurrentStreamManager(stm)
	t.Cleanup(func() { setCurrentStreamManager(nil) })

	runGatewayStartup(sm, stm, "", "", "", "", "", "127.0.0.1:8654")

	status := sm.GetStatus()
	if status.Status != SessionStatusUnavailable {
		t.Fatalf("expected status %s on tampered profile, got %s", SessionStatusUnavailable, status.Status)
	}
}
