package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/Fever-r/BombeCam/pkg/bridge"
	"github.com/Fever-r/BombeCam/pkg/osaiovalue"
	"github.com/Fever-r/BombeCam/pkg/profile"
	"github.com/Fever-r/BombeCam/pkg/serverkey"
)

// signedWith reports whether a mock-cloud request carries a signature made
// with key (recomputed from the request headers).
func signedWith(r *http.Request, key string) bool {
	msg := r.Header.Get("appid") + r.Header.Get("timestamp")
	if r.Header.Get("ApiSignType") == "2" {
		msg += r.Header.Get("uid") + r.Header.Get("api-token")
	}
	m := hmac.New(sha256.New, []byte(key))
	m.Write([]byte(msg))
	return hmac.Equal([]byte(base64.StdEncoding.EncodeToString([]byte(hex.EncodeToString(m.Sum(nil))))), []byte(r.Header.Get("sign")))
}

// The vendor replaced its key: the old one is refused. Saving the new key
// signs every refused login in again at once and lifts the 10-minute wait on
// renewing an expired session.
func TestServerKeyChangeRecoversSessions(t *testing.T) {
	const oldKey, newKey = "synthetic-old-key", "synthetic-new-key"
	reply := func(w http.ResponseWriter, v any) { _ = json.NewEncoder(w).Encode(v) }
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !signedWith(r, newKey) {
			w.WriteHeader(http.StatusUnauthorized)
			reply(w, map[string]any{"code": 2001, "msg": "bad signature"})
			return
		}
		switch r.URL.Path {
		case "/v2/login/login":
			reply(w, map[string]any{"code": 1000, "data": map[string]any{"uid": "uid-1", "api_token": "fresh-token"}})
		case "/v2/webrtcsession/user/videocall", "/v2/device/list":
			if r.Header.Get("api-token") == "fresh-token" {
				reply(w, map[string]any{"code": 1000, "data": map[string]any{"session_id": "s1"}})
			} else {
				reply(w, map[string]any{"code": 1003, "msg": "token invalid"})
			}
		default:
			reply(w, map[string]any{"code": 1000, "data": nil})
		}
	}))
	defer srv.Close()

	dir := t.TempDir()
	keys := serverkey.Load(serverkey.Options{DefaultFile: filepath.Join(dir, serverkey.FileName), Getenv: func(string) string { return "" }})
	if err := keys.Save(oldKey); err != nil {
		t.Fatal(err)
	}
	setServerKeys(keys)
	defer setServerKeys(nil)
	pm := profile.NewManager(filepath.Join(dir, "p.enc"), []byte("01234567890123456789012345678901"))
	if err := pm.Save(context.Background(), &profile.Profile{
		Version: profile.CurrentSchemaVersion, CreatedAt: time.Now().UTC(),
		Credentials: profile.CloudCredentials{AccountEmail: "me@example.com", Password: "secret", Country: "1"},
		Cameras:     map[string]profile.CameraProfile{},
	}); err != nil {
		t.Fatal(err)
	}
	SetActiveProfileManager(pm)
	defer SetActiveProfileManager(nil)
	forgetRenewAttempts()

	template := NewSessionManager("1", "code", keys)
	template.Cloud().Web, template.Cloud().GlobalBase = srv.URL, srv.URL
	reg := NewSessionRegistry(template, "1", "code", keys)
	sm := reg.Ensure("me@example.com") // the login's own session
	c := sm.Cloud()
	streamMgr := NewStreamManager(c, "rtsp://127.0.0.1:8554", "http://127.0.0.1:8888", nil, false, "")
	streamMgr.SetSessionRegistry(reg)
	defer streamMgr.SetSessionRegistry(nil)
	streamMgr.SetProfileManager(pm)
	setCurrentStreamManager(streamMgr)
	t.Cleanup(func() { setCurrentStreamManager(nil) })
	streamMgr.SetLoginRenewer(renewCloudLogin)

	// With the old key, sign-in is refused and named as possibly a key problem.
	if err := sm.Login("me@example.com", "secret"); err == nil {
		t.Fatal("the old key was accepted")
	}
	if st := sm.GetStatus(); st.Status != SessionStatusInvalidCredentials || st.Error != rejectedSignInMessage {
		t.Fatalf("status after refusal = %+v", st)
	}
	// A renewal attempt fails too and starts the 10-minute wait.
	_, err := c.VideoCall("cam")
	if nc := streamMgr.renewLogin(c, err); nc != nil {
		t.Fatal("renewed with the old key")
	}

	if err := keys.Save(newKey); err != nil {
		t.Fatal(err)
	}
	resumeAfterKeyChange(template, streamMgr)
	if st := sm.GetStatus(); st.Status != SessionStatusAuthenticated {
		t.Fatalf("status after saving the new key = %+v; want authenticated", st)
	}
	if saved := pm.GetProfile(); saved == nil || len(saved.Accounts) != 1 || saved.Accounts[0].AuthToken.Expose() != "fresh-token" {
		t.Fatal("the new sign-in was not saved")
	}

	// An older session object can renew at once: the wait was lifted.
	stale := c.Clone()
	stale.UID, stale.APIToken = "uid-1", "expired-token"
	_, err = stale.VideoCall("cam")
	sm.SetCloud(stale)
	if nc := streamMgr.renewLogin(stale, err); nc == nil || nc.APIToken != "fresh-token" {
		t.Fatalf("renewal after the key change = %+v; want an immediate fresh sign-in", nc)
	}
}

// Startup without a key waits instead of retrying, and sends nothing.
func TestStartupWaitsForServerKey(t *testing.T) {
	keys := serverkey.Load(serverkey.Options{DefaultFile: filepath.Join(t.TempDir(), serverkey.FileName), Getenv: func(string) string { return "" }})
	setServerKeys(keys)
	defer setServerKeys(nil)
	defer setPendingStartup(nil)

	sm := NewSessionManager("1", "code", keys)
	resumed := false
	if !waitForOsaioValues(sm, func() { resumed = true }) {
		t.Fatal("startup did not wait for a missing key")
	}
	if st := sm.GetStatus(); st.Status != SessionStatusServerKeyMissing || st.Error != serverKeyMissingMessage {
		t.Fatalf("status = %+v", st)
	}
	if err := sm.Login("me@example.com", "secret"); err == nil || sm.GetStatus().Status != SessionStatusServerKeyMissing {
		t.Fatalf("login without a key: err=%v status=%v", err, sm.GetStatus().Status)
	}
	if err := keys.Save("synthetic-key"); err != nil {
		t.Fatal(err)
	}
	if f := takePendingStartup(); f == nil {
		t.Fatal("no startup was waiting for the key")
	} else {
		f()
	}
	if !resumed {
		t.Fatal("startup did not resume")
	}
	if waitForOsaioValues(sm, func() {}) {
		t.Fatal("startup still waits although a key is set")
	}
}

// The app ID works like the key: Osaio refuses requests with an outdated one,
// and saving the new one on the page signs the refused login in again.
func TestAppIDChangeRecoversSessions(t *testing.T) {
	const oldID, newID = "synthetic-old-app-id", "synthetic-new-app-id"
	reply := func(w http.ResponseWriter, v any) { _ = json.NewEncoder(w).Encode(v) }
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("appid") != newID || !signedWith(r, "synthetic-key") {
			w.WriteHeader(http.StatusUnauthorized)
			reply(w, map[string]any{"code": 2001, "msg": "bad signature"})
			return
		}
		switch r.URL.Path {
		case "/v2/login/login":
			reply(w, map[string]any{"code": 1000, "data": map[string]any{"uid": "uid-1", "api_token": "fresh-token"}})
		default:
			reply(w, map[string]any{"code": 1000, "data": nil})
		}
	}))
	defer srv.Close()

	dir := t.TempDir()
	keys := serverkey.Static("synthetic-key")
	ids := osaiovalue.Load(bridge.AppIDKind, osaiovalue.Options{DefaultFile: filepath.Join(dir, bridge.AppIDFileName), Getenv: func(string) string { return "" }})
	if err := ids.Save(oldID); err != nil {
		t.Fatal(err)
	}
	setAppIDs(ids)
	defer setAppIDs(nil)
	pm := profile.NewManager(filepath.Join(dir, "p.enc"), []byte("01234567890123456789012345678901"))
	if err := pm.Save(context.Background(), &profile.Profile{
		Version: profile.CurrentSchemaVersion, CreatedAt: time.Now().UTC(),
		Credentials: profile.CloudCredentials{AccountEmail: "me@example.com", Password: "secret", Country: "1"},
		Cameras:     map[string]profile.CameraProfile{},
	}); err != nil {
		t.Fatal(err)
	}
	SetActiveProfileManager(pm)
	defer SetActiveProfileManager(nil)

	template := NewSessionManager("1", "code", keys)
	template.Cloud().Web, template.Cloud().GlobalBase = srv.URL, srv.URL
	reg := NewSessionRegistry(template, "1", "code", keys)
	sm := reg.Ensure("me@example.com")
	streamMgr := NewStreamManager(sm.Cloud(), "rtsp://127.0.0.1:8554", "http://127.0.0.1:8888", nil, false, "")
	streamMgr.SetSessionRegistry(reg)
	defer streamMgr.SetSessionRegistry(nil)
	streamMgr.SetProfileManager(pm)
	setCurrentStreamManager(streamMgr)
	t.Cleanup(func() { setCurrentStreamManager(nil) })

	if err := sm.Login("me@example.com", "secret"); err == nil {
		t.Fatal("the old app ID was accepted")
	}
	if st := sm.GetStatus(); st.Status != SessionStatusInvalidCredentials || st.Error != rejectedSignInMessage {
		t.Fatalf("status after refusal = %+v", st)
	}
	if err := ids.Save(newID); err != nil {
		t.Fatal(err)
	}
	resumeAfterChange(appIDValue, template, streamMgr)
	if st := sm.GetStatus(); st.Status != SessionStatusAuthenticated {
		t.Fatalf("status after saving the new app ID = %+v; want authenticated", st)
	}
}

// Startup without an app ID waits instead of retrying, and sends nothing.
func TestStartupWaitsForAppID(t *testing.T) {
	keys := serverkey.Load(serverkey.Options{DefaultFile: filepath.Join(t.TempDir(), serverkey.FileName), Getenv: func(string) string { return "" }})
	if err := keys.Save("synthetic-key"); err != nil {
		t.Fatal(err)
	}
	setServerKeys(keys)
	defer setServerKeys(nil)
	ids := osaiovalue.Load(bridge.AppIDKind, osaiovalue.Options{DefaultFile: filepath.Join(t.TempDir(), bridge.AppIDFileName), Getenv: func(string) string { return "" }})
	setAppIDs(ids)
	defer setAppIDs(nil)
	defer setPendingStartup(nil)

	sm := NewSessionManager("1", "code", keys)
	resumed := false
	if !waitForOsaioValues(sm, func() { resumed = true }) {
		t.Fatal("startup did not wait for a missing app ID")
	}
	if st := sm.GetStatus(); st.Status != SessionStatusAppIDMissing || st.Error != appIDMissingMessage {
		t.Fatalf("status = %+v", st)
	}
	if err := sm.Login("me@example.com", "secret"); err == nil || sm.GetStatus().Status != SessionStatusAppIDMissing {
		t.Fatalf("login without an app ID: err=%v status=%v", err, sm.GetStatus().Status)
	}
	if err := ids.Save("synthetic-app-id"); err != nil {
		t.Fatal(err)
	}
	if f := takePendingStartup(); f == nil {
		t.Fatal("no startup was waiting for the app ID")
	} else {
		f()
	}
	if !resumed {
		t.Fatal("startup did not resume")
	}
	if waitForOsaioValues(sm, func() {}) {
		t.Fatal("startup still waits although an app ID is set")
	}
}
