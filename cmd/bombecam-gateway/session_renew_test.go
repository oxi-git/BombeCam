package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Fever-r/BombeCam/pkg/profile"
)

// An expired Osaio sign-in is renewed with the saved login when a stream
// start fails, once, and the new token is saved; a camera that is merely
// offline does not trigger a sign-in.
func TestRenewLogin_ExpiredSignIn(t *testing.T) {
	var logins atomic.Int32
	var cameraOffline atomic.Bool
	reply := func(w http.ResponseWriter, v any) { _ = json.NewEncoder(w).Encode(v) }
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		good := r.Header.Get("api-token") == "new-token"
		switch r.URL.Path {
		case "/v2/login/login":
			logins.Add(1)
			reply(w, map[string]any{"code": 1000, "data": map[string]any{"uid": "uid-1", "api_token": "new-token"}})
		case "/v2/device/list":
			if good || cameraOffline.Load() {
				reply(w, map[string]any{"code": 1000, "data": nil})
			} else {
				reply(w, map[string]any{"code": 1003, "msg": "token invalid"})
			}
		case "/v2/webrtcsession/user/videocall":
			if good && !cameraOffline.Load() {
				reply(w, map[string]any{"code": 1000, "data": map[string]any{"session_id": "s1"}})
			} else {
				reply(w, map[string]any{"code": 1003})
			}
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	dir := t.TempDir()
	pm := profile.NewManager(filepath.Join(dir, "p.enc"), []byte("01234567890123456789012345678901"))
	SetActiveProfileManager(pm)
	defer SetActiveProfileManager(nil)
	renewMu.Lock()
	renewLast = map[string]time.Time{}
	renewMu.Unlock()

	template := NewSessionManager("1", "code", testServerKey)
	streamMgr := NewStreamManager(template.Cloud(), "rtsp://127.0.0.1:8554", "http://127.0.0.1:8888", nil, false, "")
	reg := ensureTestRegistry(streamMgr, template, "1", "code", testServerKey)
	setCurrentStreamManager(streamMgr)
	t.Cleanup(func() { setCurrentStreamManager(nil) })
	streamMgr.SetProfileManager(pm)
	template.SetStateForTest(SessionStatusAuthenticated, "me@example.com", "uid-1", "", nil)
	sm, _ := reg.Get("me@example.com")
	c := sm.Cloud()
	c.Web, c.GlobalBase, c.APIToken = srv.URL, srv.URL, "old-token"
	// The saved profile points at the same mock cloud the login uses.
	if err := pm.Save(context.Background(), &profile.Profile{
		Version: profile.CurrentSchemaVersion, CreatedAt: time.Now().UTC(),
		Credentials: profile.CloudCredentials{AccountEmail: "me@example.com", Password: "secret", Country: "1", AuthToken: "old-token"},
		Cameras:     map[string]profile.CameraProfile{},
	}); err != nil {
		t.Fatal(err)
	}
	streamMgr.SetLoginRenewer(renewCloudLogin)

	// camera offline, sign-in fine: no new sign-in
	cameraOffline.Store(true)
	_, err := c.VideoCall("cam")
	if nc := streamMgr.renewLogin(c, err); nc != nil || logins.Load() != 0 {
		t.Fatalf("signed in again for an offline camera (logins %d)", logins.Load())
	}
	cameraOffline.Store(false)

	_, err = c.VideoCall("cam")
	if err == nil {
		t.Fatal("expired token accepted")
	}
	nc := streamMgr.renewLogin(c, err)
	if nc == nil || nc.APIToken != "new-token" || logins.Load() != 1 {
		t.Fatalf("renewal: %+v, logins %d", nc, logins.Load())
	}
	if sm.Cloud() != nc || c.APIToken != "old-token" {
		t.Fatal("the new sign-in must replace the session's cloud without changing the old one")
	}
	if _, err := nc.VideoCall("cam"); err != nil {
		t.Fatalf("stream start after renewal: %v", err)
	}
	// a second camera's stream with the old cloud: gets the new one, no new sign-in
	if again := streamMgr.renewLogin(c, err); again != nc || logins.Load() != 1 {
		t.Fatalf("second renewal: %v, logins %d", again, logins.Load())
	}
	if saved := pm.GetProfile(); saved == nil || len(saved.Accounts) != 1 || saved.Accounts[0].AuthToken.Expose() != "new-token" {
		t.Fatal("new token not saved")
	}
}
