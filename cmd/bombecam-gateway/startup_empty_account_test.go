package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Fever-r/BombeCam/pkg/bridge"
	"github.com/Fever-r/BombeCam/pkg/profile"
)

// After a camera is reset and paired again, the Osaio account can list no
// cameras ("data": null). BombeCam must start, keep the saved camera and say
// why it can't stream.
func TestGateway_StartsWhenAccountListsNoCameras(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	profilePath := filepath.Join(dir, "profile.enc")
	key := []byte("01234567890123456789012345678901")

	var serverURL string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v2/account/get-baseurl":
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 1000, "data": map[string]any{"region": "US", "web": serverURL, "ws": "wss://mock.osaio.net/ws"}})
		case "/v2/device/list":
			_, _ = w.Write([]byte(`{"code":1000,"data":null,"msg":"success"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	serverURL = srv.URL

	pm := profile.NewManager(profilePath, key)
	if err := pm.Save(ctx, &profile.Profile{
		Version: 1, CreatedAt: time.Now().UTC(),
		Credentials: profile.CloudCredentials{
			AccountEmail: "owner@example.com", Password: profile.SecretString("pw"),
			VendorUID: "uid", AuthToken: profile.SecretString("tok"),
			TokenExpiresAt: time.Now().UTC().Add(time.Hour), Region: serverURL, Country: "1",
		},
		Cameras: map[string]profile.CameraProfile{
			"e56": {UUID: "e56", Name: "Porch Cam", Model: "WS03", EnrolledAt: time.Now().UTC()},
		},
		Connection: profile.ConnectionParameters{Timezone: "UTC"},
	}); err != nil {
		t.Fatal(err)
	}
	*profilePathFlag = profilePath
	*profileKeyFlag = string(key)
	*profileKeyFileFlag = ""

	sm := NewSessionManager("1", "phone", testServerKey)
	sm.Cloud().Web = serverURL
	sm.Cloud().GlobalBase = serverURL
	stm := NewStreamManager(sm.Cloud(), "rtsp://127.0.0.1:8554", "http://127.0.0.1:8888", nil, false, "ffmpeg")
	stm.SetSessionRegistry(NewSessionRegistry(sm, sm.Country(), sm.PhoneCode(), testServerKey))
	setCurrentStreamManager(stm)
	t.Cleanup(func() { setCurrentStreamManager(nil) })
	defer stm.CloseAll()

	runGatewayStartupSafely(sm, stm, "", "", "", "", "", "127.0.0.1:8654")

	if login, ok := currentStreamManager().Registry().Get("owner@example.com"); !ok || !login.IsAuthenticated() {
		t.Fatalf("login not restored: %+v", currentStreamManager().Registry().Status())
	}
	if _, ok := stm.GetCamera("e56"); !ok {
		t.Fatal("the saved camera must stay enrolled so it streams again once it is back on the account")
	}
}

func TestMissingFromAccount(t *testing.T) {
	prof := &profile.Profile{
		Credentials: profile.CloudCredentials{AccountEmail: "Owner@Example.com"},
		Cameras: map[string]profile.CameraProfile{
			"e56": {UUID: "e56", Name: "Porch Cam"},
			"a01": {UUID: "a01", Name: "Porch"},
			"b02": {UUID: "b02", Name: "Shed", AccountEmail: "other@example.com"},
			"c03": {UUID: "c03", Name: "Garage", AccountEmail: "owner@example.com"},
		},
	}
	prof.Normalize() // cameras saved without a login belong to the old single login
	got := missingFromAccount(prof, "owner@example.com", []bridge.Device{{UUID: "a01"}})
	if len(got) != 2 {
		t.Fatalf("want Garage and Porch Cam reported, got %q", got)
	}
	if !strings.HasPrefix(got[0], "Garage is not on the Osaio account owner@example.com") ||
		!strings.HasPrefix(got[1], "Porch Cam is not on") || !strings.Contains(got[1], "pair it to this account") {
		t.Fatalf("unexpected messages: %q", got)
	}
	if got := missingFromAccount(prof, "owner@example.com", []bridge.Device{{UUID: "a01"}, {UUID: "e56"}, {UUID: "c03"}}); len(got) != 0 {
		t.Fatalf("nothing missing, got %q", got)
	}
	if got := missingFromAccount(prof, "other@example.com", nil); len(got) != 1 || !strings.HasPrefix(got[0], "Shed is not on the Osaio account other@example.com") {
		t.Fatalf("each login reports its own cameras: %q", got)
	}
}

// A panic during startup must not close BombeCam: it is logged and shown.
func TestGuardStartup_RecoversFromPanic(t *testing.T) {
	sm := NewSessionManager("1", "phone", testServerKey)
	guardStartup(sm, func() {
		var m map[string]any
		_ = m["data"].(map[string]any) // a panic during startup
	})
	st := sm.GetStatus()
	if st.Status != SessionStatusUnavailable || !strings.Contains(st.Error, "gateway.log") {
		t.Fatalf("status after a startup panic = %+v", st)
	}
}
