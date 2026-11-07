package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/Fever-r/BombeCam/pkg/bridge"
	"github.com/Fever-r/BombeCam/pkg/policy"
	"github.com/Fever-r/BombeCam/pkg/profile"
	"github.com/Fever-r/BombeCam/pkg/renderer/openwrt"
	"github.com/Fever-r/BombeCam/pkg/routerpush"
)

// One router key set for all tests (RSA generation is slow).
var (
	testKeysOnce sync.Once
	testKeys     *routerpush.KeySet
)

func useTestKeys(t *testing.T) {
	t.Helper()
	testKeysOnce.Do(func() {
		ks, err := routerpush.NewKeySet()
		if err != nil {
			panic(err)
		}
		testKeys = ks
	})
	orig := newRouterKeySet
	newRouterKeySet = func() (*routerpush.KeySet, error) { return testKeys, nil }
	t.Cleanup(func() { newRouterKeySet = orig })
	recordRouterOutcome(routerOutcome{OK: true}, false)
}

type routerCall struct {
	target  routerpush.Target
	command string
	stdin   []byte
}

func (c routerCall) viaKey() bool { return len(c.target.Signers) > 0 && c.target.Password == "" }

// specMACs lists the MACs in a gate "apply" stdin.
func (c routerCall) specMACs() []string {
	var out []string
	sc := bufio.NewScanner(bytes.NewReader(c.stdin))
	for sc.Scan() {
		if f := strings.Fields(sc.Text()); len(f) >= 2 && f[0] == "camera" {
			out = append(out, f[1])
		}
	}
	return out
}

// stubRouter replaces the SSH push for the duration of a test.
func stubRouter(t *testing.T, fn func(routerCall) (routerpush.Result, error)) *[]routerCall {
	t.Helper()
	useTestKeys(t)
	var mu sync.Mutex
	var calls []routerCall
	orig := routerRun
	routerRun = func(_ context.Context, tg routerpush.Target, cmd string, stdin []byte) (routerpush.Result, error) {
		c := routerCall{target: tg, command: cmd, stdin: stdin}
		mu.Lock()
		calls = append(calls, c)
		mu.Unlock()
		return fn(c)
	}
	t.Cleanup(func() { routerRun = orig })
	return &calls
}

// okRouter answers like the router script: connect, gate apply/uninstall, and
// password-path applies.
func okRouter(firewall string) func(routerCall) (routerpush.Result, error) {
	return func(c routerCall) (routerpush.Result, error) {
		res := routerpush.Result{HostKey: "SHA256:routerkey", HostKeyType: "ssh-ed25519"}
		line := ""
		switch {
		case strings.Contains(c.command, " connect "):
			line = "status=ok connected=yes firewall=" + firewall + " version=" + openwrt.ScriptVersion()
		case c.command == openwrt.GateApply:
			n := len(c.specMACs())
			block := "yes"
			if n == 0 {
				block = "no"
			}
			line = fmt.Sprintf("status=ok block=%s firewall=%s cameras=%d connected=yes version=%s", block, firewall, n, openwrt.ScriptVersion())
		case c.command == openwrt.GateUninstall:
			line = "status=ok block=no firewall=" + firewall + " cameras=0 connected=no"
		case strings.Contains(c.command, " apply no"):
			line = "status=ok block=no firewall=" + firewall + " cameras=0"
		case strings.Contains(c.command, " apply yes"):
			line = "status=ok block=yes firewall=" + firewall + " cameras=1"
		default:
			return res, fmt.Errorf("unexpected command %q", c.command)
		}
		res.Output = "Firewall: " + firewall + "\nBOMBECAM_RESULT " + line + "\n"
		return res, nil
	}
}

func seedPrivacyProfile(t *testing.T, pm profile.ProfileManager, cams map[string]profile.CameraProfile) {
	t.Helper()
	if err := pm.Save(context.Background(), &profile.Profile{
		Version:     profile.CurrentSchemaVersion,
		CreatedAt:   time.Now().UTC(),
		Credentials: profile.CloudCredentials{AccountEmail: "owner@example.com", Password: "pw"},
		Cameras:     cams,
	}); err != nil {
		t.Fatal(err)
	}
}

func doJSON(t *testing.T, mux http.Handler, method, path string, body any) (int, map[string]any) {
	t.Helper()
	var rdr *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	} else {
		rdr = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, rdr)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

var testCamWithMAC = map[string]profile.CameraProfile{
	"cam-1": {UUID: "cam-1", Name: "Test Camera", MACAddress: "AA-BB-CC-DD-EE-01"},
}

func twoCams() map[string]profile.CameraProfile {
	return map[string]profile.CameraProfile{
		"cam-1": {UUID: "cam-1", Name: "Test Camera", MACAddress: "AA-BB-CC-DD-EE-01"},
		"cam-2": {UUID: "cam-2", Name: "Porch", MACAddress: "aa:bb:cc:dd:ee:02"},
	}
}

func camStates(st map[string]any) map[string]string {
	out := map[string]string{}
	for _, c := range st["cameras"].([]any) {
		m := c.(map[string]any)
		out[m["id"].(string)] = m["state"].(string)
	}
	return out
}

func routerField(resp map[string]any, key string) any {
	r, _ := resp["router"].(map[string]any)
	return r[key]
}

func TestPrivacyStatus(t *testing.T) {
	_, _, pm, _, mux, _ := setupFullTestEnvironment(t)
	useTestKeys(t)

	code, st := doJSON(t, mux, http.MethodGet, "/api/v1/privacy", nil)
	if code != 200 || st["master"] != "empty" || st["in_sync"] != true || st["cameras_connected"] != float64(0) {
		t.Fatalf("no cameras: %d %+v", code, st)
	}

	seedPrivacyProfile(t, pm, testCamWithMAC)
	_, st = doJSON(t, mux, http.MethodGet, "/api/v1/privacy", nil)
	if st["master"] != "none" || camStates(st)["cam-1"] != camStateNotBlocked || st["block_cloud_video"] != nil || st["manual_command"] != nil {
		t.Fatalf("nothing chosen yet: %+v", st)
	}
	if st["headline"] != policy.Headline {
		t.Errorf("headline wrong: %v", st["headline"])
	}
	allow := st["allowlist"].([]any)
	if len(allow) != 4 || allow[0].(map[string]any)["host"] != "mqtts02-us.osaio.net" {
		t.Errorf("allowlist = %+v", allow)
	}
	if st["script_version"] != openwrt.ScriptVersion() {
		t.Errorf("script_version = %v", st["script_version"])
	}

	// Chosen but no MAC yet: waiting, and not ready.
	seedPrivacyProfile(t, pm, map[string]profile.CameraProfile{"cam-1": {UUID: "cam-1", Name: "Test Camera"}})
	stubRouter(t, okRouter("fw4"))
	doJSON(t, mux, http.MethodPost, "/api/v1/privacy/block-all", map[string]any{"blocked": true})
	_, st = doJSON(t, mux, http.MethodGet, "/api/v1/privacy", nil)
	if camStates(st)["cam-1"] != camStateWaitingForMAC || st["ready"] != false || !strings.Contains(st["not_ready_reason"].(string), "Test Camera") {
		t.Fatalf("missing MAC: %+v", st)
	}
}

func TestPrivacy_ConnectThenPerCameraToggles(t *testing.T) {
	_, _, pm, _, mux, _ := setupFullTestEnvironment(t)
	seedPrivacyProfile(t, pm, twoCams())
	calls := stubRouter(t, okRouter("fw4"))

	// Choosing before the router is connected saves the choice only.
	code, resp := doJSON(t, mux, http.MethodPost, "/api/v1/privacy/block-all", map[string]any{"blocked": true})
	if code != 200 || routerField(resp, "error") != "router_not_connected" || len(*calls) != 0 {
		t.Fatalf("block-all before connect: %d %+v", code, resp)
	}
	st := resp["status"].(map[string]any)
	if st["master"] != "all" || camStates(st)["cam-1"] != camStateNotApplied || st["block_new_cameras"] != true {
		t.Fatalf("status before connect: %+v", st)
	}

	// Connect: password session installs the script and key, then the
	// choices go over the key.
	code, resp = doJSON(t, mux, http.MethodPost, "/api/v1/privacy/router/connect", map[string]any{
		"router_address": "192.168.8.1", "router_password": "hunter2",
	})
	if code != 200 || resp["ok"] != true || resp["firewall"] != "fw4" {
		t.Fatalf("connect: %d %+v", code, resp)
	}
	if len(*calls) != 2 {
		t.Fatalf("expected connect + apply, got %d calls", len(*calls))
	}
	c0, c1 := (*calls)[0], (*calls)[1]
	if c0.target.Password != "hunter2" || c0.target.Address != "192.168.8.1:22" || c0.target.User != "root" || c0.target.PinnedHostKey != "" ||
		!strings.Contains(c0.command, "sh /tmp/bombecam-router.sh connect --key 'ssh-ed25519 ") || !bytes.Equal(c0.stdin, openwrt.Script()) {
		t.Errorf("connect call = %+v / %s", c0.target, c0.command)
	}
	if !c1.viaKey() || c1.command != openwrt.GateApply || c1.target.PinnedHostKey != "SHA256:routerkey" || c1.target.PinnedHostKeyType != "ssh-ed25519" ||
		strings.Join(c1.specMACs(), ",") != "aa:bb:cc:dd:ee:01,aa:bb:cc:dd:ee:02" {
		t.Errorf("apply call = %+v / %s / %q", c1.target, c1.command, c1.stdin)
	}
	prof := pm.GetProfile()
	p := prof.Privacy
	if !p.RouterConnected || prof.RouterKey.IsEmpty() || p.RouterHostKey != "SHA256:routerkey" || p.RouterFirewall != "fw4" ||
		len(p.AppliedCameras) != 2 || p.BlockCloudVideo == nil || !*p.BlockCloudVideo || p.RouterScriptVersion != openwrt.ScriptVersion() {
		t.Fatalf("saved after connect: %+v", p)
	}
	raw, _ := json.Marshal(prof)
	if bytes.Contains(raw, []byte("hunter2")) || bytes.Contains(raw, []byte("PRIVATE KEY")) {
		t.Fatal("the router password is never stored, and the key never leaves the encrypted profile")
	}

	// One camera off: only that camera leaves the router, instantly.
	code, resp = doJSON(t, mux, http.MethodPost, "/api/v1/privacy/camera", map[string]any{"camera_id": "cam-2", "blocked": false})
	if code != 200 || routerField(resp, "ok") != true || len(*calls) != 3 {
		t.Fatalf("toggle cam-2 off: %d %+v", code, resp)
	}
	if c := (*calls)[2]; !c.viaKey() || strings.Join(c.specMACs(), ",") != "aa:bb:cc:dd:ee:01" {
		t.Errorf("toggle call = %q", c.stdin)
	}
	st = resp["status"].(map[string]any)
	if st["master"] != "some" || camStates(st)["cam-1"] != camStateBlocked || camStates(st)["cam-2"] != camStateNotBlocked {
		t.Fatalf("after toggle: %+v", st)
	}

	// No change, no router call.
	doJSON(t, mux, http.MethodPost, "/api/v1/privacy/camera", map[string]any{"camera_id": "cam-2", "blocked": false})
	if len(*calls) != 3 {
		t.Fatalf("an unchanged choice must not contact the router (%d calls)", len(*calls))
	}

	// Unknown camera and bad bodies.
	if code, _ := doJSON(t, mux, http.MethodPost, "/api/v1/privacy/camera", map[string]any{"camera_id": "nope", "blocked": true}); code != http.StatusNotFound {
		t.Errorf("unknown camera: %d", code)
	}
	if code, _ := doJSON(t, mux, http.MethodPost, "/api/v1/privacy/camera", map[string]any{"camera_id": "cam-1"}); code != http.StatusBadRequest {
		t.Errorf("missing blocked: %d", code)
	}
	if code, _ := doJSON(t, mux, http.MethodGet, "/api/v1/privacy/camera", nil); code != http.StatusMethodNotAllowed {
		t.Errorf("GET camera: %d", code)
	}

	// Unblock all: an empty list goes to the router; BombeCam stays connected.
	code, resp = doJSON(t, mux, http.MethodPost, "/api/v1/privacy/block-all", map[string]any{"blocked": false})
	if code != 200 || len(*calls) != 4 || len((*calls)[3].specMACs()) != 0 {
		t.Fatalf("unblock all: %d %+v", code, resp)
	}
	p = pm.GetProfile().Privacy
	if !p.RouterConnected || len(p.AppliedCameras) != 0 || *p.BlockCloudVideo || p.BlockNewCameras {
		t.Fatalf("after unblock all: %+v", p)
	}
}

func TestPrivacy_NewCamerasFollowBlockAll(t *testing.T) {
	_, streamMgr, pm, _, mux, _ := setupFullTestEnvironment(t)
	seedPrivacyProfile(t, pm, testCamWithMAC)
	calls := stubRouter(t, okRouter("fw4"))
	doJSON(t, mux, http.MethodPost, "/api/v1/privacy/router/connect", map[string]any{"router_address": "192.168.8.1", "router_password": "pw"})
	doJSON(t, mux, http.MethodPost, "/api/v1/privacy/block-all", map[string]any{"blocked": true})

	_, _ = pm.Update(context.Background(), func(p *profile.Profile) error {
		p.Cameras["cam-3"] = profile.CameraProfile{UUID: "cam-3", Name: "Garage", MACAddress: "aa:bb:cc:dd:ee:03"}
		return nil
	})
	_, st := doJSON(t, mux, http.MethodGet, "/api/v1/privacy", nil)
	if camStates(st)["cam-3"] != camStatePending || st["in_sync"] != false {
		t.Fatalf("new camera should be chosen and pending: %+v", st)
	}
	n := len(*calls)
	if o := syncRouter(context.Background(), streamMgr, pm, false); !o.OK || !o.Changed {
		t.Fatalf("sync: %+v", o)
	}
	if got := (*calls)[n].specMACs(); len(got) != 2 {
		t.Fatalf("new camera not sent to the router: %v", got)
	}
}

func TestPrivacy_KeyRejectedNeedsPassword(t *testing.T) {
	_, _, pm, _, mux, _ := setupFullTestEnvironment(t)
	seedPrivacyProfile(t, pm, twoCams())
	ok := okRouter("fw4")
	rejectKey := false
	stubRouter(t, func(c routerCall) (routerpush.Result, error) {
		if rejectKey && c.viaKey() {
			return routerpush.Result{}, routerpush.ErrAuth
		}
		return ok(c)
	})
	doJSON(t, mux, http.MethodPost, "/api/v1/privacy/router/connect", map[string]any{"router_address": "192.168.8.1", "router_password": "pw"})

	rejectKey = true // e.g. the router was reset
	code, resp := doJSON(t, mux, http.MethodPost, "/api/v1/privacy/camera", map[string]any{"camera_id": "cam-1", "blocked": true})
	if code != 200 || routerField(resp, "error") != "router_password_needed" {
		t.Fatalf("got %d %+v", code, resp)
	}
	if pm.GetProfile().Privacy.RouterConnected {
		t.Fatal("router should be marked not connected")
	}
	st := resp["status"].(map[string]any)
	if camStates(st)["cam-1"] != camStateNotApplied || st["last_error"] == nil {
		t.Fatalf("status: %+v", st)
	}
}

func TestPrivacy_ConnectFallsBackToPasswordWhenKeyRefused(t *testing.T) {
	_, _, pm, _, mux, _ := setupFullTestEnvironment(t)
	seedPrivacyProfile(t, pm, testCamWithMAC)
	ok := okRouter("fw3")
	calls := stubRouter(t, func(c routerCall) (routerpush.Result, error) {
		if c.viaKey() {
			return routerpush.Result{}, routerpush.ErrAuth
		}
		return ok(c)
	})
	code, resp := doJSON(t, mux, http.MethodPost, "/api/v1/privacy/apply", map[string]any{
		"block_cloud_video": true, "router_address": "192.168.8.1", "router_password": "pw",
	})
	if code != 200 || resp["ok"] != true || !strings.Contains(fmt.Sprint(resp["warnings"]), "doesn't accept BombeCam's key") {
		t.Fatalf("fallback: %d %+v", code, resp)
	}
	last := (*calls)[len(*calls)-1]
	if last.viaKey() || !strings.Contains(last.command, "apply yes --camera 'Test Camera=aa:bb:cc:dd:ee:01'") || last.target.PinnedHostKey != "SHA256:routerkey" {
		t.Errorf("password apply = %+v / %s", last.target, last.command)
	}
	p := pm.GetProfile().Privacy
	if p.RouterConnected || len(p.AppliedCameras) != 1 {
		t.Fatalf("saved: %+v", p)
	}
}

func TestPrivacy_ConnectErrors(t *testing.T) {
	_, _, pm, _, mux, _ := setupFullTestEnvironment(t)
	seedPrivacyProfile(t, pm, testCamWithMAC)
	body := map[string]any{"router_address": "192.168.8.1", "router_password": "pw"}

	cases := []struct {
		name string
		res  routerpush.Result
		err  error
		code int
		key  string
	}{
		{"auth", routerpush.Result{}, routerpush.ErrAuth, http.StatusUnauthorized, "router_auth"},
		{"identity", routerpush.Result{HostKey: "SHA256:new"}, routerpush.ErrHostKeyChanged, http.StatusConflict, "router_identity_changed"},
		{"unreachable", routerpush.Result{}, errors.New("wrapped: " + routerpush.ErrUnreachable.Error()), http.StatusBadGateway, "router_unreachable"},
		{"script", routerpush.Result{ExitCode: 1, Output: "ERROR: no supported firewall found\nBOMBECAM_RESULT status=error\n"}, nil, http.StatusBadGateway, "router_script_failed"},
	}
	for _, tc := range cases {
		stubRouter(t, func(routerCall) (routerpush.Result, error) { return tc.res, tc.err })
		code, resp := doJSON(t, mux, http.MethodPost, "/api/v1/privacy/router/connect", body)
		if code != tc.code || resp["error"] != tc.key {
			t.Errorf("%s: got %d %+v", tc.name, code, resp)
		}
		if tc.name == "script" && !strings.Contains(resp["message"].(string), "no supported firewall found") {
			t.Errorf("script failure message should quote the router: %v", resp["message"])
		}
	}
	if p := pm.GetProfile().Privacy; p.RouterConnected || p.RouterAddress != "" || !pm.GetProfile().RouterKey.IsEmpty() {
		t.Fatalf("a failed connect must not record a router: %+v", p)
	}

	stubRouter(t, okRouter("fw3"))
	for _, bad := range []map[string]any{
		{"router_address": "http://192.168.8.1"},
		{"router_address": "192.168.8.1", "router_user": "root; reboot"},
		{"router_address": ""},
	} {
		if code, _ := doJSON(t, mux, http.MethodPost, "/api/v1/privacy/router/connect", bad); code != http.StatusBadRequest {
			t.Errorf("%v: expected 400, got %d", bad, code)
		}
	}
	if code, _ := doJSON(t, mux, http.MethodPost, "/api/v1/privacy/apply", map[string]any{"router_address": "192.168.8.1"}); code != http.StatusBadRequest {
		t.Errorf("apply without block_cloud_video: %d", code)
	}
	if code, _ := doJSON(t, mux, http.MethodGet, "/api/v1/privacy/router/connect", nil); code != http.StatusMethodNotAllowed {
		t.Errorf("GET connect: %d", code)
	}
}

func TestPrivacy_ApplyPinsHostKeyPerRouter(t *testing.T) {
	_, _, pm, _, mux, _ := setupFullTestEnvironment(t)
	seedPrivacyProfile(t, pm, testCamWithMAC)
	calls := stubRouter(t, okRouter("fw4"))
	doJSON(t, mux, http.MethodPost, "/api/v1/privacy/apply", map[string]any{"block_cloud_video": true, "router_address": "192.168.8.1", "router_password": "pw"})
	code, resp := doJSON(t, mux, http.MethodPost, "/api/v1/privacy/apply", map[string]any{"block_cloud_video": true, "router_address": "192.168.8.1", "router_password": "pw"})
	if code != 200 || resp["cameras"] != float64(1) {
		t.Fatalf("second apply: %d %+v", code, resp)
	}
	if c := (*calls)[2]; c.target.PinnedHostKey != "SHA256:routerkey" || c.target.PinnedHostKeyType != "ssh-ed25519" {
		t.Errorf("same router must be pinned: %+v", c.target)
	}
	doJSON(t, mux, http.MethodPost, "/api/v1/privacy/apply", map[string]any{"block_cloud_video": true, "router_address": "10.0.0.1", "router_password": "pw"})
	if c := (*calls)[4]; c.target.PinnedHostKey != "" {
		t.Error("a new router must not inherit the old host key")
	}
}

func TestPrivacy_CameraRemovalUpdatesRouter(t *testing.T) {
	_, streamMgr, pm, _, mux, _ := setupFullTestEnvironment(t)
	seedPrivacyProfile(t, pm, twoCams())
	calls := stubRouter(t, okRouter("fw4"))
	doJSON(t, mux, http.MethodPost, "/api/v1/privacy/block-all", map[string]any{"blocked": true})
	doJSON(t, mux, http.MethodPost, "/api/v1/privacy/router/connect", map[string]any{"router_address": "192.168.8.1", "router_password": "pw"})
	n := len(*calls)

	_, _ = streamMgr.Enroll([]string{"cam-1"}, []bridge.Device{{UUID: "cam-1", Name: "Test Camera", Type: "WS03", Online: 1}})
	code, resp := doJSON(t, mux, http.MethodDelete, "/api/v1/cameras/cam-1", nil)
	if code != 200 || resp["still_on_router"] != false || !strings.Contains(resp["firewall_note"].(string), "Removed from your router too") {
		t.Fatalf("remove: %d %+v", code, resp)
	}
	if len(*calls) != n+1 || strings.Join((*calls)[n].specMACs(), ",") != "aa:bb:cc:dd:ee:02" {
		t.Fatalf("router not updated on removal: %+v", (*calls)[n:])
	}
	if _, ok := pm.GetProfile().Privacy.Blocked["cam-1"]; ok {
		t.Error("the removed camera's choice should be dropped")
	}

	// Without a connected router, removal says the router still has it.
	_, _ = pm.Update(context.Background(), func(p *profile.Profile) error { p.Privacy.RouterConnected = false; return nil })
	code, resp = doJSON(t, mux, http.MethodDelete, "/api/v1/cameras/cam-2", nil)
	if code != 200 || resp["still_on_router"] != true || !strings.Contains(resp["firewall_note"].(string), "Connect the router") {
		t.Fatalf("remove without router: %d %+v", code, resp)
	}
}

func TestPrivacy_DisconnectAndForget(t *testing.T) {
	_, _, pm, _, mux, _ := setupFullTestEnvironment(t)
	seedPrivacyProfile(t, pm, twoCams())
	calls := stubRouter(t, okRouter("fw4"))
	doJSON(t, mux, http.MethodPost, "/api/v1/privacy/block-all", map[string]any{"blocked": true})
	doJSON(t, mux, http.MethodPost, "/api/v1/privacy/router/connect", map[string]any{"router_address": "192.168.8.1", "router_password": "pw"})

	// Forget (router reset/replaced): nothing sent, choices kept, key gone.
	n := len(*calls)
	if code, _ := doJSON(t, mux, http.MethodPost, "/api/v1/privacy/forget-router", nil); code != 200 {
		t.Fatalf("forget: %d", code)
	}
	prof := pm.GetProfile()
	if len(*calls) != n || prof.Privacy.RouterAddress != "" || prof.Privacy.RouterConnected || !prof.RouterKey.IsEmpty() ||
		!prof.Privacy.BlockNewCameras || !prof.Privacy.Blocked["cam-1"] {
		t.Fatalf("after forget: %+v", prof.Privacy)
	}

	// Disconnect: uninstall over the key, then everything reset.
	doJSON(t, mux, http.MethodPost, "/api/v1/privacy/router/connect", map[string]any{"router_address": "192.168.8.1", "router_password": "pw"})
	code, resp := doJSON(t, mux, http.MethodPost, "/api/v1/privacy/router/disconnect", nil)
	if code != 200 || resp["ok"] != true {
		t.Fatalf("disconnect: %d %+v", code, resp)
	}
	if c := (*calls)[len(*calls)-1]; !c.viaKey() || c.command != openwrt.GateUninstall {
		t.Fatalf("disconnect call = %s", c.command)
	}
	prof = pm.GetProfile()
	if prof.Privacy.RouterAddress != "" || !prof.RouterKey.IsEmpty() || prof.Privacy.BlockNewCameras || desiredBlocked(prof.Privacy, "cam-1") {
		t.Fatalf("after disconnect: %+v", prof.Privacy)
	}

	// Disconnect with an unreachable router reports it and forgets nothing.
	doJSON(t, mux, http.MethodPost, "/api/v1/privacy/router/connect", map[string]any{"router_address": "192.168.8.1", "router_password": "pw"})
	stubRouter(t, func(routerCall) (routerpush.Result, error) { return routerpush.Result{}, routerpush.ErrUnreachable })
	code, resp = doJSON(t, mux, http.MethodPost, "/api/v1/privacy/router/disconnect", nil)
	if code != http.StatusBadGateway || resp["error"] != "router_unreachable" || pm.GetProfile().Privacy.RouterAddress == "" {
		t.Fatalf("disconnect unreachable: %d %+v", code, resp)
	}
}

func TestPrivacy_DeleteAllRemovesRouterRules(t *testing.T) {
	_, _, pm, _, mux, _ := setupFullTestEnvironment(t)
	seedPrivacyProfile(t, pm, testCamWithMAC)
	calls := stubRouter(t, okRouter("fw4"))
	doJSON(t, mux, http.MethodPost, "/api/v1/privacy/apply", map[string]any{"block_cloud_video": true, "router_address": "192.168.8.1", "router_password": "pw"})
	code, resp := doJSON(t, mux, http.MethodPost, "/api/v1/profile/forget", nil)
	if code != 200 || resp["router_updated"] != true || !strings.Contains(resp["router_message"].(string), "removed from your router") {
		t.Fatalf("forget: %d %+v", code, resp)
	}
	if c := (*calls)[len(*calls)-1]; c.command != openwrt.GateUninstall {
		t.Fatalf("expected uninstall, got %s", c.command)
	}
}

func TestPrivacyRouterScriptDownload(t *testing.T) {
	_, _, _, _, mux, _ := setupFullTestEnvironment(t)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/privacy/router-script", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != 200 || !strings.HasPrefix(rec.Body.String(), "#!/bin/sh") ||
		!strings.Contains(rec.Header().Get("Content-Disposition"), "bombecam-router.sh") {
		t.Fatalf("download: %d %q %q", rec.Code, rec.Header().Get("Content-Disposition"), rec.Body.String()[:40])
	}
}

func TestOnboardingSetupKeepsRouterSettings(t *testing.T) {
	sm, _, pm, _, mux, _ := setupFullTestEnvironment(t)
	mockServer, mockURL := setupMockCloudServer(t, nil, nil)
	defer mockServer.Close()
	sm.Cloud().GlobalBase = mockURL

	seedPrivacyProfile(t, pm, testCamWithMAC)
	_, _ = pm.Update(context.Background(), func(p *profile.Profile) error {
		p.Privacy = profile.PrivacySettings{BlockCloudVideo: profile.BoolPtr(true), RouterHostKey: "SHA256:k",
			AppliedCameras: []string{"aa:bb:cc:dd:ee:01"}, Blocked: map[string]bool{"cam-1": true}, RouterConnected: true}
		p.RouterKey = "KEYDATA"
		return nil
	})
	code, resp := doJSON(t, mux, http.MethodPost, "/api/v1/onboarding/setup", map[string]any{
		"account_email": "owner@example.com", "password": "pw", "country": "1", "force": true,
	})
	if code != 200 {
		t.Fatalf("setup: %d %+v", code, resp)
	}
	prof := pm.GetProfile()
	if p := prof.Privacy; p.BlockCloudVideo == nil || !*p.BlockCloudVideo || p.RouterHostKey != "SHA256:k" || !p.Blocked["cam-1"] ||
		prof.RouterKey.Expose() != "KEYDATA" {
		t.Fatalf("re-sign-in dropped router settings: %+v", p)
	}
}

func TestDesiredBlockedLegacyYes(t *testing.T) {
	// Profiles from the single Yes/No version: Yes meant every camera.
	ps := profile.PrivacySettings{BlockCloudVideo: profile.BoolPtr(true)}
	if !desiredBlocked(ps, "any") {
		t.Fatal("legacy Yes should block every camera")
	}
	ps.Blocked = map[string]bool{"a": false}
	if desiredBlocked(ps, "a") || desiredBlocked(ps, "b") {
		t.Fatal("explicit choices win over the legacy setting")
	}
}

func TestParseWindowsDefaultGateway(t *testing.T) {
	out := `
IPv4 Route Table
===========================================================================
Active Routes:
Network Destination        Netmask          Gateway       Interface  Metric
          0.0.0.0          0.0.0.0      10.0.0.1       10.0.0.23     50
          0.0.0.0          0.0.0.0   192.168.8.1    192.168.8.123     25
===========================================================================`
	if got := parseWindowsDefaultGateway(out); got != "192.168.8.1" {
		t.Fatalf("got %q", got)
	}
	if got := parseWindowsDefaultGateway("nothing"); got != "" {
		t.Fatalf("got %q", got)
	}
}

// TestPrivacy_EndToEndOverSSH runs the whole path for real: HTTP -> SSH (an
// in-process server that enforces authorized_keys forced commands like
// dropbear) -> the actual router script in dry-run mode -> saved state.
func TestPrivacy_EndToEndOverSSH(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs a POSIX shell")
	}
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	useTestKeys(t)
	root := t.TempDir()
	srv := startScriptSSHServer(t, "pw", root)

	orig := routerRun
	routerRun = routerpush.Run
	t.Cleanup(func() { routerRun = orig })

	_, streamMgr, pm, _, mux, _ := setupFullTestEnvironment(t)
	seedPrivacyProfile(t, pm, twoCams())
	camList := func() string {
		b, _ := os.ReadFile(filepath.Join(root, "etc/bombecam/cameras"))
		return string(b)
	}

	doJSON(t, mux, http.MethodPost, "/api/v1/privacy/block-all", map[string]any{"blocked": true})
	code, resp := doJSON(t, mux, http.MethodPost, "/api/v1/privacy/router/connect", map[string]any{"router_address": srv.addr, "router_password": "pw"})
	if code != 200 || resp["ok"] != true {
		t.Fatalf("connect over SSH: %d %+v", code, resp)
	}
	out := resp["output"].(string)
	for _, want := range []string{
		"BOMBECAM_RESULT status=ok connected=yes firewall=fw4",
		"ether saddr aa:bb:cc:dd:ee:01 limit rate over 4096 bytes/second",
		policy.Headline,
		"BOMBECAM_RESULT status=ok block=yes firewall=fw4 cameras=2",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("router output missing %q:\n%s", want, out)
		}
	}
	if camList() != "aa:bb:cc:dd:ee:01\tTest Camera\naa:bb:cc:dd:ee:02\tPorch\n" {
		t.Fatalf("router camera list = %q", camList())
	}
	if srv.keyLogins() != 1 {
		t.Fatalf("the apply after connecting should use BombeCam's key (key logins: %d)", srv.keyLogins())
	}

	// Per-camera toggle over the key (the server refuses the password now).
	srv.setPassword("")
	code, resp = doJSON(t, mux, http.MethodPost, "/api/v1/privacy/camera", map[string]any{"camera_id": "cam-2", "blocked": false})
	if code != 200 || routerField(resp, "ok") != true || camList() != "aa:bb:cc:dd:ee:01\tTest Camera\n" {
		t.Fatalf("toggle over key: %d %+v list=%q", code, resp, camList())
	}

	// Removing a blocked camera takes it off the router.
	_, _ = streamMgr.Enroll([]string{"cam-1"}, []bridge.Device{{UUID: "cam-1", Name: "Test Camera", Type: "WS03", Online: 1}})
	code, resp = doJSON(t, mux, http.MethodDelete, "/api/v1/cameras/cam-1", nil)
	if code != 200 || resp["still_on_router"] != false {
		t.Fatalf("remove: %d %+v", code, resp)
	}
	if camList() != "" {
		t.Fatalf("router still lists cameras after removal: %q", camList())
	}
	if _, err := os.Stat(filepath.Join(root, "usr/sbin/bombecam-router")); err != nil {
		t.Fatal("with no cameras blocked, BombeCam stays connected (program kept)")
	}

	// Disconnect removes the key from the router.
	code, resp = doJSON(t, mux, http.MethodPost, "/api/v1/privacy/router/disconnect", nil)
	if code != 200 {
		t.Fatalf("disconnect: %d %+v", code, resp)
	}
	ak, _ := os.ReadFile(filepath.Join(root, "etc/dropbear/authorized_keys"))
	if strings.Contains(string(ak), "bombecam") {
		t.Fatalf("key left on router:\n%s", ak)
	}
	if _, err := os.Stat(filepath.Join(root, "etc/bombecam")); !os.IsNotExist(err) {
		t.Fatal("router config left behind after disconnect")
	}
}

// scriptSSHServer is an in-process SSH server that runs commands with sh in
// dry-run mode against a fake router root. Password logins run the requested
// command; key logins honour the forced command from the authorized_keys file
// the router script writes, with SSH_ORIGINAL_COMMAND set, as dropbear does.
type scriptSSHServer struct {
	addr     string
	mu       sync.Mutex
	password string
	keyCount int
}

func (s *scriptSSHServer) setPassword(p string) { s.mu.Lock(); s.password = p; s.mu.Unlock() }
func (s *scriptSSHServer) keyLogins() int       { s.mu.Lock(); defer s.mu.Unlock(); return s.keyCount }

func startScriptSSHServer(t *testing.T, password, root string) *scriptSSHServer {
	t.Helper()
	srv := &scriptSSHServer{password: password}
	authKeys := filepath.Join(root, "etc/dropbear/authorized_keys")
	env := append(os.Environ(), "BC_ROOT="+root, "BC_DRYRUN=1", "BC_FW=fw4", "BC_AUTH_KEYS="+authKeys)
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	signer, _ := ssh.NewSignerFromKey(priv)
	cfg := &ssh.ServerConfig{
		PasswordCallback: func(_ ssh.ConnMetadata, pw []byte) (*ssh.Permissions, error) {
			srv.mu.Lock()
			defer srv.mu.Unlock()
			if srv.password != "" && string(pw) == srv.password {
				return nil, nil
			}
			return nil, errors.New("denied")
		},
		PublicKeyCallback: func(_ ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			data, _ := os.ReadFile(authKeys)
			for len(data) > 0 {
				pub, _, opts, rest, err := ssh.ParseAuthorizedKey(data)
				if err != nil {
					break
				}
				data = rest
				if bytes.Equal(pub.Marshal(), key.Marshal()) {
					for _, o := range opts {
						if v, ok := strings.CutPrefix(o, "command="); ok {
							srv.mu.Lock()
							srv.keyCount++
							srv.mu.Unlock()
							return &ssh.Permissions{Extensions: map[string]string{"force-command": strings.Trim(v, `"`)}}, nil
						}
					}
					return nil, nil
				}
			}
			return nil, errors.New("unknown key")
		},
	}
	cfg.AddHostKey(signer)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	srv.addr = ln.Addr().String()
	scriptPath := filepath.Join(root, "uploaded-script.sh")
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				conn, chans, reqs, err := ssh.NewServerConn(c, cfg)
				if err != nil {
					return
				}
				forced := ""
				if conn.Permissions != nil {
					forced = conn.Permissions.Extensions["force-command"]
				}
				go ssh.DiscardRequests(reqs)
				for nc := range chans {
					ch, creqs, err := nc.Accept()
					if err != nil {
						continue
					}
					go func() {
						for req := range creqs {
							if req.Type != "exec" {
								_ = req.Reply(false, nil)
								continue
							}
							n := binary.BigEndian.Uint32(req.Payload[:4])
							requested := string(req.Payload[4 : 4+n])
							_ = req.Reply(true, nil)
							cmd := exec.Command("sh", "-c", strings.ReplaceAll(requested, openwrt.RemotePath, scriptPath))
							cmd.Env = env
							if forced != "" {
								cmd = exec.Command("sh", "-c", forced)
								cmd.Env = append(env, "SSH_ORIGINAL_COMMAND="+requested)
							}
							cmd.Stdin = ch
							cmd.Stdout = ch
							cmd.Stderr = ch.Stderr()
							status := uint32(0)
							if err := cmd.Run(); err != nil {
								status = 1
								if ee, ok := err.(*exec.ExitError); ok {
									status = uint32(ee.ExitCode())
								}
							}
							b := make([]byte, 4)
							binary.BigEndian.PutUint32(b, status)
							_, _ = ch.SendRequest("exit-status", false, b)
							ch.Close()
						}
					}()
				}
			}(c)
		}
	}()
	return srv
}

// After an apply, the router's own report decides what BombeCam shows: a
// firewall that still holds BombeCam rules is an error the UI shows.
func TestPrivacy_RouterReportIsChecked(t *testing.T) {
	_, _, pm, _, mux, _ := setupFullTestEnvironment(t)
	seedPrivacyProfile(t, pm, twoCams())
	residue, cameras := "0", ""
	stubRouter(t, func(c routerCall) (routerpush.Result, error) {
		res, err := okRouter("fw3")(c)
		if c.command == openwrt.GateApply || c.command == openwrt.GateStatus {
			n := len(c.specMACs())
			if c.command == openwrt.GateStatus {
				n = 0
			}
			if cameras != "" {
				res.Output = strings.Replace(res.Output, fmt.Sprintf("cameras=%d", n), "cameras="+cameras, 1)
			}
			if n == 0 {
				res.Output = strings.Replace(res.Output, "cameras=0", "cameras=0 residue="+residue, 1)
			}
			if c.command == openwrt.GateStatus {
				res.Output = "BOMBECAM_RESULT status=ok block=no firewall=fw3 cameras=0 residue=" + residue + " version=" + openwrt.ScriptVersion() + "\n"
				err = nil
			}
		}
		return res, err
	})
	doJSON(t, mux, http.MethodPost, "/api/v1/privacy/router/connect", map[string]any{"router_address": "192.168.8.1", "router_password": "pw"})
	doJSON(t, mux, http.MethodPost, "/api/v1/privacy/block-all", map[string]any{"blocked": true})

	// the router answers with another camera count (asked for 1)
	cameras = "2"
	code, resp := doJSON(t, mux, http.MethodPost, "/api/v1/privacy/camera", map[string]any{"camera_id": "cam-2", "blocked": false})
	if routerField(resp, "error") != "router_state_mismatch" || routerField(resp, "ok") != false {
		t.Fatalf("a different camera count must be reported: %d %+v", code, resp)
	}
	cameras = ""

	// unblocking the last camera, but rules are left in the firewall
	residue = "3"
	doJSON(t, mux, http.MethodPost, "/api/v1/privacy/camera", map[string]any{"camera_id": "cam-2", "blocked": false})
	code, resp = doJSON(t, mux, http.MethodPost, "/api/v1/privacy/camera", map[string]any{"camera_id": "cam-1", "blocked": false})
	if routerField(resp, "error") != "router_state_mismatch" || !strings.Contains(fmt.Sprint(routerField(resp, "message")), "still in its firewall") {
		t.Fatalf("rules left behind must be reported: %d %+v", code, resp)
	}
	if n := len(pm.GetProfile().Privacy.AppliedCameras); n == 0 {
		t.Error("a failed unblock must not be recorded as done")
	}

	// the Check router button reads the router's own report
	code, resp = doJSON(t, mux, http.MethodPost, "/api/v1/privacy/router/check", map[string]any{})
	if code == 200 || resp["error"] != "router_state_mismatch" {
		t.Fatalf("check with rules left: %d %+v", code, resp)
	}
	residue = "0"
	doJSON(t, mux, http.MethodPost, "/api/v1/privacy/camera", map[string]any{"camera_id": "cam-1", "blocked": false})
	if n := len(pm.GetProfile().Privacy.AppliedCameras); n != 0 {
		t.Fatalf("a clean unblock is recorded: %d cameras still applied", n)
	}
	code, resp = doJSON(t, mux, http.MethodPost, "/api/v1/privacy/router/check", map[string]any{})
	if code != 200 || resp["ok"] != true {
		t.Fatalf("check on a clean router: %d %+v", code, resp)
	}
}

// Blocking applied by a router script that predates connection clearing is
// reported, so connections opened before the block aren't silently left open.
func TestRouterApplyWarningsOldScript(t *testing.T) {
	old := openwrt.Result{Block: "yes", Fields: map[string]string{"status": "ok", "block": "yes", "cameras": "1"}}
	if w := routerApplyWarnings(old); len(w) != 1 || !strings.Contains(w[0], "Update router") {
		t.Errorf("old script: %v", w)
	}
	for _, c := range []string{"yes", "none", "no"} {
		r := openwrt.Result{Block: "yes", Fields: map[string]string{"cleared": c}}
		if w := routerApplyWarnings(r); len(w) != 0 {
			t.Errorf("cleared=%s: unexpected %v", c, w)
		}
	}
	if w := routerApplyWarnings(openwrt.Result{Block: "no", Fields: map[string]string{}}); len(w) != 0 {
		t.Errorf("unblocking: unexpected %v", w)
	}
	if s := routerReportSummary(openwrt.Result{Fields: map[string]string{"cameras": "1", "cleared": "yes", "version": "1.0.0"}}); !strings.Contains(s, "open connections cleared") {
		t.Errorf("summary %q", s)
	}
}

func TestPickRouterAddress(t *testing.T) {
	_, mainNet, _ := net.ParseCIDR("10.0.0.23/24")
	_, travelNet, _ := net.ParseCIDR("192.168.8.123/24")
	mainGW := defaultRoute{Gateway: net.ParseIP("10.0.0.1").To4(), Metric: 10}
	travelGW := defaultRoute{Gateway: net.ParseIP("192.168.8.1").To4(), Metric: 50}
	for _, tc := range []struct {
		name   string
		last   string
		cams   []string
		locals []*net.IPNet
		routes []defaultRoute
		want   routerSuggestion
	}{
		{"last router wins", "192.168.8.1:22", []string{"10.0.0.50"}, []*net.IPNet{mainNet}, []defaultRoute{mainGW}, routerSuggestion{"192.168.8.1", "last"}},
		// PC wired to the main router (lower metric) and on the travel
		// router's Wi-Fi, cameras on the travel router.
		{"cameras behind the travel router", "", []string{"192.168.8.140", "192.168.8.141"}, []*net.IPNet{mainNet, travelNet}, []defaultRoute{mainGW, travelGW}, routerSuggestion{"192.168.8.1", "cameras"}},
		{"camera network without a default route", "", []string{"192.168.8.140"}, []*net.IPNet{mainNet, travelNet}, []defaultRoute{mainGW}, routerSuggestion{"192.168.8.1", "cameras"}},
		{"camera outside the PC's networks", "", []string{"192.168.9.77"}, []*net.IPNet{mainNet}, []defaultRoute{mainGW}, routerSuggestion{"192.168.9.1", "cameras"}},
		{"most cameras decide", "", []string{"10.0.0.50", "192.168.8.140", "192.168.8.141"}, []*net.IPNet{mainNet, travelNet}, []defaultRoute{mainGW, travelGW}, routerSuggestion{"192.168.8.1", "cameras"}},
		{"no cameras yet", "", nil, []*net.IPNet{mainNet}, []defaultRoute{travelGW, mainGW}, routerSuggestion{"10.0.0.1", "default_gateway"}},
		{"public camera address ignored", "", []string{"8.8.8.8"}, []*net.IPNet{mainNet}, []defaultRoute{mainGW}, routerSuggestion{"10.0.0.1", "default_gateway"}},
		{"nothing known", "", nil, nil, nil, routerSuggestion{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := pickRouterAddress(tc.last, tc.cams, tc.locals, tc.routes); got != tc.want {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestParseLinuxDefaultRoutes(t *testing.T) {
	in := "Iface\tDestination\tGateway \tFlags\tRefCnt\tUse\tMetric\tMask\n" +
		"eth0\t00000000\t0100000A\t0003\t0\t0\t100\t00000000\n" +
		"wlan0\t00000000\t0108A8C0\t0003\t0\t0\t600\t00000000\n" +
		"wlan0\t0008A8C0\t00000000\t0001\t0\t0\t600\t00FFFFFF\n"
	got := parseLinuxDefaultRoutes(strings.NewReader(in))
	if len(got) != 2 || got[0].Gateway.String() != "10.0.0.1" || got[1].Gateway.String() != "192.168.8.1" || got[1].Metric != 600 {
		t.Fatalf("got %+v", got)
	}
}

// Disconnecting keeps the router's address for the next Connect router.
func TestForgetRouterKeepsLastAddress(t *testing.T) {
	_, _, pm, _, _, _ := setupFullTestEnvironment(t)
	_ = pm.Save(context.Background(), &profile.Profile{Version: profile.CurrentSchemaVersion,
		Credentials: profile.CloudCredentials{AccountEmail: "a@example.test"},
		Privacy:     profile.PrivacySettings{RouterAddress: "192.168.8.1:22", LastRouterAddress: "192.168.8.1:22", RouterConnected: true}})
	forgetRouter(context.Background(), pm, true)
	ps := pm.GetProfile().Privacy
	if ps.RouterAddress != "" || ps.RouterConnected || ps.LastRouterAddress != "192.168.8.1:22" {
		t.Fatalf("privacy after forget: %+v", ps)
	}
}

func TestAllowlistViewRegion(t *testing.T) {
	us := allowlistView(false, policy.RegionUS)
	if len(us) != 4 || us[0]["host"] != "mqtts02-us.osaio.net" {
		t.Fatalf("US allowlist = %+v", us)
	}
	eu := allowlistView(false, policy.RegionEU)
	if len(eu) != 5 {
		t.Fatalf("EU allowlist has %d rules, want 5: %+v", len(eu), eu)
	}
	if eu[0]["host"] != "mqtts-eu.osaio.net" || eu[1]["host"] != "mqtts01-eu.osaio.net" {
		t.Fatalf("EU control rules = %+v", eu[:2])
	}
	if eu[2]["host"] != "wss-eu.osaio.net" {
		t.Fatalf("EU stream setup rule = %+v", eu[2])
	}
}

func TestPrivacyRegionDefaults(t *testing.T) {
	if got := privacyRegion(profile.PrivacySettings{}); got != policy.RegionUS {
		t.Fatalf("empty region = %q, want us", got)
	}
	if got := privacyRegion(profile.PrivacySettings{Region: "EU"}); got != policy.RegionEU {
		t.Fatalf("EU region = %q", got)
	}
	if got := privacyRegion(profile.PrivacySettings{Region: "bogus"}); got != policy.RegionUS {
		t.Fatalf("bogus region = %q, want us", got)
	}
	if got := appliedRegion(profile.PrivacySettings{}); got != "us" {
		t.Fatalf("empty applied region = %q, want us", got)
	}
	if got := appliedRegion(profile.PrivacySettings{AppliedRegion: "eu"}); got != "eu" {
		t.Fatalf("applied region = %q", got)
	}
}
