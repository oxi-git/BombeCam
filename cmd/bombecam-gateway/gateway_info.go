package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"github.com/Fever-r/BombeCam/internal/version"
	"net/http"
	"os"
	"runtime"
	"sync/atomic"
)

// gatewayHeadless is set when BombeCam runs without a desktop (Docker, or
// -headless): no browser, no tray, and in Docker "Shut down" would only make
// the container restart (restart: unless-stopped).
var gatewayHeadless atomic.Bool

var errAutostartUnsupported = errors.New("start with Windows is only available on Windows")

// handleGatewayInfo serves GET /api/v1/gateway/info: what the page needs to
// show the right Settings options.
func handleGatewayInfo(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET required", http.StatusMethodNotAllowed)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"version":   version.Version,
		"platform":  runtime.GOOS,
		"headless":  gatewayHeadless.Load(),
		"docker":    inContainer(),
		"autostart": autostartInfo(r),
	})
}

// inContainer reports whether BombeCam runs in Docker, where the compose
// file restarts it (restart: unless-stopped): Shut down would only restart it.
func inContainer() bool {
	if runtime.GOOS != "linux" {
		return false
	}
	if v := os.Getenv("BOMBECAM_IN_DOCKER"); v != "" {
		return v == "1" || v == "true" || v == "yes"
	}
	_, err := os.Stat("/.dockerenv")
	return err == nil
}

func autostartInfo(r *http.Request) map[string]any {
	info := map[string]any{"supported": autostartSupported() && !gatewayHeadless.Load()}
	if info["supported"] == false {
		return info
	}
	enabled, thisCopy, command := autostartStatus()
	info["enabled"] = enabled
	info["this_copy"] = thisCopy
	// the command holds the Windows user's folder name: this PC only
	if enabled && !thisCopy && isLoopbackRequest(r) {
		info["other_command"] = command
	}
	return info
}

// handleAutostart serves GET/POST /api/v1/gateway/autostart {"enabled": bool}.
// Changing it needs a request from this PC or operator authentication, and a
// browser session must send its CSRF token.
func handleAutostart(w http.ResponseWriter, r *http.Request, streamMgr *StreamManager) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, autostartInfo(r))
		return
	case http.MethodPost:
	default:
		http.Error(w, "GET or POST required", http.StatusMethodNotAllowed)
		return
	}
	if !autostartSupported() || gatewayHeadless.Load() {
		writeJSON(w, http.StatusNotImplemented, map[string]any{"error": errAutostartUnsupported.Error()})
		return
	}
	var body struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid json payload"})
		return
	}
	if err := setAutostart(body.Enabled); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": fmt.Sprintf("could not change Start with Windows: %v", err)})
		return
	}
	fmt.Printf("[startup] start with Windows: %v\n", body.Enabled)
	writeJSON(w, http.StatusOK, autostartInfo(r))
}
