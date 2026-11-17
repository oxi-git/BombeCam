package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Fever-r/BombeCam/pkg/auth"
	"github.com/Fever-r/BombeCam/pkg/bridge"
	"github.com/Fever-r/BombeCam/pkg/profile"
)

func securityServer(t *testing.T) (*SessionManager, *StreamManager, profile.ProfileManager, *http.Client, string) {
	t.Helper()
	previousOM, previousPM, previousCC := GatewayOperatorManager(), ActiveProfileManager(), GatewayControlChannel()
	SetGatewayOperatorManager(nil)
	sm, stm, pm, _, _, _ := setupFullTestEnvironment(t)
	om := auth.NewOperatorManager(auth.OperatorConfig{})
	SetGatewayOperatorManager(om)
	stm.SetProfileManager(pm)
	mux := SetupAPIMux(sm, stm)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Exercise the LAN authorization branch without accessing the user's LAN.
		r.RemoteAddr = "192.0.2.55:12345"
		SecurityBoundaryHandler(mux).ServeHTTP(w, r)
	}))
	t.Cleanup(func() {
		server.Close()
		stm.StopStreaming()
		SetGatewayOperatorManager(previousOM)
		SetActiveProfileManager(previousPM)
		SetGatewayControlChannel(previousCC)
	})
	return sm, stm, pm, server.Client(), server.URL
}

func securityRequest(t *testing.T, client *http.Client, method, target, body string, headers map[string]string) (*http.Response, []byte) {
	t.Helper()
	req, _ := http.NewRequest(method, target, strings.NewReader(body))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	return resp, data
}

func securityVisitor(t *testing.T, client *http.Client, base string) map[string]string {
	t.Helper()
	resp, data := securityRequest(t, client, "GET", base+"/api/v1/auth/csrf", "", nil)
	var csrf struct {
		Token string `json:"csrf_token"`
	}
	if resp.StatusCode != 200 || json.Unmarshal(data, &csrf) != nil || csrf.Token == "" {
		t.Fatal("visitor bootstrap failed")
	}
	cookies := resp.Cookies()
	if len(cookies) != 1 {
		t.Fatal("missing visitor cookie")
	}
	sess, ok := GatewayOperatorManager().VerifySession(cookies[0].Value)
	if !ok || sess.IsOperator {
		t.Fatal("LAN visitor was elevated")
	}
	return map[string]string{"Cookie": cookies[0].String(), "X-CSRF-Token": csrf.Token}
}

func TestSecurity_LANMutationsRequireOperator(t *testing.T) {
	_, stm, _, client, base := securityServer(t)
	_, err := stm.Enroll([]string{"security-camera"}, []bridge.Device{{UUID: "security-camera", Name: "Synthetic camera", Type: "WS03", Online: 1}})
	if err != nil {
		t.Fatal(err)
	}
	visitor := securityVisitor(t, client, base)
	routes := []struct{ method, path, body string }{
		{"POST", "/api/v1/cameras/security-camera/control", `{"action":"led","value":"off"}`},
		{"POST", "/api/v1/cameras/security-camera/talk/start", "{}"},
		{"POST", "/api/v1/cameras/security-camera/talk/stop", "{}"},
		{"POST", "/api/v1/cameras/security-camera/ptz", "{}"},
		{"POST", "/api/v1/cameras/security-camera/stream-name", "{}"},
		{"DELETE", "/api/v1/cameras/security-camera", ""},
		{"POST", "/ir?cam=security-camera&mode=off", ""},
		{"POST", "/led?cam=security-camera&on=0", ""},
		{"POST", "/light?cam=security-camera&on=0", ""},
		{"POST", "/ptz?cam=security-camera&dir=left", ""},
		{"POST", "/talk/start?cam=security-camera", ""},
		{"POST", "/talk/stop?cam=security-camera", ""},
		{"POST", "/api/v1/gateway/start", "{}"},
		{"POST", "/api/v1/gateway/stop", "{}"},
		{"POST", "/api/v1/onboarding/reset", "{}"},
		{"DELETE", "/api/v1/onboarding/profile", ""},
		{"POST", "/api/v1/cameras/search", "{}"},
		{"POST", "/api/v1/cameras/add", "{}"},
		{"POST", "/api/v1/onboarding/cameras/enroll", "{}"},
	}
	for _, route := range routes {
		t.Run(route.path, func(t *testing.T) {
			for _, headers := range []map[string]string{nil, visitor} {
				resp, data := securityRequest(t, client, route.method, base+route.path, route.body, headers)
				if resp.StatusCode != 401 {
					t.Fatalf("wanted 401, got %d: %s", resp.StatusCode, data)
				}
			}
		})
	}
	if _, ok := stm.GetCamera("security-camera"); !ok {
		t.Fatal("denied removal changed camera inventory")
	}
	token, _ := GatewayOperatorManager().GenerateAPIToken()
	resp, data := securityRequest(t, client, "POST", base+"/api/v1/cameras/security-camera/control", `{"action":"led","value":"off"}`, map[string]string{"Authorization": "Bearer " + token})
	if resp.StatusCode != 200 {
		t.Fatalf("operator cannot control: %d %s", resp.StatusCode, data)
	}
	resp, data = securityRequest(t, client, "POST", base+"/ir?cam=security-camera&mode=off", "", map[string]string{"Authorization": "Bearer " + token})
	if resp.StatusCode != 200 {
		t.Fatalf("operator cannot use legacy control: %d %s", resp.StatusCode, data)
	}
	operator, err := GatewayOperatorManager().CreateSession()
	if err != nil {
		t.Fatal(err)
	}
	resp, _ = securityRequest(t, client, "POST", base+"/api/v1/cameras/security-camera/control", `{"action":"led","value":"off"}`, map[string]string{"Cookie": auth.SessionCookieName + "=" + operator.Token})
	if resp.StatusCode != 403 {
		t.Fatal("operator cookie bypassed CSRF")
	}
	// Resetting an empty profile must not grant authority to a visitor.
	resp, _ = securityRequest(t, client, "POST", base+"/api/v1/operator/token", "{}", visitor)
	if resp.StatusCode != 401 {
		t.Fatal("visitor acquired an API token")
	}
}

func TestSecurity_StreamSecretsRequireOperator(t *testing.T) {
	_, stm, pm, client, base := securityServer(t)
	p := &profile.Profile{Credentials: profile.CloudCredentials{AccountEmail: "security@example.test"}, Integrations: profile.IntegrationSettings{StreamAuth: true, StreamUser: "security", StreamPassword: "synthetic-stream-secret"}}
	if err := pm.Save(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	old := currentIntegrationSettings()
	storeIntegrationSettings(p.Integrations)
	stm.SetStreamCredentials(p.Integrations.StreamUser, p.Integrations.StreamPassword.Expose())
	t.Cleanup(func() { storeIntegrationSettings(old) })
	_, _ = stm.Enroll([]string{"security-camera"}, []bridge.Device{{UUID: "security-camera", Name: "Synthetic camera", Type: "WS03", Online: 1}})
	visitor := securityVisitor(t, client, base)
	for _, path := range []string{"/api/v1/integrations/settings", "/api/v1/integrations/streams", "/api/v1/integrations/frigate?format=yaml", "/api/v1/integrations/homeassistant", "/api/v1/cameras/security-camera/stream"} {
		for _, headers := range []map[string]string{nil, visitor} {
			resp, data := securityRequest(t, client, "GET", base+path, "", headers)
			if resp.StatusCode != 401 || bytes.Contains(data, []byte("synthetic-stream-secret")) {
				t.Fatalf("secret exposed at %s: %d %s", path, resp.StatusCode, data)
			}
		}
	}
	token, _ := GatewayOperatorManager().GenerateAPIToken()
	resp, data := securityRequest(t, client, "GET", base+"/api/v1/integrations/settings", "", map[string]string{"Authorization": "Bearer " + token})
	if resp.StatusCode != 200 || !bytes.Contains(data, []byte("synthetic-stream-secret")) {
		t.Fatal("operator cannot retrieve stream credentials")
	}
}

func TestSecurity_SetupSavedCredentialsAndPreservation(t *testing.T) {
	sm, _, pm, client, base := securityServer(t)
	cloud, url := setupMockCloudServer(t, nil, nil)
	defer cloud.Close()
	sm.Cloud().GlobalBase = url
	created := time.Now().UTC().Add(-time.Hour)
	p := &profile.Profile{CreatedAt: created, Credentials: profile.CloudCredentials{AccountEmail: "security@example.test", Country: "44", Password: "synthetic-cloud-secret"}, Integrations: profile.IntegrationSettings{StreamAuth: true, StreamUser: "security", StreamPassword: "synthetic-stream-secret", RTSPPort: 18554, NVRAddress: "192.0.2.20", MQTT: profile.MQTTSettings{Host: "broker.example.test", Password: "synthetic-mqtt-secret"}}, Privacy: profile.PrivacySettings{Blocked: map[string]bool{"security-camera": true}}, RouterKey: "synthetic-router-key", Cameras: map[string]profile.CameraProfile{"security-camera": {UUID: "security-camera"}}}
	if err := pm.Save(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	visitor := securityVisitor(t, client, base)
	// From another device, knowing an Osaio login's email or password never
	// grants access to BombeCam: only the administrator's sign-in does.
	for _, body := range []string{`{"account_email":"security@example.test","force":true}`, `{"account_email":"stranger@example.test","password":"other-secret","force":true}`, `{"account_email":"security@example.test","password":"synthetic-cloud-secret","force":true}`} {
		for _, headers := range []map[string]string{nil, visitor} {
			resp, data := securityRequest(t, client, "POST", base+"/api/v1/onboarding/setup", body, headers)
			if resp.StatusCode != 401 || len(resp.Cookies()) != 0 {
				t.Fatalf("setup takeover: %d %s", resp.StatusCode, data)
			}
		}
	}
	token, _ := GatewayOperatorManager().GenerateAPIToken()
	for _, entry := range []struct {
		body    string
		headers map[string]string
	}{
		{`{"account_email":"security@example.test","force":true}`, map[string]string{"Authorization": "Bearer " + token}},
		{`{"account_email":"new-account@example.test","password":"new-secret","country":"44","force":true}`, map[string]string{"Authorization": "Bearer " + token}},
	} {
		resp, data := securityRequest(t, client, "POST", base+"/api/v1/onboarding/setup", entry.body, entry.headers)
		if resp.StatusCode != 200 {
			t.Fatalf("authorized setup failed: %d %s", resp.StatusCode, data)
		}
		after := pm.GetProfile()
		if !reflect.DeepEqual(after.Integrations, p.Integrations) || !reflect.DeepEqual(after.Privacy, p.Privacy) || after.RouterKey != p.RouterKey || !after.CreatedAt.Equal(created) || len(after.Cameras) != 1 {
			t.Fatalf("re-login dropped unrelated settings")
		}
	}
	if _, ok := pm.GetProfile().FindAccount("security@example.test"); !ok {
		t.Fatal("switching lost the previous account")
	}
}

func TestSecurity_SetupDoesNotOverwriteUnreadableProfile(t *testing.T) {
	sm, stm, _, _, _ := securityServer(t)
	file := filepath.Join(t.TempDir(), "corrupt.enc")
	corruptPM := profile.NewManager(file, []byte(strings.Repeat("k", 32)))
	if err := os.WriteFile(file, []byte("invalid encrypted profile"), 0600); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest("POST", "/api/v1/onboarding/setup", strings.NewReader(`{"account_email":"security@example.test","password":"supplied-secret","force":true}`))
	request.RemoteAddr = "127.0.0.1:12345" // this PC: no sign-in needed, so the profile check is reached
	response := httptest.NewRecorder()
	stm.SetProfileManager(corruptPM)
	handleOnboardingSetup(response, request, sm, stm)
	if response.Code != 503 {
		t.Fatalf("want 503, got %d: %s", response.Code, response.Body.String())
	}
	after, err := os.ReadFile(file)
	if err != nil || string(after) != "invalid encrypted profile" {
		t.Fatal("unreadable profile was replaced")
	}
}

func TestSecurity_SetupPreservesConcurrentSettings(t *testing.T) {
	sm, _, pm, client, base := securityServer(t)
	original := &profile.Profile{Credentials: profile.CloudCredentials{AccountEmail: "security@example.test", Country: "44", Password: "synthetic-password"}, Integrations: profile.IntegrationSettings{NVRAddress: "192.0.2.20"}}
	if err := pm.Save(context.Background(), original); err != nil {
		t.Fatal(err)
	}
	var vendorURL string
	vendor := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v2/account/get-baseurl":
			json.NewEncoder(w).Encode(map[string]any{"code": 1000, "data": map[string]any{"web": vendorURL}})
		case "/v2/login/login":
			_, err := pm.Update(r.Context(), func(p *profile.Profile) error {
				p.Integrations.NVRAddress = "192.0.2.21"
				p.Integrations.StreamAuth = true
				p.Integrations.StreamUser = "synthetic"
				p.Integrations.StreamPassword = "concurrent-secret"
				return nil
			})
			if err != nil {
				t.Error(err)
			}
			json.NewEncoder(w).Encode(map[string]any{"code": 1000, "data": map[string]any{"uid": "synthetic-uid", "api_token": "synthetic-token"}})
		case "/v2/device/list":
			json.NewEncoder(w).Encode(map[string]any{"code": 1000, "data": map[string]any{"data": []any{}}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer vendor.Close()
	vendorURL = vendor.URL
	sm.Cloud().GlobalBase = vendorURL
	token, _ := GatewayOperatorManager().GenerateAPIToken()
	resp, data := securityRequest(t, client, "POST", base+"/api/v1/onboarding/setup", `{"account_email":"security@example.test","force":true}`, map[string]string{"Authorization": "Bearer " + token})
	if resp.StatusCode != 200 {
		t.Fatalf("setup failed: %d %s", resp.StatusCode, data)
	}
	after := pm.GetProfile().Integrations
	if after.NVRAddress != "192.0.2.21" || !after.StreamAuth || after.StreamPassword.Expose() != "concurrent-secret" {
		t.Fatal("setup overwrote settings changed during vendor login")
	}
}

func TestSecurity_TokenCannotAuthenticateAnUnrelatedVendorAccount(t *testing.T) {
	sm, _, pm, client, base := securityServer(t)
	if err := pm.Save(context.Background(), &profile.Profile{Credentials: profile.CloudCredentials{AccountEmail: "owner@example.test", Password: "owner-secret"}}); err != nil {
		t.Fatal(err)
	}
	calls := make(chan struct{}, 8)
	vendor := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls <- struct{}{}
		http.Error(w, "unexpected vendor request", 500)
	}))
	defer vendor.Close()
	sm.Cloud().GlobalBase = vendor.URL
	resp, data := securityRequest(t, client, "POST", base+"/api/v1/operator/token", `{"account_email":"stranger@example.test","password":"other-secret"}`, nil)
	if resp.StatusCode != 401 {
		t.Fatalf("unrelated vendor account granted operator token: %d %s", resp.StatusCode, data)
	}
	if len(calls) != 0 {
		t.Fatal("token route attempted an unrelated vendor login")
	}
}

func TestSecurity_SetupInvalidPasswordPreservesProfile(t *testing.T) {
	sm, _, pm, client, base := securityServer(t)
	original := &profile.Profile{Credentials: profile.CloudCredentials{AccountEmail: "security@example.test", Country: "44", Password: "original-secret"}, Integrations: profile.IntegrationSettings{StreamAuth: true, StreamPassword: "original-stream-secret"}}
	if err := pm.Save(context.Background(), original); err != nil {
		t.Fatal(err)
	}
	var vendorURL string
	vendor := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v2/account/get-baseurl" {
			json.NewEncoder(w).Encode(map[string]any{"code": 1000, "data": map[string]any{"web": vendorURL}})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"code": 2001, "msg": "invalid password"})
	}))
	defer vendor.Close()
	vendorURL = vendor.URL
	sm.Cloud().GlobalBase = vendorURL
	resp, data := securityRequest(t, client, "POST", base+"/api/v1/onboarding/setup", `{"account_email":"security@example.test","password":"invalid-secret","force":true}`, nil)
	if resp.StatusCode != 401 || len(resp.Cookies()) != 0 {
		t.Fatalf("invalid password granted session: %d %s", resp.StatusCode, data)
	}
	after := pm.GetProfile()
	if len(after.Accounts) != 1 || after.Accounts[0].Password != original.Credentials.Password || !reflect.DeepEqual(after.Integrations, original.Integrations) {
		t.Fatal("invalid login changed the saved profile")
	}
}
