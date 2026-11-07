package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/Fever-r/BombeCam/pkg/netstack"
	"github.com/Fever-r/BombeCam/pkg/policy"
)

func TestGetEnvOrDefault(t *testing.T) {
	key := "TEST_BOMBECAM_VAR_XYZ"
	os.Unsetenv(key)
	if val := getEnvOrDefault(key, "fallback"); val != "fallback" {
		t.Fatalf("expected fallback, got %q", val)
	}

	os.Setenv(key, "custom_val")
	defer os.Unsetenv(key)
	if val := getEnvOrDefault(key, "fallback"); val != "custom_val" {
		t.Fatalf("expected custom_val, got %q", val)
	}
}

func TestResolveNetworkParams(t *testing.T) {
	camIf := "eth_test"
	camSubnet := "10.99.0.0/24"
	gwIP := "10.99.0.1"
	wanIf := "wan_test"

	resolveNetworkParams(&camIf, &camSubnet, &gwIP, &wanIf)

	if camIf != "eth_test" || camSubnet != "10.99.0.0/24" || gwIP != "10.99.0.1" || wanIf != "wan_test" {
		t.Fatalf("explicit params were modified: if=%s, subnet=%s, gw=%s, wan=%s", camIf, camSubnet, gwIP, wanIf)
	}
}

func TestResolveNetworkParamsAutoDiscovery(t *testing.T) {
	var camIf, camSubnet, gwIP, wanIf string
	resolveNetworkParams(&camIf, &camSubnet, &gwIP, &wanIf)
	// On hosts with network interfaces, some or all may be discovered.
	// The important thing is it does not crash or inject hardcoded 192.168.30.
	if camSubnet == "192.168.30.0/24" || gwIP == "192.168.30.1" || camIf == "eth0.30" {
		t.Fatalf("hardcoded network parameters detected: if=%s, subnet=%s, gw=%s", camIf, camSubnet, gwIP)
	}
}

func TestGetEnvIntOrDefault(t *testing.T) {
	key := "TEST_BOMBECAM_INT_VAR_XYZ"
	os.Unsetenv(key)
	if val := getEnvIntOrDefault(key, 8653); val != 8653 {
		t.Fatalf("expected default 8653, got %d", val)
	}

	os.Setenv(key, "9999")
	if val := getEnvIntOrDefault(key, 8653); val != 9999 {
		t.Fatalf("expected custom 9999, got %d", val)
	}

	os.Setenv(key, "not_an_int")
	if val := getEnvIntOrDefault(key, 8653); val != 8653 {
		t.Fatalf("expected fallback 8653 on invalid int, got %d", val)
	}
	os.Unsetenv(key)
}

func TestAPIHostAndPortDefaults(t *testing.T) {
	os.Unsetenv("BOMBECAM_NET_API_HOST")
	os.Unsetenv("BOMBECAM_NET_API_PORT")

	defaultHost := getEnvOrDefault("BOMBECAM_NET_API_HOST", "0.0.0.0")
	if defaultHost != "0.0.0.0" {
		t.Fatalf("expected default host '0.0.0.0', got %q", defaultHost)
	}

	defaultPort := getEnvIntOrDefault("BOMBECAM_NET_API_PORT", 8653)
	if defaultPort != 8653 {
		t.Fatalf("expected default port 8653, got %d", defaultPort)
	}

	os.Setenv("BOMBECAM_NET_API_HOST", "192.168.1.1")
	os.Setenv("BOMBECAM_NET_API_PORT", "9653")
	defer func() {
		os.Unsetenv("BOMBECAM_NET_API_HOST")
		os.Unsetenv("BOMBECAM_NET_API_PORT")
	}()

	customHost := getEnvOrDefault("BOMBECAM_NET_API_HOST", "0.0.0.0")
	if customHost != "192.168.1.1" {
		t.Fatalf("expected custom host '192.168.1.1', got %q", customHost)
	}

	customPort := getEnvIntOrDefault("BOMBECAM_NET_API_PORT", 8653)
	if customPort != 9653 {
		t.Fatalf("expected custom port 9653, got %d", customPort)
	}
}

func TestParseYesNo(t *testing.T) {
	for in, want := range map[string]bool{"yes": true, "Y": true, "on": true, "1": true, "no": false, "OFF": false, "0": false} {
		got, err := parseYesNo(in)
		if err != nil || got != want {
			t.Errorf("parseYesNo(%q) = %v, %v", in, got, err)
		}
	}
	if _, err := parseYesNo("maybe"); err == nil {
		t.Error("maybe accepted")
	}
}

func TestCameraEntries(t *testing.T) {
	os.Setenv("CAMERA_MAC", "Test Camera=aa:bb:cc:dd:ee:01, aa:bb:cc:dd:ee:02")
	defer os.Unsetenv("CAMERA_MAC")
	got := cameraEntries(nil)
	if len(got) != 2 || got[0] != "Test Camera=aa:bb:cc:dd:ee:01" {
		t.Fatalf("got %v", got)
	}
	if got := cameraEntries([]string{"x=aa:bb:cc:dd:ee:03"}); len(got) != 1 {
		t.Fatalf("flags must win over env: %v", got)
	}
}

func TestConfigRequestMerge(t *testing.T) {
	cur := configValues{
		Block:            false,
		Cameras:          []string{"aa:bb:cc:dd:ee:01"},
		Region:           policy.RegionUS,
		BlockStreamSetup: false,
	}
	// Omitted fields keep their current value.
	got, err := configRequest{}.merge(cur)
	if err != nil {
		t.Fatal(err)
	}
	if got.Block || got.Region != policy.RegionUS || got.BlockStreamSetup ||
		len(got.Cameras) != 1 || got.Cameras[0] != "aa:bb:cc:dd:ee:01" {
		t.Fatalf("omitted fields changed: %+v", got)
	}

	block := true
	region := "eu"
	setup := true
	cameras := []string{"AA:BB:CC:DD:EE:02"}
	got, err = configRequest{
		BlockCloudVideo:  &block,
		Cameras:          &cameras,
		Region:           &region,
		BlockStreamSetup: &setup,
	}.merge(cur)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Block || got.Region != policy.RegionEU || !got.BlockStreamSetup || got.Cameras[0] != "AA:BB:CC:DD:EE:02" {
		t.Fatalf("request not applied: %+v", got)
	}

	if _, err := (configRequest{Region: strPtr("mars")}).merge(cur); err == nil {
		t.Fatal("invalid region accepted")
	}
	empty := []string{}
	// With blocking off there is nothing to protect, so an empty list keeps
	// the current one (the gateway sends it when no camera is chosen).
	got, err = configRequest{Cameras: &empty}.merge(cur)
	if err != nil {
		t.Fatalf("empty camera list with blocking off: %v", err)
	}
	if len(got.Cameras) != 1 || got.Cameras[0] != "aa:bb:cc:dd:ee:01" {
		t.Fatalf("empty list must keep the current cameras: %+v", got)
	}
	on := true
	if _, err := (configRequest{BlockCloudVideo: &on, Cameras: &empty}).merge(cur); err == nil {
		t.Fatal("empty camera list accepted while blocking is on")
	}
}

func TestHandleNetConfig(t *testing.T) {
	newTestManager := func() *netstack.RulesetManager {
		m, err := netstack.NewRulesetManager(netstack.RulesetConfig{
			Cameras: []string{"Front=aa:bb:cc:dd:ee:01"},
			Region:  policy.RegionUS,
		})
		if err != nil {
			t.Fatal(err)
		}
		return m
	}

	t.Run("invalid region is a request error", func(t *testing.T) {
		m := newTestManager()
		applied := false
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/v1/net/config",
			strings.NewReader(`{"region":"mars"}`))
		handleNetConfig(rec, req, m, &sync.Mutex{}, func(bool) error {
			applied = true
			return nil
		}, func() map[string]any { return statusPayload(m) })

		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400 (body %s)", rec.Code, rec.Body)
		}
		if applied {
			t.Fatal("apply ran for an invalid request")
		}
		var body map[string]string
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if body["error"] != "invalid_request" || body["message"] == "" {
			t.Fatalf("body = %v", body)
		}
		if m.Region() != policy.RegionUS {
			t.Fatalf("failed request changed the region to %s", m.Region())
		}
	})

	t.Run("malformed MAC is a request error", func(t *testing.T) {
		m := newTestManager()
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/v1/net/config",
			strings.NewReader(`{"cameras":["not-a-mac"]}`))
		handleNetConfig(rec, req, m, &sync.Mutex{}, func(bool) error {
			t.Fatal("apply ran for an invalid request")
			return nil
		}, func() map[string]any { return statusPayload(m) })

		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400 (body %s)", rec.Code, rec.Body)
		}
	})

	t.Run("valid request reconfigures and applies", func(t *testing.T) {
		m := newTestManager()
		var appliedBlock bool
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/v1/net/config",
			strings.NewReader(`{"region":"eu","cameras":["AA:BB:CC:DD:EE:02"],"block_stream_setup":true}`))
		handleNetConfig(rec, req, m, &sync.Mutex{}, func(block bool) error {
			appliedBlock = block
			return nil
		}, func() map[string]any { return statusPayload(m) })

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body)
		}
		if appliedBlock {
			t.Fatal("apply did not keep the current block setting")
		}
		if m.Region() != policy.RegionEU || !m.BlockStreamSetupEnabled() {
			t.Fatalf("manager not reconfigured: region=%s streamSetup=%v", m.Region(), m.BlockStreamSetupEnabled())
		}
		if got := m.CameraMACs(); len(got) != 1 || got[0] != "aa:bb:cc:dd:ee:02" {
			t.Fatalf("cameras = %v", got)
		}
		var body map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if body["region"] != "eu" || body["block_stream_setup"] != true {
			t.Fatalf("status payload = %v", body)
		}
		if cams, _ := body["cameras"].([]any); len(cams) != 1 || cams[0] != "aa:bb:cc:dd:ee:02" {
			t.Fatalf("status cameras = %v", body["cameras"])
		}
	})

	t.Run("apply failure is a server error", func(t *testing.T) {
		m := newTestManager()
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/v1/net/config",
			strings.NewReader(`{"block_cloud_video":true}`))
		handleNetConfig(rec, req, m, &sync.Mutex{}, func(bool) error {
			return os.ErrPermission
		}, func() map[string]any { return statusPayload(m) })

		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500 (body %s)", rec.Code, rec.Body)
		}
		var body map[string]string
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if body["error"] != "apply_failed" {
			t.Fatalf("body = %v", body)
		}
	})
}

func TestStatusPayloadIncludesConfig(t *testing.T) {
	m, err := netstack.NewRulesetManager(netstack.RulesetConfig{
		Cameras:          []string{"Back=AA:BB:CC:DD:EE:03"},
		Region:           policy.RegionEU,
		BlockStreamSetup: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	got := statusPayload(m)
	if got["region"] != "eu" || got["block_stream_setup"] != true {
		t.Fatalf("payload = %v", got)
	}
	if cams, _ := got["cameras"].([]string); len(cams) != 1 || cams[0] != "aa:bb:cc:dd:ee:03" {
		t.Fatalf("cameras = %v", got["cameras"])
	}
}

func strPtr(s string) *string { return &s }
