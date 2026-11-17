package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/Fever-r/BombeCam/pkg/bridge"
	"github.com/Fever-r/BombeCam/pkg/policy"
	"github.com/Fever-r/BombeCam/pkg/profile"
)

// cameraSearchAdd is the shared payload for search and add.
type cameraSearchRequest struct {
	Email     string   `json:"email"`
	Password  string   `json:"password"`
	Country   string   `json:"country,omitempty"`
	CameraIDs []string `json:"camera_ids"`
}

type cameraListItem struct {
	ID           string `json:"id"`
	AccountEmail string `json:"account_email"`
	Name         string `json:"name"`
	Model        string `json:"model"`
	Online       bool   `json:"online"`
	IP           string `json:"ip"`
	Enrolled     bool   `json:"enrolled"`
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// handleCameraSearch authenticates an Osaio login and lists the cameras it can
// see, without changing the enrolled set. The login is stored (encrypted) so its
// cameras can reconnect after a restart; nothing is enrolled until the user adds.
//
// POST /api/v1/cameras/search  {email, password}
func handleCameraSearch(w http.ResponseWriter, r *http.Request, streamMgr *StreamManager) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	var req cameraSearchRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid json payload"})
		return
	}
	req.Email = strings.TrimSpace(req.Email)
	if req.Email == "" || req.Password == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "email and password are required"})
		return
	}
	reg := streamMgr.Registry()
	if reg == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "session registry unavailable"})
		return
	}
	pm := streamMgr.ProfileManager()
	var saved *profile.Profile
	if pm != nil {
		saved, _ = pm.Load(r.Context())
	}
	country, countryErr := resolveAccountCountry(req.Country, req.Email, saved, countrySession(reg, req.Email))
	if countryErr != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "country_required", "detail": countryErr.Error()})
		return
	}
	// The same checked sign-in as Osaio logins > Add a login: it stores the
	// login (encrypted) so its cameras reconnect after a restart.
	session, fail := signInToPool(r.Context(), reg, pm, req.Email, req.Password, country)
	if fail != nil {
		writeJSON(w, fail.HTTP, map[string]any{"error": "login failed", "detail": fail.Message})
		return
	}
	devs, _, err := session.DeviceList(true)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": fmt.Sprintf("discovery failed: %v", err)})
		return
	}

	enrolledSet := map[string]bool{}
	if pm != nil {
		if prof := pm.GetProfile(); prof != nil {
			for id := range prof.Cameras {
				enrolledSet[id] = true
			}
		}
	}

	items := make([]cameraListItem, 0, len(devs))
	for _, d := range devs {
		camIP := ""
		if mc, ok := streamMgr.GetCamera(d.UUID); ok {
			mc.mu.RLock()
			camIP = mc.IP
			mc.mu.RUnlock()
		}
		items = append(items, cameraListItem{
			ID:           d.UUID,
			AccountEmail: profile.NormalizeEmail(req.Email),
			Name:         d.Name,
			Model:        d.Type,
			Online:       d.Online != 0,
			IP:           camIP,
			Enrolled:     enrolledSet[d.UUID],
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"account_email": profile.NormalizeEmail(req.Email),
		"cameras":       items,
	})
}

// handleCameraAdd enrolls selected cameras from a previously searched login,
// merging them alongside cameras from other logins (never removing them).
//
// POST /api/v1/cameras/add  {email, camera_ids}
func handleCameraAdd(w http.ResponseWriter, r *http.Request, streamMgr *StreamManager) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	var req cameraSearchRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid json payload"})
		return
	}
	req.Email = profile.NormalizeEmail(req.Email)
	if req.Email == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "email is required"})
		return
	}
	if len(req.CameraIDs) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "camera_ids must not be empty"})
		return
	}
	reg := streamMgr.Registry()
	if reg == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "session registry unavailable"})
		return
	}
	session, ok := reg.Get(req.Email)
	if !ok || !session.IsAuthenticated() {
		writeJSON(w, http.StatusConflict, map[string]any{"error": "search for this login first", "account_email": req.Email})
		return
	}
	newDevs := session.GetCachedDevices()
	newMap := make(map[string]bridge.Device, len(newDevs))
	for _, d := range newDevs {
		newMap[d.UUID] = d
	}
	for _, id := range req.CameraIDs {
		if _, exists := newMap[id]; !exists {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": fmt.Sprintf("camera '%s' not in this login's inventory", id)})
			return
		}
	}

	pm := streamMgr.ProfileManager()

	// Build the union desired set (existing enrolled + newly requested) and a
	// combined inventory so Enroll keeps other logins' cameras running.
	desired := make(map[string]bool)
	combined := make([]bridge.Device, 0, len(newDevs))
	combined = append(combined, newDevs...)
	if pm != nil {
		if prof := pm.GetProfile(); prof != nil {
			for id, cam := range prof.Cameras {
				desired[id] = true
				if _, dup := newMap[id]; !dup {
					combined = append(combined, bridge.Device{UUID: cam.UUID, Name: cam.Name, Type: cam.Model, Model: 1, Online: 1})
				}
			}
		}
	}
	for _, id := range req.CameraIDs {
		desired[id] = true
	}
	desiredList := make([]string, 0, len(desired))
	for id := range desired {
		desiredList = append(desiredList, id)
	}

	if _, err := streamMgr.Enroll(desiredList, combined); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}

	if pm != nil {
		_, _ = pm.Update(r.Context(), func(p *profile.Profile) error {
			if p.Cameras == nil {
				p.Cameras = make(map[string]profile.CameraProfile)
			}
			for _, id := range req.CameraIDs {
				dev := newMap[id]
				cam := p.Cameras[id]
				cam.UUID = id
				cam.AccountEmail = req.Email
				cam.Name = dev.Name
				cam.Model = dev.Type
				cam.Online = dev.Online == 1
				if cam.EnrolledAt.IsZero() {
					cam.EnrolledAt = time.Now().UTC()
				}
				if mc, ok := streamMgr.GetCamera(id); ok {
					if ip := mc.LANIP(); ip != "" {
						cam.IPAddress = ip
					}
				}
				p.Cameras[id] = cam
			}
			return nil
		})
	}

	if pm != nil {
		syncStreamNames(r.Context(), pm, streamMgr)
	}
	kickIntegrations()
	writeJSON(w, http.StatusOK, map[string]any{
		"added":          req.CameraIDs,
		"account_email":  req.Email,
		"active_streams": streamMgr.ActiveStreamCount(),
	})
}

// handleCameraRemove un-enrolls one camera: it stops the local stream, drops
// the camera from storage and, if the router blocks it, removes it from the
// router too (over BombeCam's router key; without a connected router the reply
// says the router still has it).
//
// DELETE /api/v1/cameras/{id}
func handleCameraRemove(w http.ResponseWriter, r *http.Request, camID string, streamMgr *StreamManager) {
	camID = strings.TrimSpace(camID)
	if camID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "camera id required"})
		return
	}

	pm := streamMgr.ProfileManager()
	onRouter := false
	if pm != nil {
		if prof := pm.GetProfile(); prof != nil {
			if cam, ok := prof.Cameras[camID]; ok {
				mac := policy.CanonicalMAC(cam.MACAddress)
				for _, m := range prof.Privacy.AppliedCameras {
					if mac != "" && m == mac {
						onRouter = true
					}
				}
			}
		}
	}

	wasEnrolled := streamMgr.Unenroll(camID)
	if pm != nil {
		_ = pm.UnenrollCamera(r.Context(), camID)
		_, _ = pm.Update(r.Context(), func(p *profile.Profile) error {
			delete(p.Privacy.Blocked, camID)
			return nil
		})
	}

	note := "BombeCam did not change this camera's internet access."
	var router *routerOutcome
	stillOnRouter := false
	if onRouter && pm != nil {
		ctx, cancel := context.WithTimeout(context.Background(), changeTimeout)
		o := syncRouter(ctx, streamMgr, pm, false)
		cancel()
		router = &o
		switch {
		case o.OK:
			note = "Removed from your router too: the router no longer limits this camera."
		case o.Code == "router_not_connected" || o.Code == "router_password_needed":
			stillOnRouter = true
			note = "Your router still limits this camera. Connect the router in the Firewall tab to update it."
		default:
			stillOnRouter = true
			note = "Your router still limits this camera: " + o.Message + " BombeCam will update the router when it can reach it."
		}
	}
	kickIntegrations()
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":              camID != "",
		"removed":         camID,
		"was_enrolled":    wasEnrolled,
		"still_on_router": stillOnRouter,
		"router":          router,
		"firewall_note":   note,
		"active_streams":  streamMgr.ActiveStreamCount(),
	})
}
