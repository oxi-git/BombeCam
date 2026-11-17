package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Fever-r/BombeCam/pkg/bridge"
	"github.com/Fever-r/BombeCam/pkg/profile"
)

func setupFullTestEnvironment(t *testing.T) (*SessionManager, *StreamManager, profile.ProfileManager, *mockControlChannel, *http.ServeMux, string) {
	t.Helper()
	tmpDir := t.TempDir()
	profilePath := filepath.Join(tmpDir, "profile.enc")
	keyPath := filepath.Join(tmpDir, "profile.key")

	pm, err := profile.NewDefaultManager(profilePath, "test-master-key-0123456789abcdef0123456789abcdef", keyPath)
	if err != nil {
		t.Fatalf("failed to create profile manager: %v", err)
	}
	SetActiveProfileManager(pm)

	sm := NewSessionManager("1", "test-phone-code", testServerKey)
	streamMgr := NewStreamManager(nil, "rtsp://127.0.0.1:8554", "http://127.0.0.1:8888", nil, false, "ffmpeg")
	streamMgr.SetRunner(func(ctx context.Context, mc *ManagedCamera, rtpPort int) {
		<-ctx.Done()
	})

	mockCC := &mockControlChannel{irMode: 1, ledOn: true}
	SetGatewayControlChannel(mockCC)

	streamMgr.SetProfileManager(pm)
	// Same wiring as main: the manager the gateway reads from, and its pool.
	setCurrentStreamManager(streamMgr)
	t.Cleanup(func() { setCurrentStreamManager(nil) })
	ensureTestRegistry(streamMgr, sm, sm.Country(), sm.PhoneCode(), testServerKey)
	mux := SetupAPIMux(sm, streamMgr)
	return sm, streamMgr, pm, mockCC, mux, profilePath
}

func TestOnboarding_Status_FirstRun_And_Configured(t *testing.T) {
	sm, streamMgr, pm, _, mux, _ := setupFullTestEnvironment(t)
	ctx := context.Background()

	// 1. Initial State: No profile on disk (first-run mode)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/onboarding/status", nil)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d", rr.Code)
	}

	var statusResp map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &statusResp); err != nil {
		t.Fatalf("failed to parse json response: %v", err)
	}

	if statusResp["has_profile"] != false {
		t.Errorf("expected has_profile == false, got %v", statusResp["has_profile"])
	}
	if statusResp["first_run"] != true {
		t.Errorf("expected first_run == true, got %v", statusResp["first_run"])
	}
	if v, ok := statusResp["block_cloud_video"]; !ok || v != nil {
		t.Errorf("expected block_cloud_video == null before it is ever applied, got %v", v)
	}
	if _, ok := statusResp["privacy_mode"]; ok {
		t.Errorf("the retired privacy_mode field must not be reported")
	}
	if statusResp["enrolled_cameras_count"].(float64) != 0 {
		t.Errorf("expected 0 enrolled cameras, got %v", statusResp["enrolled_cameras_count"])
	}

	// 2. State after Profile Configured
	testProf := &profile.Profile{
		Version:   profile.CurrentSchemaVersion,
		CreatedAt: time.Now().UTC(),
		Privacy:   profile.PrivacySettings{BlockCloudVideo: profile.BoolPtr(true)},
		Credentials: profile.CloudCredentials{
			AccountEmail: "user@example.com",
			Password:     profile.SecretString("SecretPass123"),
			VendorUID:    "uid-test-123",
		},
		Cameras: map[string]profile.CameraProfile{
			"cam-uuid-1": {UUID: "cam-uuid-1", Name: "Front Door", Online: true},
		},
	}
	if err := pm.Save(ctx, testProf); err != nil {
		t.Fatalf("failed to save profile: %v", err)
	}
	sm.SetStateForTest(SessionStatusAuthenticated, "user@example.com", "uid-test-123", "", nil)

	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d", rr.Code)
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &statusResp); err != nil {
		t.Fatalf("failed to parse json response: %v", err)
	}

	if statusResp["has_profile"] != true {
		t.Errorf("expected has_profile == true, got %v", statusResp["has_profile"])
	}
	if statusResp["first_run"] != false {
		t.Errorf("expected first_run == false, got %v", statusResp["first_run"])
	}
	if _, named := statusResp["account_email"]; named {
		t.Errorf("the status must not single out a login, got account_email %v", statusResp["account_email"])
	}
	if statusResp["logins_count"] != float64(1) {
		t.Errorf("expected logins_count == 1, got %v", statusResp["logins_count"])
	}
	if statusResp["ready_for_discovery"] != true {
		t.Errorf("expected ready_for_discovery == true, got %v", statusResp["ready_for_discovery"])
	}
	_ = streamMgr
}

func TestOnboarding_Setup_ValidationErrors(t *testing.T) {
	_, _, _, _, mux, _ := setupFullTestEnvironment(t)

	// Missing email
	body := strings.NewReader(`{"password": "secretPassword"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/onboarding/setup", body)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 Bad Request for missing email, got %d", rr.Code)
	}

	// Missing password
	body = strings.NewReader(`{"account_email": "user@example.com"}`)
	req = httptest.NewRequest(http.MethodPost, "/api/v1/onboarding/setup", body)
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 Bad Request for missing password, got %d", rr.Code)
	}

	// Malformed JSON
	body = strings.NewReader(`{invalid json`)
	req = httptest.NewRequest(http.MethodPost, "/api/v1/onboarding/setup", body)
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 Bad Request for malformed JSON, got %d", rr.Code)
	}

	// Wrong HTTP method
	req = httptest.NewRequest(http.MethodGet, "/api/v1/onboarding/setup", nil)
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405 Method Not Allowed for GET, got %d", rr.Code)
	}
}

func TestOnboarding_Setup_AuthFailure(t *testing.T) {
	sm, _, pm, _, mux, _ := setupFullTestEnvironment(t)

	var mockURL string
	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v2/account/get-baseurl":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"code": 1000,
				"data": map[string]any{"region": "US", "web": mockURL, "ws": "wss://mock/ws"},
			})
		case "/v2/login/login":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"code": 2001,
				"msg":  "invalid password",
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer mockServer.Close()
	mockURL = mockServer.URL

	sm.Cloud().GlobalBase = mockServer.URL

	body := strings.NewReader(`{
		"account_email": "baduser@example.com",
		"password": "WrongPassword123"
	}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/onboarding/setup", body)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 Unauthorized for rejected credentials, got %d", rr.Code)
	}

	// Ensure no profile was written to storage!
	hasProfile, _ := pm.HasProfile(context.Background())
	if hasProfile {
		t.Fatal("profile was persisted to disk after authentication failure; MUST NOT persist on failure")
	}
}

func TestOnboarding_Setup_Success_And_ZeroSecretLeakage(t *testing.T) {
	sm, _, pm, _, mux, profilePath := setupFullTestEnvironment(t)

	var loginCount int
	mockServer, mockURL := setupMockCloudServer(t, &loginCount, nil)
	defer mockServer.Close()

	sm.Cloud().GlobalBase = mockURL

	sensitivePassword := "SuperSecretUserPass99!"
	sensitiveSecondaryPass := "SecondarySecretPass88!"

	setupPayload := map[string]any{
		"account_email": "realuser@example.com",
		"password":      sensitivePassword,
		"country":       "1",
		"secondary_account": map[string]any{
			"enabled":       true,
			"account_email": "sec@example.com",
			"password":      sensitiveSecondaryPass,
			"role":          "guest",
			"notes":         "Secondary account for NVR",
		},
		"allowed_subnets": []string{"192.168.1.0/24"},
		"timezone":        "America/New_York",
		"timezone_offset": -5.0,
		"force":           false,
	}
	payloadBytes, _ := json.Marshal(setupPayload)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/onboarding/setup", bytes.NewReader(payloadBytes))
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 OK for successful setup, got %d. Body: %s", rr.Code, rr.Body.String())
	}

	respStr := rr.Body.String()

	// ASSERT ZERO SECRET LEAKAGE in HTTP response payload
	if strings.Contains(respStr, sensitivePassword) {
		t.Fatalf("SECURITY VIOLATION: plaintext primary password leaked in onboarding setup response: %s", respStr)
	}
	if strings.Contains(respStr, sensitiveSecondaryPass) {
		t.Fatalf("SECURITY VIOLATION: plaintext secondary password leaked in onboarding setup response: %s", respStr)
	}
	if strings.Contains(respStr, "mock-token-refreshed-999") {
		t.Fatalf("SECURITY VIOLATION: auth token leaked in onboarding setup response: %s", respStr)
	}

	// Verify profile was saved to disk
	hasProfile, err := pm.HasProfile(context.Background())
	if err != nil || !hasProfile {
		t.Fatalf("expected profile to be saved to disk, hasProfile=%v, err=%v", hasProfile, err)
	}

	// Read raw disk file bytes and verify ciphertext contains ZERO plaintext passwords
	rawDiskBytes, err := os.ReadFile(profilePath)
	if err != nil {
		t.Fatalf("failed to read profile file on disk: %v", err)
	}
	if bytes.Contains(rawDiskBytes, []byte(sensitivePassword)) {
		t.Fatalf("SECURITY VIOLATION: raw disk file contains plaintext password")
	}
	if bytes.Contains(rawDiskBytes, []byte(sensitiveSecondaryPass)) {
		t.Fatalf("SECURITY VIOLATION: raw disk file contains plaintext secondary password")
	}

	// Verify loaded profile can decrypt credentials
	loaded, err := pm.Load(context.Background())
	if err != nil {
		t.Fatalf("failed to decrypt saved profile: %v", err)
	}
	if len(loaded.Accounts) != 1 || loaded.Accounts[0].Password.Expose() != sensitivePassword {
		t.Fatalf("expected the login with its decrypted password, got %+v", loaded.Accounts)
	}
	// The retired "secondary account" settings are ignored, not stored.
	if bytes.Contains([]byte(loaded.String()), []byte("sec@example.com")) {
		t.Fatalf("retired secondary account was stored: %s", loaded)
	}
	if loaded.Privacy.BlockCloudVideo != nil {
		t.Fatalf("signing in must not set Block cloud video, got %+v", loaded.Privacy)
	}
	if strings.Contains(respStr, "privacy_mode") {
		t.Fatalf("setup response must not carry the retired privacy_mode field: %s", respStr)
	}

	// Verify conflict without force
	conflictReq := httptest.NewRequest(http.MethodPost, "/api/v1/onboarding/setup", bytes.NewReader(payloadBytes))
	conflictRR := httptest.NewRecorder()
	mux.ServeHTTP(conflictRR, conflictReq)
	if conflictRR.Code != http.StatusConflict {
		t.Fatalf("expected 409 Conflict when profile already exists, got %d", conflictRR.Code)
	}

	// Verify success with force: true
	setupPayload["force"] = true
	forceBytes, _ := json.Marshal(setupPayload)
	forceReq := httptest.NewRequest(http.MethodPost, "/api/v1/onboarding/setup", bytes.NewReader(forceBytes))
	forceRR := httptest.NewRecorder()
	mux.ServeHTTP(forceRR, forceReq)
	if forceRR.Code != http.StatusOK {
		t.Fatalf("expected 200 OK when force: true, got %d", forceRR.Code)
	}
}

func TestOnboarding_Profile_PublicView(t *testing.T) {
	sm, _, pm, _, mux, _ := setupFullTestEnvironment(t)

	// 1. Initial State: No profile -> 404
	req := httptest.NewRequest(http.MethodGet, "/api/v1/onboarding/profile", nil)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("expected 404 Not Found when profile does not exist, got %d", rr.Code)
	}

	// 2. Save profile
	prof := &profile.Profile{
		Version:   profile.CurrentSchemaVersion,
		CreatedAt: time.Now().UTC(),
		Privacy:   profile.PrivacySettings{BlockCloudVideo: profile.BoolPtr(true)},
		Credentials: profile.CloudCredentials{
			AccountEmail: "viewuser@example.com",
			Password:     profile.SecretString("SecretPass123"),
			VendorUID:    "uid-view-01",
		},
	}
	_ = pm.Save(context.Background(), prof)
	sm.SetStateForTest(SessionStatusAuthenticated, "viewuser@example.com", "uid-view-01", "", nil)

	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d", rr.Code)
	}

	respStr := rr.Body.String()
	if strings.Contains(respStr, "SecretPass123") {
		t.Fatalf("SECURITY VIOLATION: plaintext secret found in /api/v1/onboarding/profile: %s", respStr)
	}

	var pubView profile.PublicProfileView
	if err := json.Unmarshal(rr.Body.Bytes(), &pubView); err != nil {
		t.Fatalf("failed to decode PublicProfileView: %v", err)
	}
	if len(pubView.Accounts) != 1 || pubView.Accounts[0].AccountEmail != "viewuser@example.com" {
		t.Errorf("expected the login viewuser@example.com, got %+v", pubView.Accounts)
	}
}

func TestOnboarding_Reset_And_Delete(t *testing.T) {
	sm, streamMgr, pm, _, mux, _ := setupFullTestEnvironment(t)

	// Seed profile & session
	prof := &profile.Profile{
		Version:   profile.CurrentSchemaVersion,
		CreatedAt: time.Now().UTC(),
		Credentials: profile.CloudCredentials{
			AccountEmail: "reset@example.com",
			VendorUID:    "uid-reset",
		},
	}
	_ = pm.Save(context.Background(), prof)
	sm.SetStateForTest(SessionStatusAuthenticated, "reset@example.com", "uid-reset", "", nil)

	// Enroll a mock camera
	dev := bridge.Device{UUID: "mock-cam-del", Name: "Del Cam", Type: "WS03", Online: 1}
	_, _ = streamMgr.Enroll([]string{"mock-cam-del"}, []bridge.Device{dev})

	if streamMgr.CameraCount() == 0 {
		t.Fatal("expected camera to be enrolled")
	}

	// Execute DELETE /api/v1/onboarding/profile
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/onboarding/profile", nil)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d", rr.Code)
	}

	// Verify profile was deleted from disk
	hasProfile, _ := pm.HasProfile(context.Background())
	if hasProfile {
		t.Fatal("profile was not deleted from disk after DELETE /api/v1/onboarding/profile")
	}

	// Verify session reset
	if sm.IsAuthenticated() {
		t.Fatal("session is still authenticated after reset")
	}
	if sm.GetStatus().Status != SessionStatusCredentialsMissing {
		t.Errorf("expected session status %s, got %s", SessionStatusCredentialsMissing, sm.GetStatus().Status)
	}

	// Verify streams closed
	if streamMgr.CameraCount() != 0 {
		t.Errorf("expected 0 cameras in StreamManager after reset, got %d", streamMgr.CameraCount())
	}
	// No router was set up, so the reply says nothing on a router changed.
	var resetBody map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &resetBody)
	if resetBody["router_updated"] != true || !strings.Contains(fmt.Sprint(resetBody["router_message"]), "No router was set up") {
		t.Errorf("unexpected router fields on reset: %+v", resetBody)
	}

	// Verify alias POST /api/v1/onboarding/reset
	_ = pm.Save(context.Background(), prof)
	resetReq := httptest.NewRequest(http.MethodPost, "/api/v1/onboarding/reset", nil)
	resetRR := httptest.NewRecorder()
	mux.ServeHTTP(resetRR, resetReq)
	if resetRR.Code != http.StatusOK {
		t.Fatalf("expected 200 OK for /api/v1/onboarding/reset, got %d", resetRR.Code)
	}
	hasProfAfterReset, _ := pm.HasProfile(context.Background())
	if hasProfAfterReset {
		t.Fatal("profile was not deleted after POST /api/v1/onboarding/reset")
	}
}

func TestOnboarding_CameraDiscovery_Aliasing_And_Auth(t *testing.T) {
	sm, streamMgr, pm, _, mux, _ := setupFullTestEnvironment(t)

	// Pre-seed profile with a camera that has a known LAN IP
	prof := &profile.Profile{
		Version:   profile.CurrentSchemaVersion,
		CreatedAt: time.Now().UTC(),
		Privacy:   profile.PrivacySettings{BlockCloudVideo: profile.BoolPtr(true)},
		Cameras: map[string]profile.CameraProfile{
			"mock-uuid-001": {
				UUID:      "mock-uuid-001",
				Name:      "Front Porch Cam",
				Model:     "WS03",
				IPAddress: "192.168.1.155",
			},
		},
	}
	_ = pm.Save(context.Background(), prof)

	// 1. Unauthenticated check on both routes
	for _, route := range []string{"/api/v1/onboarding/cameras/discover", "/api/v1/inventory/discover"} {
		req := httptest.NewRequest(http.MethodGet, route, nil)
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, req)
		if rr.Code != http.StatusUnauthorized {
			t.Fatalf("[%s] expected 401 Unauthorized when unauthenticated, got %d", route, rr.Code)
		}
	}

	// 2. Authenticate session with mock device inventory
	mockDevs := []bridge.Device{
		{UUID: "mock-uuid-001", Name: "Front Porch Cam", Type: "WS03", Online: 1},
		{UUID: "mock-uuid-002", Name: "Backyard Cam", Type: "WS03", Online: 0},
	}
	sm.SetStateForTest(SessionStatusAuthenticated, "user@example.com", "uid-100", "", mockDevs)

	// 3. Query both endpoints and assert identical response and resolved IP
	var resp1, resp2 string
	for i, route := range []string{"/api/v1/onboarding/cameras/discover", "/api/v1/inventory/discover"} {
		req := httptest.NewRequest(http.MethodGet, route, nil)
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("[%s] expected 200 OK, got %d", route, rr.Code)
		}

		var payload struct {
			Cameras []struct {
				ID     string `json:"id"`
				Name   string `json:"name"`
				Model  string `json:"model"`
				Online bool   `json:"online"`
				IP     string `json:"ip"`
			} `json:"cameras"`
			FreshnessTS string `json:"freshness_ts"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
			t.Fatalf("[%s] json decode failed: %v", route, err)
		}
		if len(payload.Cameras) != 2 {
			t.Fatalf("[%s] expected 2 cameras, got %d", route, len(payload.Cameras))
		}
		if payload.Cameras[0].IP != "192.168.1.155" {
			t.Errorf("[%s] expected IP '192.168.1.155' from profile, got %q", route, payload.Cameras[0].IP)
		}
		if payload.Cameras[0].Online != true {
			t.Errorf("[%s] expected camera 0 online == true", route)
		}
		if payload.Cameras[1].Online != false {
			t.Errorf("[%s] expected camera 1 online == false", route)
		}

		if i == 0 {
			resp1 = rr.Body.String()
		} else {
			resp2 = rr.Body.String()
		}
	}

	if resp1 != resp2 {
		t.Errorf("aliased routes returned divergent payloads:\nRoute1: %s\nRoute2: %s", resp1, resp2)
	}
	_ = streamMgr
}

func TestOnboarding_CameraEnrollment_Persistence_And_RestartRecovery(t *testing.T) {
	sm, streamMgr, pm, _, mux, profilePath := setupFullTestEnvironment(t)

	mockServer, mockURL := setupMockCloudServer(t, nil, nil)
	defer mockServer.Close()
	sm.Cloud().GlobalBase = mockURL

	// Pre-seed profile without enrolled cameras
	prof := &profile.Profile{
		Version:   profile.CurrentSchemaVersion,
		CreatedAt: time.Now().UTC(),
		Privacy:   profile.PrivacySettings{BlockCloudVideo: profile.BoolPtr(true)},
		Credentials: profile.CloudCredentials{
			AccountEmail:   "persist@example.com",
			Password:       profile.SecretString("SecretPass123"),
			VendorUID:      "mock-uid-cached",
			AuthToken:      profile.SecretString("mock-token-cached"),
			TokenExpiresAt: time.Now().UTC().Add(24 * time.Hour),
			Region:         mockURL,
		},
		Cameras: make(map[string]profile.CameraProfile),
	}
	_ = pm.Save(context.Background(), prof)

	mockDevs := []bridge.Device{
		{UUID: "cam-uuid-101", Name: "Driveway", Type: "WS03", Online: 1},
		{UUID: "cam-uuid-102", Name: "Living Room", Type: "WS03", Online: 1},
	}
	sm.SetStateForTest(SessionStatusAuthenticated, "persist@example.com", "mock-uid-cached", "", mockDevs)

	// 1. Enroll cam-uuid-101 via /api/v1/onboarding/cameras/enroll
	enrollBody := strings.NewReader(`{"camera_ids": ["cam-uuid-101"]}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/onboarding/cameras/enroll", enrollBody)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d. Body: %s", rr.Code, rr.Body.String())
	}

	// Verify StreamManager has the camera
	if _, ok := streamMgr.GetCamera("cam-uuid-101"); !ok {
		t.Fatal("expected camera 'cam-uuid-101' to be enrolled in StreamManager")
	}

	// Verify ProfileManager has the camera persisted to encrypted disk
	savedProf, err := pm.Load(context.Background())
	if err != nil {
		t.Fatalf("failed to reload profile: %v", err)
	}
	if _, ok := savedProf.Cameras["cam-uuid-101"]; !ok {
		t.Fatal("expected camera 'cam-uuid-101' to be saved into ProfileManager on disk")
	}

	// 2. Simulate Daemon Restart
	// Close current managers
	streamMgr.CloseAll()

	// Create fresh managers from the same profile on disk
	freshPM, err := profile.NewDefaultManager(profilePath, "test-master-key-0123456789abcdef0123456789abcdef", "")
	if err != nil {
		t.Fatalf("failed to create fresh profile manager: %v", err)
	}
	freshSM := NewSessionManager("1", "test-phone-code", testServerKey)
	freshSM.Cloud().GlobalBase = mockURL
	freshStreamMgr := NewStreamManager(freshSM.Cloud(), "rtsp://127.0.0.1:8554", "http://127.0.0.1:8888", nil, false, "ffmpeg")
	freshStreamMgr.SetRunner(func(ctx context.Context, mc *ManagedCamera, rtpPort int) {
		<-ctx.Done()
	})
	// A restart rebuilds the manager, so it needs its own login pool.
	ensureTestRegistry(freshStreamMgr, freshSM, freshSM.Country(), freshSM.PhoneCode(), testServerKey)

	// Restore gateway state from disk profile
	diskProf, err := freshPM.Load(context.Background())
	if err != nil {
		t.Fatalf("failed to load disk profile on restart: %v", err)
	}
	restoreGatewayState(context.Background(), freshSM, freshStreamMgr, freshPM, diskProf)

	// 3. Verify camera was automatically restored and active in the fresh StreamManager
	restoredCam, ok := freshStreamMgr.GetCamera("cam-uuid-101")
	if !ok {
		t.Fatal("DYNAMIC ENROLLMENT FAILED RESTAR RECOVERY: camera was NOT restored after simulated restart")
	}
	if restoredCam.Name != "Driveway" {
		t.Errorf("expected restored camera name 'Driveway', got %q", restoredCam.Name)
	}
}

func TestOnboarding_Stream_DynamicICELanResolution_Persistence(t *testing.T) {
	_, streamMgr, pm, _, mux, _ := setupFullTestEnvironment(t)

	// Pre-seed profile
	prof := &profile.Profile{
		Version:   profile.CurrentSchemaVersion,
		CreatedAt: time.Now().UTC(),
		Privacy:   profile.PrivacySettings{BlockCloudVideo: profile.BoolPtr(true)},
		Cameras: map[string]profile.CameraProfile{
			"cam-ice-01": {UUID: "cam-ice-01", Name: "ICE Cam", Online: true},
		},
	}
	_ = pm.Save(context.Background(), prof)

	dev := bridge.Device{UUID: "cam-ice-01", Name: "ICE Cam", Type: "WS03", Online: 1}
	_, _ = streamMgr.Enroll([]string{"cam-ice-01"}, []bridge.Device{dev})

	mc, ok := streamMgr.GetCamera("cam-ice-01")
	if !ok {
		t.Fatal("expected camera enrolled")
	}

	// Verify initial IP is empty
	if mc.IP != "" {
		t.Fatalf("expected initial IP to be empty, got %q", mc.IP)
	}

	// Dynamically update IP (simulating v.OnCandidateHostIP)
	discoveredIP := "192.168.1.188"
	mc.mu.Lock()
	mc.IP = discoveredIP
	mc.mu.Unlock()

	// Persist to profile
	_, err := pm.Update(context.Background(), func(p *profile.Profile) error {
		if cam, found := p.Cameras["cam-ice-01"]; found {
			cam.IPAddress = discoveredIP
			p.Cameras["cam-ice-01"] = cam
		}
		return nil
	})
	if err != nil {
		t.Fatalf("failed to update profile with discovered IP: %v", err)
	}

	// Check stream descriptors endpoint returns updated descriptors
	req := httptest.NewRequest(http.MethodGet, "/api/v1/cameras/cam-ice-01/stream", nil)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d", rr.Code)
	}

	// Verify profile disk state contains discovered IP
	reloaded, _ := pm.Load(context.Background())
	if reloaded.Cameras["cam-ice-01"].IPAddress != discoveredIP {
		t.Errorf("expected learned IP %q in ProfileManager, got %q", discoveredIP, reloaded.Cameras["cam-ice-01"].IPAddress)
	}
}

func TestCamera_DedicatedPTZ(t *testing.T) {
	_, streamMgr, _, mockCC, mux, _ := setupFullTestEnvironment(t)

	// Enroll camera
	dev := bridge.Device{UUID: "ptz-cam-01", Name: "PTZ Front", Type: "WS03", Online: 1}
	_, _ = streamMgr.Enroll([]string{"ptz-cam-01"}, []bridge.Device{dev})

	// 1. Numeric direction (1=left)
	body := strings.NewReader(`{"direction": 1, "duration_ms": 300}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/cameras/ptz-cam-01/ptz", body)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d. Body: %s", rr.Code, rr.Body.String())
	}
	if mockCC.ptzDir != 1 || mockCC.ptzDur != 300 {
		t.Errorf("expected mockCC dir=1, dur=300; got dir=%d, dur=%d", mockCC.ptzDir, mockCC.ptzDur)
	}

	// 2. String direction ("up" -> 3)
	body = strings.NewReader(`{"direction": "up", "duration_ms": 500}`)
	req = httptest.NewRequest(http.MethodPost, "/api/v1/cameras/ptz-cam-01/ptz", body)
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d. Body: %s", rr.Code, rr.Body.String())
	}
	if mockCC.ptzDir != 3 || mockCC.ptzDur != 500 {
		t.Errorf("expected mockCC dir=3, dur=500; got dir=%d, dur=%d", mockCC.ptzDir, mockCC.ptzDur)
	}

	// 3. String numeric ("2" -> 2 right) with default duration
	body = strings.NewReader(`{"direction": "2"}`)
	req = httptest.NewRequest(http.MethodPost, "/api/v1/cameras/ptz-cam-01/ptz", body)
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d. Body: %s", rr.Code, rr.Body.String())
	}
	if mockCC.ptzDir != 2 || mockCC.ptzDur != 400 {
		t.Errorf("expected mockCC dir=2, dur=400; got dir=%d, dur=%d", mockCC.ptzDir, mockCC.ptzDur)
	}

	// 4. Invalid direction
	body = strings.NewReader(`{"direction": 99}`)
	req = httptest.NewRequest(http.MethodPost, "/api/v1/cameras/ptz-cam-01/ptz", body)
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 Bad Request for invalid direction, got %d", rr.Code)
	}

	// 5. Unknown camera -> 404
	body = strings.NewReader(`{"direction": 1}`)
	req = httptest.NewRequest(http.MethodPost, "/api/v1/cameras/unknown-cam/ptz", body)
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("expected 404 Not Found for unknown camera, got %d", rr.Code)
	}
}

func TestCamera_StreamDescriptors(t *testing.T) {
	_, streamMgr, _, _, mux, _ := setupFullTestEnvironment(t)

	// Enroll camera
	dev := bridge.Device{UUID: "stream-cam-01", Name: "Living Room Cam", Type: "WS03", Online: 1}
	_, _ = streamMgr.Enroll([]string{"stream-cam-01"}, []bridge.Device{dev})

	// 1. Valid camera stream descriptor
	req := httptest.NewRequest(http.MethodGet, "/api/v1/cameras/stream-cam-01/stream", nil)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d", rr.Code)
	}

	var desc struct {
		ID               string `json:"id"`
		Name             string `json:"name"`
		Streaming        bool   `json:"streaming"`
		RtspURL          string `json:"rtsp_url"`
		HlsURL           string `json:"hls_url"`
		VideoCodec       string `json:"video_codec"`
		AudioCodec       string `json:"audio_codec"`
		PublicationReady bool   `json:"publication_ready"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &desc); err != nil {
		t.Fatalf("json decode failed: %v", err)
	}

	if desc.ID != "stream-cam-01" {
		t.Errorf("expected ID 'stream-cam-01', got %s", desc.ID)
	}
	if desc.Name != "Living Room Cam" {
		t.Errorf("expected Name 'Living Room Cam', got %s", desc.Name)
	}
	if !strings.HasPrefix(desc.RtspURL, "rtsp://") {
		t.Errorf("expected valid RTSP URL, got %s", desc.RtspURL)
	}
	if !strings.HasSuffix(desc.HlsURL, ".m3u8") {
		t.Errorf("expected valid HLS URL ending with .m3u8, got %s", desc.HlsURL)
	}
	if desc.VideoCodec != "h264" || desc.AudioCodec != "aac" {
		t.Errorf("expected codecs h264/aac, got %s/%s", desc.VideoCodec, desc.AudioCodec)
	}

	// 2. Unknown camera -> 404
	req = httptest.NewRequest(http.MethodGet, "/api/v1/cameras/nonexistent-cam/stream", nil)
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("expected 404 Not Found, got %d", rr.Code)
	}
}

// TestOnboarding_Setup_CountryAndTimezoneAutoDetection verifies that when country and timezone
// are omitted from POST /api/v1/onboarding/setup, they are safely auto-detected/defaulted by
// the backend, persisting valid country and timezone info in the encrypted profile without failing.
// It also verifies that an explicitly provided country ("44") and timezone are preserved.
func TestOnboarding_Setup_CountryAndTimezoneAutoDetection(t *testing.T) {
	sm, _, pm, _, mux, _ := setupFullTestEnvironment(t)

	mockCloud, mockURL := setupMockCloudServer(t, nil, nil)
	defer mockCloud.Close()
	sm.Cloud().GlobalBase = mockURL

	// 1. Omit country, timezone, timezone_offset
	setupPayload := map[string]any{
		"account_email": "autodetect@example.com",
		"password":      "SecretPass123!",
		"force":         true,
	}
	payloadBytes, _ := json.Marshal(setupPayload)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/onboarding/setup", bytes.NewReader(payloadBytes))
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 OK for setup with omitted country/tz, got %d (body: %s)", rr.Code, rr.Body.String())
	}

	savedProf, err := pm.Load(context.Background())
	if err != nil {
		t.Fatalf("failed to load saved profile: %v", err)
	}

	if a, ok := savedProf.FindAccount("autodetect@example.com"); !ok || a.Country == "" {
		t.Fatal("expected non-empty auto-detected/default country, got empty string")
	}
	if savedProf.Connection.Timezone == "" {
		t.Fatal("expected non-empty auto-detected/default timezone, got empty string")
	}

	// 2. Explicit country and timezone must NOT be clobbered
	explicitPayload := map[string]any{
		"account_email":   "explicit@example.com",
		"password":        "SecretPass123!",
		"country":         "44",
		"timezone":        "Europe/London",
		"timezone_offset": 1.0,
		"force":           true,
	}
	explicitBytes, _ := json.Marshal(explicitPayload)

	req2 := httptest.NewRequest(http.MethodPost, "/api/v1/onboarding/setup", bytes.NewReader(explicitBytes))
	rr2 := httptest.NewRecorder()
	mux.ServeHTTP(rr2, req2)

	if rr2.Code != http.StatusOK {
		t.Fatalf("expected 200 OK for setup with explicit country/tz, got %d (body: %s)", rr2.Code, rr2.Body.String())
	}

	savedProf2, err := pm.Load(context.Background())
	if err != nil {
		t.Fatalf("failed to load saved profile: %v", err)
	}
	if a, _ := savedProf2.FindAccount("explicit@example.com"); a.Country != "44" {
		t.Fatalf("expected explicit country '44' to be preserved, got %q", a.Country)
	}
	if len(savedProf2.Accounts) != 2 {
		t.Fatalf("a second quick start adds a login and keeps the first, got %d logins", len(savedProf2.Accounts))
	}
	if savedProf2.Connection.Timezone != "Europe/London" {
		t.Fatalf("expected explicit timezone 'Europe/London' to be preserved, got %q", savedProf2.Connection.Timezone)
	}
	if savedProf2.Connection.TimezoneOffset != 1.0 {
		t.Fatalf("expected explicit timezone_offset 1.0 to be preserved, got %f", savedProf2.Connection.TimezoneOffset)
	}
}
