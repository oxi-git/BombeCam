package netlocal

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestStatusParsesPayload(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/net/status" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"block_cloud_video":true,"applied":true,"ruleset_hash":"abc123",
			"region":"eu","cameras":["AA:BB:CC:DD:EE:01"],"block_stream_setup":true,
			"allowed_addresses":{"mqtts-eu.osaio.net":["1.2.3.4"]},"headline":"Camera access is limited","os":"linux"}`))
	}))
	defer srv.Close()

	st, err := New(srv.URL, "").Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !st.BlockCloudVideo || !st.Applied || st.RulesetHash != "abc123" || st.Region != "eu" ||
		!st.BlockStreamSetup || st.Headline != "Camera access is limited" || st.OS != "linux" {
		t.Fatalf("status = %+v", st)
	}
	if len(st.Cameras) != 1 || st.Cameras[0] != "AA:BB:CC:DD:EE:01" {
		t.Fatalf("cameras = %v", st.Cameras)
	}
	if got := st.AllowedAddresses["mqtts-eu.osaio.net"]; len(got) != 1 || got[0] != "1.2.3.4" {
		t.Fatalf("allowed_addresses = %v", st.AllowedAddresses)
	}
}

func TestApplySendsConfigAndBearer(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/net/config" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		if h := r.Header.Get("Authorization"); h != "Bearer secret-token" {
			t.Errorf("Authorization = %q", h)
		}
		if c := r.Header.Get("Content-Type"); c != "application/json" {
			t.Errorf("Content-Type = %q", c)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"block_cloud_video":true}`))
	}))
	defer srv.Close()

	err := New(srv.URL, "secret-token").Apply(context.Background(), true,
		[]string{"aa:bb:cc:dd:ee:01", "aa:bb:cc:dd:ee:02"}, "eu", true)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 4 {
		t.Fatalf("body has %d fields: %v", len(got), got)
	}
	if got["block_cloud_video"] != true || got["region"] != "eu" || got["block_stream_setup"] != true {
		t.Fatalf("body = %v", got)
	}
	cams, _ := got["cameras"].([]any)
	if len(cams) != 2 || cams[0] != "aa:bb:cc:dd:ee:01" || cams[1] != "aa:bb:cc:dd:ee:02" {
		t.Fatalf("cameras = %v", got["cameras"])
	}
}

func TestApplyWithNoCamerasSendsEmptyList(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	if err := New(srv.URL, "").Apply(context.Background(), false, nil, "us", false); err != nil {
		t.Fatal(err)
	}
	if cams, ok := got["cameras"].([]any); !ok || len(cams) != 0 {
		t.Fatalf("cameras = %v (want an empty list)", got["cameras"])
	}
}

func TestNon2xxReturnsErrorWithBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("iptables refused the rule"))
	}))
	defer srv.Close()
	c := New(srv.URL, "")

	err := c.Apply(context.Background(), true, []string{"aa:bb:cc:dd:ee:01"}, "us", false)
	if err == nil || !strings.Contains(err.Error(), "502") || !strings.Contains(err.Error(), "iptables refused the rule") {
		t.Fatalf("Apply error = %v", err)
	}
	if _, err := c.Status(context.Background()); err == nil || !strings.Contains(err.Error(), "502") {
		t.Fatalf("Status error = %v", err)
	}
}

func TestLongErrorBodyIsTruncated(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(strings.Repeat("x", 2000)))
	}))
	defer srv.Close()

	_, err := New(srv.URL, "").Status(context.Background())
	if err == nil {
		t.Fatal("expected an error")
	}
	if len(err.Error()) > 600 {
		t.Fatalf("error body was not truncated (%d bytes)", len(err.Error()))
	}
}

func TestNewFromEnv(t *testing.T) {
	t.Setenv(EnvURL, "")
	t.Setenv(EnvKey, "")
	defaults := NewFromEnv()
	if defaults.BaseURL != DefaultURL || defaults.APIKey != "" {
		t.Fatalf("defaults: %+v", defaults)
	}
	if defaults.http.Timeout != 3*time.Second {
		t.Fatalf("timeout = %v", defaults.http.Timeout)
	}
	if URLFromEnv() != DefaultURL {
		t.Fatalf("URLFromEnv = %q", URLFromEnv())
	}

	t.Setenv(EnvURL, "http://127.0.0.1:9876/")
	t.Setenv(EnvKey, " k ")
	got := NewFromEnv()
	if got.BaseURL != "http://127.0.0.1:9876" || got.APIKey != "k" {
		t.Fatalf("from env: %+v", got)
	}
	if URLFromEnv() != "http://127.0.0.1:9876" {
		t.Fatalf("URLFromEnv = %q", URLFromEnv())
	}
}
