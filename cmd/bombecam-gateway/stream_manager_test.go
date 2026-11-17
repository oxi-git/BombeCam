package main

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Fever-r/BombeCam/pkg/bridge"
)

func TestStreamManager_EnrollmentValidation(t *testing.T) {
	sm := NewStreamManager(nil, "rtsp://127.0.0.1:8554", "http://127.0.0.1:8888", nil, false, "ffmpeg")

	inventory := []bridge.Device{
		{UUID: "cam-valid-01", Name: "Front Door", Type: "WS03", Online: 1},
		{UUID: "cam-valid-02", Name: "Backyard", Type: "WS03", Online: 1},
	}

	// 0. Enrolling empty camera IDs must fail with explicit error
	_, err := sm.Enroll([]string{}, inventory)
	if err == nil {
		t.Fatal("expected error when enrolling empty camera IDs, got nil")
	}
	if !strings.Contains(err.Error(), "no camera IDs provided") {
		t.Fatalf("expected error mentioning 'no camera IDs provided', got %v", err)
	}

	// 1. Enrolling an unknown ID must fail with explicit error
	_, err = sm.Enroll([]string{"cam-valid-01", "cam-unknown-99"}, inventory)
	if err == nil {
		t.Fatal("expected error when enrolling unknown camera ID, got nil")
	}
	if !strings.Contains(err.Error(), "cam-unknown-99") {
		t.Fatalf("expected error mentioning cam-unknown-99, got %v", err)
	}

	// 2. Enrolling valid IDs succeeds
	var runnerCalled atomic.Int32
	sm.SetRunner(func(ctx context.Context, mc *ManagedCamera, rtpPort int) {
		runnerCalled.Add(1)
		<-ctx.Done()
	})

	enrolled, err := sm.Enroll([]string{"cam-valid-01", "cam-valid-02"}, inventory)
	if err != nil {
		t.Fatalf("unexpected enrollment error: %v", err)
	}
	if len(enrolled) != 2 {
		t.Fatalf("expected 2 enrolled cameras, got %d", len(enrolled))
	}

	time.Sleep(20 * time.Millisecond)
	if runnerCalled.Load() != 2 {
		t.Fatalf("expected runner to be called 2 times, got %d", runnerCalled.Load())
	}

	// 3. Re-enrolling with only 1 camera removes the unenrolled one and cancels its context
	enrolled, err = sm.Enroll([]string{"cam-valid-01"}, inventory)
	if err != nil {
		t.Fatalf("unexpected re-enrollment error: %v", err)
	}
	if len(enrolled) != 1 || enrolled[0] != "cam-valid-01" {
		t.Fatalf("expected only cam-valid-01, got %v", enrolled)
	}

	if _, exists := sm.GetCamera("cam-valid-02"); exists {
		t.Fatal("expected cam-valid-02 to be unenrolled and removed")
	}
	if _, exists := sm.GetCamera("cam-valid-01"); !exists {
		t.Fatal("expected cam-valid-01 to remain enrolled")
	}
}

func TestStreamManager_LockoutVisibilityAndUnlock(t *testing.T) {
	sm := NewStreamManager(nil, "rtsp://127.0.0.1:8554", "http://127.0.0.1:8888", nil, false, "ffmpeg")
	sm.SetRunner(func(ctx context.Context, mc *ManagedCamera, rtpPort int) {
		<-ctx.Done()
	})

	inventory := []bridge.Device{
		{UUID: "cam-lockout-01", Name: "Garage", Type: "WS03", Online: 1},
	}

	_, err := sm.Enroll([]string{"cam-lockout-01"}, inventory)
	if err != nil {
		t.Fatalf("enroll error: %v", err)
	}

	mc, ok := sm.GetCamera("cam-lockout-01")
	if !ok {
		t.Fatal("expected camera to be found")
	}

	// Verify initial ACTIVE state with budget 3
	status := mc.GetStatus(nil)
	if status.RecoveryState != "ACTIVE" || status.RecoveryAttempts != 3 {
		t.Fatalf("expected ACTIVE with 3 attempts, got %s / %d", status.RecoveryState, status.RecoveryAttempts)
	}

	// Simulate 3 failures
	canRetry1 := mc.Session.RecordFailure()
	if !canRetry1 || mc.Session.RecoveryBudget != 2 || mc.Session.State != SessionStateRecovering {
		t.Fatalf("failure 1: expected retry=true, budget=2, state=RECOVERING; got retry=%v, budget=%d, state=%s", canRetry1, mc.Session.RecoveryBudget, mc.Session.State)
	}

	canRetry2 := mc.Session.RecordFailure()
	if !canRetry2 || mc.Session.RecoveryBudget != 1 || mc.Session.State != SessionStateRecovering {
		t.Fatalf("failure 2: expected retry=true, budget=1, state=RECOVERING; got retry=%v, budget=%d, state=%s", canRetry2, mc.Session.RecoveryBudget, mc.Session.State)
	}

	canRetry3 := mc.Session.RecordFailure()
	if canRetry3 || mc.Session.RecoveryBudget != 0 || mc.Session.State != SessionStateLockout {
		t.Fatalf("failure 3: expected retry=false, budget=0, state=LOCKOUT; got retry=%v, budget=%d, state=%s", canRetry3, mc.Session.RecoveryBudget, mc.Session.State)
	}

	// CRITICAL TEST: Camera in LOCKOUT MUST REMAIN VISIBLE in GetStatus and GetAllCameras!
	status = mc.GetStatus(nil)
	if status.RecoveryState != "LOCKOUT" {
		t.Fatalf("expected LOCKOUT state in status response, got %s", status.RecoveryState)
	}
	if status.RecoveryAttempts != 0 {
		t.Fatalf("expected 0 recovery attempts, got %d", status.RecoveryAttempts)
	}

	allCams := sm.GetAllCameras()
	if len(allCams) != 1 || allCams[0].UUID != "cam-lockout-01" {
		t.Fatalf("camera in LOCKOUT disappeared from GetAllCameras(): %+v", allCams)
	}

	// Test unlock
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	unlockedMC, err := sm.Unlock("cam-lockout-01")
	if err != nil {
		t.Fatalf("unlock failed: %v", err)
	}

	err = unlockedMC.Session.WaitUnlockContext(ctx)
	if err != nil {
		t.Fatalf("WaitUnlockContext failed after unlock: %v", err)
	}

	status = unlockedMC.GetStatus(nil)
	if status.RecoveryState != "ACTIVE" || status.RecoveryAttempts != 3 {
		t.Fatalf("expected ACTIVE with budget 3 after unlock, got %s / %d", status.RecoveryState, status.RecoveryAttempts)
	}
}

func TestStreamManager_StrictManualMode(t *testing.T) {
	sm := NewStreamManager(nil, "rtsp://127.0.0.1:8554", "http://127.0.0.1:8888", nil, false, "ffmpeg")
	inventory := []bridge.Device{
		{UUID: "cam-manual-01", Name: "Driveway", Type: "WS03", Online: 1},
	}

	_, _ = sm.Enroll([]string{"cam-manual-01"}, inventory)
	mc, _ := sm.GetCamera("cam-manual-01")

	if mc.GetStatus(nil).StrictManual != false {
		t.Fatal("expected strict_manual initially false")
	}

	err := sm.SetStrictManual("cam-manual-01", true)
	if err != nil {
		t.Fatalf("SetStrictManual failed: %v", err)
	}

	if mc.GetStatus(nil).StrictManual != true {
		t.Fatal("expected strict_manual to be true")
	}

	_ = sm.SetStrictManual("cam-manual-01", false)
	if mc.GetStatus(nil).StrictManual != false {
		t.Fatal("expected strict_manual to be false")
	}
}

// withLAN makes this machine look like it has the given addresses (the
// first with Default set is the default-route address) for the test.
func withLAN(t *testing.T, addrs ...LANAddress) {
	t.Helper()
	oldList, oldPrimary := lanAddressesFunc, primaryIPFunc
	primary := ""
	for _, a := range addrs {
		if a.Default {
			primary = a.IP
		}
	}
	lanAddressesFunc = func() []LANAddress { return append([]LANAddress(nil), addrs...) }
	primaryIPFunc = func() string { return primary }
	t.Cleanup(func() { lanAddressesFunc, primaryIPFunc = oldList, oldPrimary })
}

func reqWithHost(host string) *http.Request {
	if host == "" {
		return nil
	}
	req := httptest.NewRequest(http.MethodGet, "http://"+host+"/test", nil)
	req.Host = host
	return req
}

// The URLs handed to NVRs name an address other devices can reach, never
// 127.0.0.1 (unless this PC has no network address at all).
func TestStreamManager_AdvertisedAddress(t *testing.T) {
	cases := []struct {
		name     string
		lan      []LANAddress
		chosen   string
		host     string
		wantRTSP string
		wantHLS  string
		source   string
		missing  bool
	}{
		{name: "loopback only", host: "127.0.0.1:8654",
			wantRTSP: "rtsp://127.0.0.1:8554/cam-test-1", wantHLS: "http://127.0.0.1:8888/cam-test-1/index.m3u8", source: addrFromLoopback},
		{name: "single address, page on 127.0.0.1", lan: []LANAddress{{IP: "192.168.1.20", Interface: "Wi-Fi", Default: true}}, host: "127.0.0.1:8654",
			wantRTSP: "rtsp://192.168.1.20:8554/cam-test-1", wantHLS: "http://192.168.1.20:8888/cam-test-1/index.m3u8", source: addrFromDefault},
		{name: "single address, page on localhost", lan: []LANAddress{{IP: "192.168.1.20", Default: true}}, host: "localhost:8654",
			wantRTSP: "rtsp://192.168.1.20:8554/cam-test-1", source: addrFromDefault},
		{name: "single address, httptest host", lan: []LANAddress{{IP: "192.168.1.20", Default: true}}, host: "example.com",
			wantRTSP: "rtsp://192.168.1.20:8554/cam-test-1", source: addrFromDefault},
		{name: "single address, no request", lan: []LANAddress{{IP: "192.168.1.20", Default: true}},
			wantRTSP: "rtsp://192.168.1.20:8554/cam-test-1", source: addrFromDefault},
		{name: "two networks, automatic picks the default route",
			lan:  []LANAddress{{IP: "10.0.0.5", Interface: "Ethernet", Default: true}, {IP: "192.168.8.2", Interface: "Wi-Fi"}},
			host: "127.0.0.1:8654", wantRTSP: "rtsp://10.0.0.5:8554/cam-test-1", source: addrFromDefault},
		{name: "two networks, user picked the other one",
			lan:    []LANAddress{{IP: "10.0.0.5", Interface: "Ethernet", Default: true}, {IP: "192.168.8.2", Interface: "Wi-Fi"}},
			chosen: "192.168.8.2", host: "127.0.0.1:8654",
			wantRTSP: "rtsp://192.168.8.2:8554/cam-test-1", wantHLS: "http://192.168.8.2:8888/cam-test-1/index.m3u8", source: addrFromChoice},
		{name: "chosen address no longer on this PC falls back",
			lan:    []LANAddress{{IP: "10.0.0.7", Default: true}},
			chosen: "10.0.0.5", host: "127.0.0.1:8654", wantRTSP: "rtsp://10.0.0.7:8554/cam-test-1", source: addrFromDefault, missing: true},
		{name: "page opened on a LAN address", lan: []LANAddress{{IP: "10.0.0.5", Default: true}}, host: "192.168.1.50:8654",
			wantRTSP: "rtsp://192.168.1.50:8554/cam-test-1", wantHLS: "http://192.168.1.50:8888/cam-test-1/index.m3u8", source: addrFromPage},
		{name: "page opened on a host name", lan: []LANAddress{{IP: "10.0.0.5", Default: true}}, host: "nas.local:8654",
			wantRTSP: "rtsp://nas.local:8554/cam-test-1", source: addrFromPage},
		{name: "IPv6 page host", host: "[2001:db8::1]:8654",
			wantRTSP: "rtsp://[2001:db8::1]:8554/cam-test-1", source: addrFromPage},
		{name: "choice wins over the page host",
			lan:    []LANAddress{{IP: "10.0.0.5", Default: true}, {IP: "192.168.8.2"}},
			chosen: "192.168.8.2", host: "10.0.0.5:8654", wantRTSP: "rtsp://192.168.8.2:8554/cam-test-1", source: addrFromChoice},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			withLAN(t, tc.lan...)
			sm := NewStreamManager(nil, "rtsp://127.0.0.1:8554", "http://127.0.0.1:8888", nil, false, "ffmpeg")
			sm.SetNVRAddress(tc.chosen)
			req := reqWithHost(tc.host)
			if got := sm.ConsumerRTSPURL("cam-test-1", req); got != tc.wantRTSP {
				t.Errorf("RTSP %q, want %q", got, tc.wantRTSP)
			}
			if tc.wantHLS != "" {
				if got := sm.ConsumerHLSURL("cam-test-1", req); got != tc.wantHLS {
					t.Errorf("HLS %q, want %q", got, tc.wantHLS)
				}
			}
			if _, src, missing := sm.AdvertisedHost(req); src != tc.source || missing != tc.missing {
				t.Errorf("source %q missing %v, want %q %v", src, missing, tc.source, tc.missing)
			}
		})
	}
}

// BOMBECAM_RTSP_CONSUMER_BASE (-consumer-rtsp-base) still overrides everything.
func TestStreamManager_AdvertisedAddress_Override(t *testing.T) {
	withLAN(t, LANAddress{IP: "10.0.0.5", Default: true}, LANAddress{IP: "192.168.8.2"})
	sm := NewStreamManager(nil, "rtsp://mediamtx:8554", "http://mediamtx:8888", nil, false, "ffmpeg")
	sm.SetConsumerRTSPBase("rtsp://nvr.corp.local:9554")
	sm.SetConsumerHLSBase("http://nvr.corp.local:9888")
	sm.SetNVRAddress("192.168.8.2")

	req := reqWithHost("192.168.1.50:8080")
	if got := sm.ConsumerRTSPURL("cam-test-1", req); got != "rtsp://nvr.corp.local:9554/cam-test-1" {
		t.Errorf("RTSP %q", got)
	}
	if got := sm.ConsumerHLSURL("cam-test-1", req); got != "http://nvr.corp.local:9888/cam-test-1/index.m3u8" {
		t.Errorf("HLS %q", got)
	}
	// the internal publish target is untouched
	if sm.PublishRTSPBase() != "rtsp://mediamtx:8554" {
		t.Errorf("publish base %q", sm.PublishRTSPBase())
	}
}

// Friendly names, the camera-ID form, stream passwords and custom ports.
func TestStreamManager_AdvertisedURLs_NamesPortsPassword(t *testing.T) {
	withLAN(t, LANAddress{IP: "192.168.1.20", Default: true})
	sm := NewStreamManager(nil, "rtsp://127.0.0.1:9554", "http://127.0.0.1:9888", nil, false, "")
	id := "a1b2c3d4e5f60718293a4b5c6d7e8f90"
	sm.SetStreamNames(map[string]string{id: "test_camera"})
	if got := sm.ConsumerRTSPURL(id, nil); got != "rtsp://192.168.1.20:9554/test_camera" {
		t.Errorf("friendly RTSP %q", got)
	}
	if got := sm.ConsumerRTSPURLByID(id, nil); got != "rtsp://192.168.1.20:9554/"+id {
		t.Errorf("ID RTSP %q", got)
	}
	if got := sm.ConsumerHLSURL(id, nil); got != "http://192.168.1.20:9888/test_camera/index.m3u8" {
		t.Errorf("HLS %q", got)
	}
	sm.SetStreamCredentials("bombecam", "p@ss:word")
	if got := sm.ConsumerRTSPURL(id, nil); got != "rtsp://bombecam:p%40ss%3Aword@192.168.1.20:9554/test_camera" {
		t.Errorf("RTSP with password %q", got)
	}
	sm.SetConsumerRTSPBase("rtsp://nvr.lan:8554")
	if got := sm.ConsumerRTSPURL(id, nil); got != "rtsp://bombecam:p%40ss%3Aword@nvr.lan:8554/test_camera" {
		t.Errorf("override with password %q", got)
	}
	if got := sm.ViewerHLSURL(id); got != "/api/v1/cameras/"+id+"/hls/index.m3u8" {
		t.Errorf("viewer HLS %q", got)
	}
}

func TestUsableLANIP(t *testing.T) {
	for ip, want := range map[string]bool{
		"10.0.0.5": true, "192.168.8.2": true, "172.20.1.1": true, "100.101.102.103": true,
		"127.0.0.1": false, "169.254.3.4": false, "8.8.8.8": false, "0.0.0.0": false, "fe80::1": false, "2001:db8::1": false,
	} {
		if got := usableLANIP(net.ParseIP(ip)); got != want {
			t.Errorf("%s: %v, want %v", ip, got, want)
		}
	}
	a := []LANAddress{{IP: "192.168.8.2"}, {IP: "10.0.0.5", Default: true}, {IP: "172.20.0.1"}}
	sortLANAddresses(a)
	if a[0].IP != "10.0.0.5" || a[1].IP != "172.20.0.1" || a[2].IP != "192.168.8.2" {
		t.Errorf("order %v", a)
	}
}

func TestLockoutCooldown_CappedAtTwoMinutes(t *testing.T) {
	want := []time.Duration{30 * time.Second, time.Minute, 2 * time.Minute, 2 * time.Minute, 2 * time.Minute}
	for i, w := range want {
		if got := lockoutCooldown(i+1, DefaultRetryMaxWait); got != w {
			t.Errorf("round %d: wait %v, want %v", i+1, got, w)
		}
	}
	if got := lockoutCooldown(50, DefaultRetryMaxWait); got != 2*time.Minute {
		t.Errorf("round 50: wait %v, want the 2 minute cap", got)
	}
	if got := lockoutCooldown(0, 0); got != 30*time.Second {
		t.Errorf("defaults: %v", got)
	}
	// a custom cap (flag -retry-max-wait / BOMBECAM_RETRY_MAX_WAIT)
	if got := lockoutCooldown(9, 5*time.Minute); got != 5*time.Minute {
		t.Errorf("custom cap: %v", got)
	}
	if got := lockoutCooldown(1, 10*time.Second); got != 10*time.Second {
		t.Errorf("cap below the first step: %v", got)
	}
	sm := NewStreamManager(nil, "", "", nil, false, "")
	if sm.RetryMaxWait() != DefaultRetryMaxWait {
		t.Errorf("default max wait %v", sm.RetryMaxWait())
	}
	sm.SetRetryMaxWait(90 * time.Second)
	if sm.RetryMaxWait() != 90*time.Second {
		t.Errorf("max wait %v", sm.RetryMaxWait())
	}
}

// The Firewall tab finds a camera's MAC by the address its media comes
// from; the camera often names that address only in the SDP answer, so it is
// taken from the media path description.
func TestLanIPFromRemote(t *testing.T) {
	for in, want := range map[string]string{
		"192.168.8.23:44845 (host)": "192.168.8.23",
		"10.0.0.7:5000":             "10.0.0.7",
		"[fe80::1]:5000 (host)":     "",
		"garbage":                   "",
		"":                          "",
	} {
		if got := lanIPFromRemote(in); got != want {
			t.Errorf("%q: got %q, want %q", in, got, want)
		}
	}
}
