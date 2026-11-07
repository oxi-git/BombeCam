package main

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"

	"github.com/Fever-r/BombeCam/pkg/netlocal"
	"github.com/Fever-r/BombeCam/pkg/profile"
)

type localApply struct {
	block            bool
	cameras          []string
	region           string
	blockStreamSetup bool
}

// fakeLocalFirewall stands in for bombecam-net. Apply updates its status the
// way the real daemon does, so the gateway's verify-after-apply step passes.
type fakeLocalFirewall struct {
	mu        sync.Mutex
	status    netlocal.Status
	statusErr error
	applyErr  error
	applies   []localApply
}

func (f *fakeLocalFirewall) Status(context.Context) (netlocal.Status, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.statusErr != nil {
		return netlocal.Status{}, f.statusErr
	}
	return f.status, nil
}

func (f *fakeLocalFirewall) Apply(_ context.Context, block bool, cameras []string, region string, blockStreamSetup bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.applies = append(f.applies, localApply{
		block: block, cameras: append([]string(nil), cameras...),
		region: region, blockStreamSetup: blockStreamSetup,
	})
	if f.applyErr != nil {
		return f.applyErr
	}
	st := f.status
	st.BlockCloudVideo = block
	st.Applied = true
	st.Cameras = append([]string(nil), cameras...)
	st.Region = region
	st.BlockStreamSetup = blockStreamSetup
	f.status = st
	return nil
}

func (f *fakeLocalFirewall) applied() []localApply {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]localApply(nil), f.applies...)
}

// stubLocalFirewall replaces the client factory for one test.
func stubLocalFirewall(t *testing.T, f *fakeLocalFirewall) *fakeLocalFirewall {
	t.Helper()
	orig := newLocalFirewallClient
	newLocalFirewallClient = func() localFirewallClient { return f }
	recordRouterOutcome(routerOutcome{OK: true}, false)
	t.Cleanup(func() {
		newLocalFirewallClient = orig
		recordRouterOutcome(routerOutcome{OK: true}, false)
	})
	return f
}

func seedLocalPrivacy(t *testing.T, pm profile.ProfileManager, region string, blockNew bool) {
	t.Helper()
	_, _ = pm.Update(context.Background(), func(p *profile.Profile) error {
		p.Privacy.Backend = profile.FirewallBackendLocal
		p.Privacy.Region = region
		p.Privacy.BlockNewCameras = blockNew
		return nil
	})
}

func TestPrivacyLocalInSync(t *testing.T) {
	_, _, pm, _, mux, _ := setupFullTestEnvironment(t)
	seedPrivacyProfile(t, pm, testCamWithMAC)
	seedLocalPrivacy(t, pm, "eu", true)
	f := stubLocalFirewall(t, &fakeLocalFirewall{status: netlocal.Status{
		BlockCloudVideo: true, Applied: true, Region: "eu",
		Cameras: []string{"AA-BB-CC-DD-EE-01"},
	}})

	code, st := doJSON(t, mux, http.MethodGet, "/api/v1/privacy", nil)
	if code != http.StatusOK {
		t.Fatalf("status: %d", code)
	}
	if st["backend"] != profile.FirewallBackendLocal || st["in_sync"] != true || st["needs_reapply"] != false {
		t.Fatalf("in sync status = %+v", st)
	}
	if camStates(st)["cam-1"] != camStateBlocked {
		t.Fatalf("camera state = %v", camStates(st)["cam-1"])
	}
	local, _ := st["local"].(map[string]any)
	if local["reachable"] != true || local["region"] != "eu" {
		t.Fatalf("local = %+v", local)
	}
	if n := len(f.applied()); n != 0 {
		t.Fatalf("an in-sync status must not apply anything (%d applies)", n)
	}
	if st["manual_command"] != nil || st["manual_remove_command"] != nil {
		t.Fatalf("the router command must not be offered for local: %+v", st)
	}
}

func TestPrivacyLocalOutOfSync(t *testing.T) {
	_, _, pm, _, mux, _ := setupFullTestEnvironment(t)
	seedPrivacyProfile(t, pm, testCamWithMAC)
	seedLocalPrivacy(t, pm, "eu", true)
	stubLocalFirewall(t, &fakeLocalFirewall{status: netlocal.Status{
		BlockCloudVideo: true, Applied: true, Region: "eu",
		Cameras: []string{"aa:bb:cc:dd:ee:99"},
	}})

	_, st := doJSON(t, mux, http.MethodGet, "/api/v1/privacy", nil)
	if st["in_sync"] != false || st["needs_reapply"] != true {
		t.Fatalf("out of sync status = %+v", st)
	}
	if camStates(st)["cam-1"] != camStatePending {
		t.Fatalf("camera state = %v", camStates(st)["cam-1"])
	}
	local, _ := st["local"].(map[string]any)
	if local["reachable"] != true {
		t.Fatalf("local = %+v", local)
	}
}

func TestPrivacyLocalUnreachable(t *testing.T) {
	_, _, pm, _, mux, _ := setupFullTestEnvironment(t)
	seedPrivacyProfile(t, pm, testCamWithMAC)
	seedLocalPrivacy(t, pm, "eu", true)
	stubLocalFirewall(t, &fakeLocalFirewall{statusErr: errors.New("dial tcp 127.0.0.1:8653: connection refused")})

	code, st := doJSON(t, mux, http.MethodGet, "/api/v1/privacy", nil)
	if code != http.StatusOK {
		t.Fatalf("status: %d", code)
	}
	if st["in_sync"] != false || st["backend"] != profile.FirewallBackendLocal {
		t.Fatalf("unreachable status = %+v", st)
	}
	local, _ := st["local"].(map[string]any)
	if local["reachable"] != false {
		t.Fatalf("local = %+v", local)
	}
	if camStates(st)["cam-1"] != camStateNotApplied {
		t.Fatalf("camera state = %v", camStates(st)["cam-1"])
	}
	last, _ := st["last_error"].(map[string]any)
	if last["error"] != "local_firewall_unreachable" {
		t.Fatalf("last_error = %+v", st["last_error"])
	}

	// A change while it is down reports it and keeps the choice.
	code, resp := doJSON(t, mux, http.MethodPost, "/api/v1/privacy/block-all", map[string]any{"blocked": true})
	if code != http.StatusOK {
		t.Fatalf("block-all: %d", code)
	}
	if routerField(resp, "error") != "local_firewall_unreachable" || !pm.GetProfile().Privacy.BlockNewCameras {
		t.Fatalf("block-all while unreachable = %+v", resp)
	}
}

func TestPrivacyBackendSwitch(t *testing.T) {
	_, _, pm, _, mux, _ := setupFullTestEnvironment(t)
	seedPrivacyProfile(t, pm, testCamWithMAC)
	_, _ = pm.Update(context.Background(), func(p *profile.Profile) error {
		p.Privacy.BlockNewCameras = true
		return nil
	})
	f := stubLocalFirewall(t, &fakeLocalFirewall{status: netlocal.Status{}})

	code, resp := doJSON(t, mux, http.MethodPost, "/api/v1/privacy/backend", map[string]any{"backend": "local"})
	if code != http.StatusOK || resp["ok"] != true || resp["backend"] != profile.FirewallBackendLocal {
		t.Fatalf("switch to local: %d %+v", code, resp)
	}
	prof := pm.GetProfile()
	if prof.Privacy.Backend != profile.FirewallBackendLocal {
		t.Fatalf("backend not saved: %+v", prof.Privacy)
	}
	applies := f.applied()
	if len(applies) != 1 || !applies[0].block || applies[0].region != "us" ||
		len(applies[0].cameras) != 1 || applies[0].cameras[0] != "aa:bb:cc:dd:ee:01" {
		t.Fatalf("applies = %+v", applies)
	}
	if st, _ := resp["status"].(map[string]any); st["backend"] != profile.FirewallBackendLocal || st["in_sync"] != true {
		t.Fatalf("status after switch = %+v", st)
	}

	// An unknown backend is refused and changes nothing.
	code, resp = doJSON(t, mux, http.MethodPost, "/api/v1/privacy/backend", map[string]any{"backend": "cloud"})
	if code != http.StatusBadRequest || resp["error"] != "invalid_backend" {
		t.Fatalf("invalid backend: %d %+v", code, resp)
	}
	if pm.GetProfile().Privacy.Backend != profile.FirewallBackendLocal {
		t.Fatal("a rejected backend must not be saved")
	}
}

func TestPrivacyLocalBlockAll(t *testing.T) {
	_, _, pm, _, mux, _ := setupFullTestEnvironment(t)
	seedPrivacyProfile(t, pm, testCamWithMAC)
	seedLocalPrivacy(t, pm, "us", false)
	f := stubLocalFirewall(t, &fakeLocalFirewall{status: netlocal.Status{}})

	code, resp := doJSON(t, mux, http.MethodPost, "/api/v1/privacy/block-all", map[string]any{"blocked": true})
	if code != http.StatusOK || resp["ok"] != true || routerField(resp, "ok") != true {
		t.Fatalf("block-all: %d %+v", code, resp)
	}
	applies := f.applied()
	if len(applies) != 1 {
		t.Fatalf("applies = %+v", applies)
	}
	a := applies[0]
	if !a.block || a.region != "us" || a.blockStreamSetup {
		t.Fatalf("apply = %+v", a)
	}
	if len(a.cameras) != 1 || a.cameras[0] != "aa:bb:cc:dd:ee:01" {
		t.Fatalf("cameras = %v (want canonical)", a.cameras)
	}
	st, _ := resp["status"].(map[string]any)
	if st["in_sync"] != true || camStates(st)["cam-1"] != camStateBlocked {
		t.Fatalf("status after block-all = %+v", st)
	}
}

// Unblocking every camera sends an empty camera list with blocking off; the
// daemon keeps its list for when blocking comes back on, and the gateway must
// still see the result as in sync.
func TestPrivacyLocalUnblockAll(t *testing.T) {
	_, _, pm, _, mux, _ := setupFullTestEnvironment(t)
	seedPrivacyProfile(t, pm, testCamWithMAC)
	seedLocalPrivacy(t, pm, "us", true)
	f := stubLocalFirewall(t, &fakeLocalFirewall{status: netlocal.Status{
		BlockCloudVideo: true, Applied: true, Region: "us",
		Cameras: []string{"aa:bb:cc:dd:ee:01"},
	}})

	code, resp := doJSON(t, mux, http.MethodPost, "/api/v1/privacy/block-all", map[string]any{"blocked": false})
	if code != http.StatusOK || routerField(resp, "ok") != true {
		t.Fatalf("unblock all: %d %+v", code, resp)
	}
	applies := f.applied()
	if len(applies) != 1 || applies[0].block || len(applies[0].cameras) != 0 {
		t.Fatalf("unblock all applies = %+v", applies)
	}
	if st, _ := resp["status"].(map[string]any); st["in_sync"] != true {
		t.Fatalf("status after unblock = %+v", st)
	}
}

// A backend that cannot apply the choices yet is still saved: the request
// succeeded, and ok/message carry the apply outcome.
func TestPrivacyBackendSwitchLocalUnreachableSavesChoice(t *testing.T) {
	_, _, pm, _, mux, _ := setupFullTestEnvironment(t)
	seedPrivacyProfile(t, pm, testCamWithMAC)
	stubLocalFirewall(t, &fakeLocalFirewall{statusErr: errors.New("connection refused")})

	code, resp := doJSON(t, mux, http.MethodPost, "/api/v1/privacy/backend", map[string]any{"backend": "local"})
	if code != http.StatusOK || resp["ok"] != false || resp["error"] != "local_firewall_unreachable" {
		t.Fatalf("switch with daemon down: %d %+v", code, resp)
	}
	if pm.GetProfile().Privacy.Backend != profile.FirewallBackendLocal {
		t.Fatal("the chosen backend must be saved even when it cannot apply yet")
	}
}

// The Osaio region is a setting of its own, so a local backend can be moved
// to EU without a router connection.
func TestPrivacyRegionChangeAppliesLocal(t *testing.T) {
	_, _, pm, _, mux, _ := setupFullTestEnvironment(t)
	seedPrivacyProfile(t, pm, testCamWithMAC)
	seedLocalPrivacy(t, pm, "us", true)
	f := stubLocalFirewall(t, &fakeLocalFirewall{status: netlocal.Status{
		BlockCloudVideo: true, Applied: true, Region: "us",
		Cameras: []string{"aa:bb:cc:dd:ee:01"},
	}})

	code, resp := doJSON(t, mux, http.MethodPost, "/api/v1/privacy/region", map[string]any{"region": "eu"})
	if code != http.StatusOK || resp["ok"] != true || resp["region"] != "eu" {
		t.Fatalf("region change: %d %+v", code, resp)
	}
	applies := f.applied()
	if len(applies) != 1 || applies[0].region != "eu" {
		t.Fatalf("applies = %+v", applies)
	}
	if pm.GetProfile().Privacy.Region != "eu" {
		t.Fatalf("region not saved: %+v", pm.GetProfile().Privacy.Region)
	}
	if code, _ := doJSON(t, mux, http.MethodPost, "/api/v1/privacy/region", map[string]any{"region": "mars"}); code != http.StatusBadRequest {
		t.Fatalf("invalid region accepted: %d", code)
	}
}

// A profile that never chose a region follows the daemon's, so a daemon
// already configured for EU is not moved to the default US.
func TestPrivacyLocalAdoptsDaemonRegion(t *testing.T) {
	_, _, pm, _, mux, _ := setupFullTestEnvironment(t)
	seedPrivacyProfile(t, pm, testCamWithMAC)
	_, _ = pm.Update(context.Background(), func(p *profile.Profile) error {
		p.Privacy.Backend = profile.FirewallBackendLocal
		p.Privacy.BlockNewCameras = true
		return nil
	})
	f := stubLocalFirewall(t, &fakeLocalFirewall{status: netlocal.Status{
		BlockCloudVideo: true, Applied: true, Region: "eu",
		Cameras: []string{"aa:bb:cc:dd:ee:01"},
	}})

	// Before any sync the status already reports the daemon's region and
	// counts the choices as in sync.
	_, st := doJSON(t, mux, http.MethodGet, "/api/v1/privacy", nil)
	if st["region"] != "eu" || st["in_sync"] != true {
		t.Fatalf("status must adopt the daemon region: %+v", st)
	}

	code, resp := doJSON(t, mux, http.MethodPost, "/api/v1/privacy/backend", map[string]any{"backend": "local"})
	if code != http.StatusOK || resp["ok"] != true {
		t.Fatalf("switch to local: %d %+v", code, resp)
	}
	applies := f.applied()
	if len(applies) != 1 || applies[0].region != "eu" {
		t.Fatalf("applies = %+v", applies)
	}
	if pm.GetProfile().Privacy.Region != "eu" {
		t.Fatalf("adopted region not saved: %q", pm.GetProfile().Privacy.Region)
	}
}

// A router profile must not touch the local factory at all.
func TestPrivacyRouterDoesNotUseLocalFactory(t *testing.T) {
	_, _, pm, _, mux, _ := setupFullTestEnvironment(t)
	seedPrivacyProfile(t, pm, testCamWithMAC)
	orig := newLocalFirewallClient
	called := false
	newLocalFirewallClient = func() localFirewallClient {
		called = true
		return &fakeLocalFirewall{}
	}
	t.Cleanup(func() { newLocalFirewallClient = orig; recordRouterOutcome(routerOutcome{OK: true}, false) })

	doJSON(t, mux, http.MethodGet, "/api/v1/privacy", nil)
	if called {
		t.Fatal("the local firewall client must not be built for the router backend")
	}
}
