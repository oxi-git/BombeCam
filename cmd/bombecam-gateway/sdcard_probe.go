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
		Method string
		Path   string
		Data   map[string]any
	}{
		// Controllo di sanità: questi due funzionano già nel gateway.
		{"GET", "device/list?per_page=1&page=1", nil},
		{"POST", "webrtcsession/user/videocall", map[string]any{"device_id": camID}},
		// Varianti SD card.
		{"POST", "device/sdcard/info", map[string]any{"device_id": camID}},
		{"GET", "device/sdcard/info", nil},
		{"POST", "sdcard/info", map[string]any{"device_id": camID}},
		{"GET", "device/record/list?device_id=" + camID + "&per_page=5", nil},
		{"POST", "device/record/list", map[string]any{"device_id": camID, "start_time": 0, "end_time": 0, "page": 1, "per_page": 5}},
		{"GET", "record/list?device_id=" + camID + "&per_page=5", nil},
		{"POST", "device/sdcard/list", map[string]any{"device_id": camID}},
		{"POST", "device/storage", map[string]any{"device_id": camID}},
		{"POST", "device/storage/info", map[string]any{"device_id": camID}},
		{"POST", "device/record/sdcard", map[string]any{"device_id": camID}},
		{"POST", "device/sdcard/record", map[string]any{"device_id": camID}},
		{"POST", "device/record/list_by_day", map[string]any{"device_id": camID, "day": "2026-11-14"}},
		{"POST", "device/record/month", map[string]any{"device_id": camID, "year": 2026, "month": 11}},
	}
	out := make([]map[string]any, 0, len(probes))
	for _, p := range probes {
		code, msg, keys, err := cloud.Probe(p.Method, p.Path, p.Data)
		row := map[string]any{"method": p.Method, "path": "/v2/device/" + p.Path, "code": code, "msg": msg, "keys": keys}
		if err != nil {
			row["error"] = err.Error()
		}
		out = append(out, row)
	}
	writeJSON(w, http.StatusOK, map[string]any{"camera_id": camID, "probes": out})
}
