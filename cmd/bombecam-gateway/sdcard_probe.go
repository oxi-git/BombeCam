package main

import (
	"net/http"
)

// handleSDCardProbe (diagnostica temporanea) prova le varianti note dell'API
// Osaio per la SD card e riporta i codici di risposta. Serve a scegliere gli
// endpoint veri invece di indovinarli; da rimuovere una volta scelti.
func handleSDCardProbe(w http.ResponseWriter, r *http.Request, camID string, sessionMgr *SessionManager) {
	cloud := sessionsFor(sessionMgr).Template().Cloud()
	if cloud == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "not signed in"})
		return
	}
	probes := []struct {
		Path string
		Data map[string]any
	}{
		{"sdcard/info", map[string]any{"device_id": camID}},
		{"sdcard/capacity", map[string]any{"device_id": camID}},
		{"sdcard/format/status", map[string]any{"device_id": camID}},
		{"record/list", map[string]any{"device_id": camID, "start_time": 0, "end_time": 0, "page": 1, "per_page": 5}},
		{"record/sdcard/list", map[string]any{"device_id": camID, "start_time": 0, "end_time": 0, "page": 1, "per_page": 5}},
		{"sdcard/record/list", map[string]any{"device_id": camID, "start_time": 0, "end_time": 0, "page": 1, "per_page": 5}},
	}
	out := make([]map[string]any, 0, len(probes))
	for _, p := range probes {
		code, msg, keys, err := cloud.ProbePost(p.Path, p.Data)
		row := map[string]any{"path": "/v2/device/" + p.Path, "code": code, "msg": msg, "keys": keys}
		if err != nil {
			row["error"] = err.Error()
		}
		out = append(out, row)
	}
	writeJSON(w, http.StatusOK, map[string]any{"camera_id": camID, "probes": out})
}
