// Package netlocal is the gateway's client for the local firewall daemon
// (bombecam-net). On Home Assistant the daemon applies "Block cloud video"
// itself and serves its own state over a small HTTP API on the loopback
// address, so the gateway can read and change both without a router.
package netlocal

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// Environment variables that point the gateway at the local daemon.
const (
	EnvURL = "BOMBECAM_LOCAL_FIREWALL_URL"
	EnvKey = "BOMBECAM_LOCAL_FIREWALL_KEY"
	// DefaultURL is where bombecam-net listens when started without an
	// explicit address.
	DefaultURL = "http://127.0.0.1:8653"
)

// Status is the daemon's account of what it is applying right now.
type Status struct {
	BlockCloudVideo  bool                `json:"block_cloud_video"`
	Applied          bool                `json:"applied"`
	RulesetHash      string              `json:"ruleset_hash"`
	Region           string              `json:"region"`
	Cameras          []string            `json:"cameras"`
	BlockStreamSetup bool                `json:"block_stream_setup"`
	AllowedAddresses map[string][]string `json:"allowed_addresses"`
	Headline         string              `json:"headline"`
	OS               string              `json:"os"`
}

// Client talks to bombecam-net's local API.
type Client struct {
	// BaseURL is the daemon's address without a trailing slash.
	BaseURL string
	// APIKey is sent as a bearer token when the daemon was started with one.
	APIKey string
	http   *http.Client
}

// New builds a client. An empty baseURL means the daemon's default address.
func New(baseURL, apiKey string) *Client {
	baseURL = strings.TrimSpace(baseURL)
	if baseURL == "" {
		baseURL = DefaultURL
	}
	return &Client{
		BaseURL: strings.TrimRight(baseURL, "/"),
		APIKey:  strings.TrimSpace(apiKey),
		http:    &http.Client{Timeout: 3 * time.Second},
	}
}

// NewFromEnv builds a client from BOMBECAM_LOCAL_FIREWALL_URL and
// BOMBECAM_LOCAL_FIREWALL_KEY.
func NewFromEnv() *Client {
	return New(os.Getenv(EnvURL), os.Getenv(EnvKey))
}

// URLFromEnv is the daemon address the gateway would use, for messages and
// the status page. It never contacts the daemon.
func URLFromEnv() string {
	if u := strings.TrimSpace(os.Getenv(EnvURL)); u != "" {
		return strings.TrimRight(u, "/")
	}
	return DefaultURL
}

// Status reads the daemon's current state.
func (c *Client) Status(ctx context.Context) (Status, error) {
	var st Status
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+"/api/v1/net/status", nil)
	if err != nil {
		return st, err
	}
	c.authorize(req)
	resp, err := c.http.Do(req)
	if err != nil {
		return st, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return st, responseError(resp)
	}
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		return st, fmt.Errorf("bombecam-net status: %w", err)
	}
	return st, nil
}

// Apply sends the whole desired state to the daemon. A 2xx reply means the
// rules were applied; 4xx means the request was invalid, 5xx that applying
// failed.
func (c *Client) Apply(ctx context.Context, block bool, cameras []string, region string, blockStreamSetup bool) error {
	if cameras == nil {
		cameras = []string{}
	}
	body, err := json.Marshal(struct {
		BlockCloudVideo  bool     `json:"block_cloud_video"`
		Cameras          []string `json:"cameras"`
		Region           string   `json:"region"`
		BlockStreamSetup bool     `json:"block_stream_setup"`
	}{block, cameras, region, blockStreamSetup})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/api/v1/net/config", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	c.authorize(req)
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return responseError(resp)
	}
	// The reply repeats the status; the caller re-reads Status to check it.
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}

func (c *Client) authorize(req *http.Request) {
	if c.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.APIKey)
	}
}

// responseError keeps the daemon's own explanation, truncated so a large
// error page does not end up in the UI.
func responseError(resp *http.Response) error {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	msg := strings.TrimSpace(string(b))
	if msg == "" {
		return fmt.Errorf("bombecam-net returned %s", resp.Status)
	}
	return fmt.Errorf("bombecam-net returned %s: %s", resp.Status, msg)
}
