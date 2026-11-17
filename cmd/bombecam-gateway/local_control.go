package main

import (
	"net/http"

	"github.com/Fever-r/BombeCam/pkg/bridge"
)

// isLocalControl reports whether commands go to the cameras through the local
// firewall backend rather than the vendor cloud.
func isLocalControl(cc bridge.ControlChannel) bool {
	_, ok := bridge.AsLocalControl(cc)
	return ok
}

func handleControlStatus(w http.ResponseWriter, r *http.Request, streamMgr *StreamManager) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	cc := GatewayControlChannel()
	if local, ok := bridge.AsLocalControl(cc); ok {
		writeJSON(w, http.StatusOK, map[string]any{
			"test_mode":         streamMgr.LocalControlTest(),
			"control":           local.LocalStatus(r.URL.Query().Get("camera_id")),
			"firewall_verified": false,
		})
		return
	}
	route := "cloud"
	if cc == nil {
		route = "unavailable"
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"test_mode": false,
		"control":   map[string]any{"route": route},
	})
}
