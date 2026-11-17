package main

import (
	"context"
	"crypto/md5"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Fever-r/BombeCam/pkg/profile"
)

// fakeOsaioPool stands in for Osaio with several logins. Each login has one
// password and its own cameras; the uid in a signed request names the login.
func fakeOsaioPool(t *testing.T, passwords map[string]string, cameras map[string][]string) *httptest.Server {
	t.Helper()
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v2/account/get-baseurl":
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 1000, "data": map[string]any{"web": srv.URL}})
		case "/v2/login/login":
			var body struct {
				Account  string `json:"account"`
				Password string `json:"password"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			want, ok := passwords[body.Account]
			if !ok || fmt.Sprintf("%x", md5.Sum([]byte(want))) != body.Password {
				_ = json.NewEncoder(w).Encode(map[string]any{"code": 2001, "msg": "wrong password"})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 1000, "data": map[string]any{"uid": body.Account, "api_token": "tok-" + body.Account}})
		case "/v2/device/list":
			devs := []map[string]any{}
			for _, id := range cameras[r.Header.Get("uid")] {
				devs = append(devs, map[string]any{"uuid": id, "name": "Cam " + id, "type": "WS03", "model_type": 1, "online": 1})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 1000, "data": map[string]any{"data": devs}})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func postJSON(t *testing.T, mux http.Handler, path, body string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, path, strings.NewReader(body)))
	var out map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &out)
	return rr, out
}

func listAccounts(t *testing.T, mux http.Handler) map[string]accountView {
	t.Helper()
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/v1/accounts", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("list accounts: %d %s", rr.Code, rr.Body.String())
	}
	var resp struct {
		Accounts []accountView `json:"accounts"`
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &resp)
	out := map[string]accountView{}
	for _, a := range resp.Accounts {
		out[a.Email] = a
	}
	return out
}

// setupAccountPool signs in a@ and b@ and adds one camera from each.
func setupAccountPool(t *testing.T) (*SessionManager, *StreamManager, profile.ProfileManager, http.Handler, map[string]string) {
	t.Helper()
	unknownCountryEnvironment(t)
	sm, stm, pm, _, mux, _ := setupFullTestEnvironment(t)
	passwords := map[string]string{"a@example.test": "pw-a", "b@example.test": "pw-b"}
	srv := fakeOsaioPool(t, passwords, map[string][]string{"a@example.test": {"cam-a"}, "b@example.test": {"cam-b"}})
	sm.Cloud().GlobalBase = srv.URL
	ensureTestRegistry(stm, sm, "1", "fixture", testServerKey)
	t.Cleanup(func() { stm.SetSessionRegistry(nil) })

	for _, email := range []string{"a@example.test", "b@example.test"} {
		rr, _ := postJSON(t, mux, "/api/v1/accounts/signin", fmt.Sprintf(`{"email":%q,"password":%q,"country":"1"}`, email, passwords[email]))
		if rr.Code != http.StatusOK {
			t.Fatalf("sign in %s: %d %s", email, rr.Code, rr.Body.String())
		}
		rr, out := postJSON(t, mux, "/api/v1/accounts/cameras", fmt.Sprintf(`{"email":%q}`, email))
		if rr.Code != http.StatusOK {
			t.Fatalf("cameras %s: %d %s", email, rr.Code, rr.Body.String())
		}
		cams, _ := out["cameras"].([]any)
		if len(cams) != 1 {
			t.Fatalf("cameras for %s: %v", email, out)
		}
		id := cams[0].(map[string]any)["id"].(string)
		if rr, _ := postJSON(t, mux, "/api/v1/cameras/add", fmt.Sprintf(`{"email":%q,"camera_ids":[%q]}`, email, id)); rr.Code != http.StatusOK {
			t.Fatalf("add %s: %d %s", id, rr.Code, rr.Body.String())
		}
	}
	return sm, stm, pm, mux, passwords
}

func TestAccountsPoolSignInListAndCameras(t *testing.T) {
	template, stm, pm, mux, _ := setupAccountPool(t)
	reg := stm.Registry()
	a, okA := reg.Get("a@example.test")
	b, okB := reg.Get("b@example.test")
	if !okA || !okB || a == b || a == template || b == template || !a.IsAuthenticated() || !b.IsAuthenticated() {
		t.Fatal("every login needs its own signed-in session, none of them the template")
	}
	if template.IsAuthenticated() || template.Email() != "" {
		t.Fatalf("the template must never become a login: %+v", template.GetStatus())
	}
	got := listAccounts(t, mux)
	for _, email := range []string{"a@example.test", "b@example.test"} {
		a := got[email]
		if a.Status != "connected" || a.Cameras != 1 || !a.HasPassword || a.Country != "1" {
			t.Fatalf("%s: %+v", email, a)
		}
	}
	prof := pm.GetProfile()
	if prof.Cameras["cam-b"].AccountEmail != "b@example.test" || stm.CameraCount() != 2 {
		t.Fatalf("cameras: %+v (%d streaming)", prof.Cameras, stm.CameraCount())
	}
	raw, _ := json.Marshal(listAccounts(t, mux))
	if strings.Contains(string(raw), "pw-") || strings.Contains(string(raw), "tok-") {
		t.Fatalf("the login list leaked a secret: %s", raw)
	}
}

// A mistyped password leaves the working sign-in alone; the right one is saved.
func TestAccountsWrongPasswordKeepsWorkingLogin(t *testing.T) {
	_, stm, pm, mux, _ := setupAccountPool(t)
	a, _ := stm.Registry().Get("a@example.test")
	cloudA := a.Cloud()
	rr, out := postJSON(t, mux, "/api/v1/accounts/signin", `{"email":"a@example.test","password":"typo","country":"1"}`)
	if rr.Code != http.StatusUnauthorized || out["error"] != "invalid_credentials" {
		t.Fatalf("wrong password: %d %v", rr.Code, out)
	}
	if !a.IsAuthenticated() || a.Cloud() != cloudA {
		t.Fatalf("a typo must not sign the login out: %+v", a.GetStatus())
	}
	if pw, _ := pm.GetProfile().FindAccount("a@example.test"); pw.Password.Expose() != "pw-a" {
		t.Fatal("a rejected password must not be saved")
	}
	if got := listAccounts(t, mux)["a@example.test"]; got.Status != "connected" {
		t.Fatalf("status after typo: %+v", got)
	}
}

// Removing a login takes only its cameras with it.
func TestAccountsRemoveSecondaryLogin(t *testing.T) {
	_, stm, pm, mux, _ := setupAccountPool(t)
	a, _ := stm.Registry().Get("a@example.test")
	rr, out := postJSON(t, mux, "/api/v1/accounts/remove", `{"email":"b@example.test"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("remove: %d %v", rr.Code, out)
	}
	prof := pm.GetProfile()
	if _, ok := prof.FindAccount("b@example.test"); ok {
		t.Fatal("login still stored")
	}
	if _, ok := prof.Cameras["cam-b"]; ok || stm.CameraCount() != 1 {
		t.Fatalf("b's camera not removed: %+v", prof.Cameras)
	}
	if got, _ := stm.Registry().Get("a@example.test"); got != a || !a.IsAuthenticated() {
		t.Fatal("the other login's session must stay as it was")
	}
	if _, ok := prof.Cameras["cam-a"]; !ok {
		t.Fatal("the other login's camera must stay")
	}
	if _, ok := stm.Registry().Get("b@example.test"); ok {
		t.Fatal("b's session still registered")
	}
}

// Logins are equal: removing the first login added is no different from
// removing any other. The remaining login keeps its session and cloud, and
// nothing signs in again.
func TestAccountsRemoveFirstLoginLeavesOthersUntouched(t *testing.T) {
	_, stm, pm, mux, _ := setupAccountPool(t)
	b, _ := stm.Registry().Get("b@example.test")
	cloudB := b.Cloud()
	rr, out := postJSON(t, mux, "/api/v1/accounts/remove", `{"email":"a@example.test"}`)
	if rr.Code != http.StatusOK || out["logins_left"] != float64(1) {
		t.Fatalf("remove: %d %v", rr.Code, out)
	}
	if got, ok := stm.Registry().Get("b@example.test"); !ok || got != b || !b.IsAuthenticated() || b.Cloud() != cloudB {
		t.Fatalf("b's session must be untouched: %+v", b.GetStatus())
	}
	if stm.Registry().CloudFor("a@example.test") != nil {
		t.Fatal("a's session should be gone")
	}
	prof := pm.GetProfile()
	if len(prof.Accounts) != 1 || prof.Accounts[0].AccountEmail != "b@example.test" {
		t.Fatalf("profile: %s", prof)
	}
	if _, ok := prof.Cameras["cam-a"]; ok || stm.CameraCount() != 1 {
		t.Fatal("a's camera should be gone")
	}
}

// Removing the last login leaves BombeCam ready for a new one, keeping the
// rest of its settings.
func TestAccountsRemoveLastLogin(t *testing.T) {
	sm, stm, pm, mux, _ := setupAccountPool(t)
	_, _ = pm.Update(context.Background(), func(p *profile.Profile) error {
		p.Integrations.Snapshots = true
		return nil
	})
	for _, email := range []string{"a@example.test", "b@example.test"} {
		if rr, out := postJSON(t, mux, "/api/v1/accounts/remove", fmt.Sprintf(`{"email":%q}`, email)); rr.Code != http.StatusOK {
			t.Fatalf("remove %s: %d %v", email, rr.Code, out)
		}
	}
	prof := pm.GetProfile()
	if len(prof.Accounts) != 0 || len(prof.Cameras) != 0 || stm.CameraCount() != 0 {
		t.Fatalf("expected no logins or cameras: %s", prof)
	}
	if !prof.Integrations.Snapshots {
		t.Fatal("other settings must be kept")
	}
	if len(stm.Registry().Sessions()) != 0 || stm.Registry().Status().Status != SessionStatusCredentialsMissing {
		t.Fatalf("no login should be left: %+v", stm.Registry().Status())
	}
	_ = sm
	if rr, _ := postJSON(t, mux, "/api/v1/accounts/remove", `{"email":"a@example.test"}`); rr.Code != http.StatusNotFound {
		t.Fatalf("removing an unknown login: %d", rr.Code)
	}
}

func TestAccountCamerasNeedsSignedInLogin(t *testing.T) {
	_, _, _, mux, _ := setupAccountPool(t)
	if rr, _ := postJSON(t, mux, "/api/v1/accounts/cameras", `{"email":"nobody@example.test"}`); rr.Code != http.StatusConflict {
		t.Fatalf("unknown login: %d", rr.Code)
	}
}

// At start-up every saved login is restored the same way, and only the
// cameras that were added come back:
//   - a@ (first in the profile) has a password Osaio now refuses: only its
//     own camera is affected;
//   - b@ restores from its saved sign-in (its saved password is stale, so a
//     password sign-in would fail): a later login is not second-class;
//   - b@ can also see cam-b-new, which was never added: it stays out.
func TestRestoreBringsBackSavedLoginsAndOnlyAddedCameras(t *testing.T) {
	unknownCountryEnvironment(t)
	_, _, pm, _, _, _ := setupFullTestEnvironment(t)
	srv := fakeOsaioPool(t, map[string]string{"b@example.test": "pw-b"}, map[string][]string{"b@example.test": {"cam-b", "cam-b-new"}})
	prof := &profile.Profile{Version: profile.CurrentSchemaVersion, Cameras: map[string]profile.CameraProfile{
		"cam-a": {UUID: "cam-a", Name: "A", AccountEmail: "a@example.test"},
		"cam-b": {UUID: "cam-b", Name: "B", AccountEmail: "b@example.test"},
	}}
	prof.UpsertAccount(profile.CloudCredentials{AccountEmail: "a@example.test", Password: "changed-in-app", Country: "1"})
	prof.UpsertAccount(profile.CloudCredentials{AccountEmail: "b@example.test", Password: "stale", Country: "1",
		VendorUID: "b@example.test", AuthToken: "tok-b@example.test", Region: srv.URL, TokenExpiresAt: time.Now().Add(time.Hour)})
	if err := pm.Save(context.Background(), prof); err != nil {
		t.Fatal(err)
	}

	template := NewSessionManager("1", "fixture", testServerKey)
	template.Cloud().GlobalBase = srv.URL
	stm := NewStreamManager(template.Cloud(), "rtsp://127.0.0.1:8554", "http://127.0.0.1:8888", nil, false, "ffmpeg")
	stm.SetRunner(func(ctx context.Context, mc *ManagedCamera, rtpPort int) { <-ctx.Done() })
	reg := ensureTestRegistry(stm, template, "1", "fixture", testServerKey)
	defer stm.SetSessionRegistry(nil)

	restoreGatewayState(context.Background(), template, stm, pm, pm.GetProfile())

	if a, ok := reg.Get("a@example.test"); !ok || a.GetStatus().Status != SessionStatusInvalidCredentials {
		t.Fatalf("a@ should need its password: %+v", reg.Status())
	}
	if b, ok := reg.Get("b@example.test"); !ok || !b.IsAuthenticated() {
		t.Fatal("b@ should be restored from its saved sign-in")
	}
	if reg.Status().Status != SessionStatusAuthenticated {
		t.Fatalf("one working login is enough for the page: %+v", reg.Status())
	}
	if stm.CameraCount() != 2 {
		t.Fatalf("both saved cameras should be set up again, got %d", stm.CameraCount())
	}
	if _, added := stm.GetCamera("cam-b-new"); added {
		t.Fatal("a camera that was never added must not be added at start-up")
	}
	if _, saved := pm.GetProfile().Cameras["cam-b-new"]; saved {
		t.Fatal("a camera that was never added must not be saved")
	}
}

// The first sign-in page is only a quick start: it adds a login to the pool
// like any other. Using it again with another login keeps the first one and
// its cameras, and neither login is special.
func TestQuickStartAddsToThePool(t *testing.T) {
	template, stm, pm, mux, passwords := setupAccountPool(t)
	_ = passwords
	a, _ := stm.Registry().Get("a@example.test")
	rr, out := postJSON(t, mux, "/api/v1/onboarding/setup", `{"account_email":"b@example.test","password":"pw-b","country":"1","force":true}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("quick start: %d %v", rr.Code, out)
	}
	if got, _ := stm.Registry().Get("a@example.test"); got != a || !a.IsAuthenticated() {
		t.Fatal("signing another login in must not touch a@")
	}
	prof := pm.GetProfile()
	if len(prof.Accounts) != 2 || len(prof.Cameras) != 2 || stm.CameraCount() != 2 {
		t.Fatalf("logins %d, cameras %d (%d streaming)", len(prof.Accounts), len(prof.Cameras), stm.CameraCount())
	}
	if template.IsAuthenticated() {
		t.Fatal("the template must never become a login")
	}
}
