package main

// "Block cloud video" — the gateway side.
//
// The camera firewall lives on the router the cameras connect to. Each camera
// is blocked or not; "Block all" blocks every camera, including cameras added
// later. The router is connected once with its admin password: BombeCam
// uploads its router script and installs its own SSH key, which the router
// restricts to the script's gate command (apply, status, connections,
// version, uninstall). After that every change goes over the key, so turning
// a camera on or off, or removing it, updates the router right away. The
// password is never stored. The router's SSH host key is pinned the first
// time, so another device cannot pose as the router later.

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Fever-r/BombeCam/pkg/policy"
	"github.com/Fever-r/BombeCam/pkg/profile"
	"github.com/Fever-r/BombeCam/pkg/renderer/openwrt"
	"github.com/Fever-r/BombeCam/pkg/routerpush"
)

// Replaced in tests.
var (
	routerRun       = routerpush.Run
	newRouterKeySet = routerpush.NewKeySet
)

// routerMu serialises router changes: two SSH sessions editing the same
// router at once would race on its config files.
var routerMu sync.Mutex

// routerLast remembers the most recent router outcome for the status page.
var routerLast struct {
	sync.Mutex
	outcome routerOutcome
	at      time.Time
	auto    bool // the outcome came from the background sync
}

// Camera states shown in the UI.
const (
	camStateBlocked       = "blocked"         // chosen and in force on the router
	camStateNotBlocked    = "not_blocked"     // not chosen, not on the router
	camStateWaitingForMAC = "waiting_for_mac" // chosen, but BombeCam doesn't know its MAC yet
	camStatePending       = "pending"         // router not updated yet (connected router)
	camStateNotApplied    = "not_applied"     // router not connected, so the choice isn't in force
)

type privacyCamera struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	MAC      string `json:"mac"`
	IP       string `json:"ip,omitempty"`
	Blocked  bool   `json:"blocked"`   // the user's choice
	OnRouter bool   `json:"on_router"` // the router blocks it now
	Covered  bool   `json:"covered"`   // same as on_router (older clients)
	State    string `json:"state"`
}

// routerOutcome is what happened when BombeCam tried to bring the router in
// line with the per-camera choices.
type routerOutcome struct {
	OK       bool     `json:"ok"`
	Changed  bool     `json:"changed"` // something was sent to the router
	Code     string   `json:"error,omitempty"`
	Message  string   `json:"message,omitempty"`
	Warnings []string `json:"warnings,omitempty"`
	Output   string   `json:"output,omitempty"`
	Firewall string   `json:"firewall,omitempty"`
}

func yesNoWord(b bool) string {
	if b {
		return "Yes"
	}
	return "No"
}

// privacyChoice says whether the router blocks any camera: true, false, or
// null when the router was never set up.
func privacyChoice(prof *profile.Profile) any {
	if prof == nil || prof.Privacy.BlockCloudVideo == nil {
		return nil
	}
	return *prof.Privacy.BlockCloudVideo
}

// desiredBlocked is the user's choice for one camera.
func desiredBlocked(ps profile.PrivacySettings, id string) bool {
	if v, ok := ps.Blocked[id]; ok {
		return v
	}
	if ps.Blocked == nil && !ps.BlockNewCameras && ps.BlockCloudVideo != nil && *ps.BlockCloudVideo {
		return true // settings from the single Yes/No version: Yes meant every camera
	}
	return ps.BlockNewCameras
}

// collectPrivacyCameras lists the enrolled cameras with their MAC addresses
// and blocking state. A MAC the profile does not know yet is looked up from
// this PC's ARP table using the camera's LAN address, and remembered.
func collectPrivacyCameras(ctx context.Context, streamMgr *StreamManager, pm profile.ProfileManager) []privacyCamera {
	var prof *profile.Profile
	if pm != nil {
		prof = pm.GetProfile()
	}
	byID := map[string]*privacyCamera{}
	var order []string
	add := func(id, name, mac, ip string) {
		c, ok := byID[id]
		if !ok {
			c = &privacyCamera{ID: id}
			byID[id] = c
			order = append(order, id)
		}
		if c.Name == "" {
			c.Name = name
		}
		if c.MAC == "" {
			c.MAC = policy.CanonicalMAC(mac)
		}
		if c.IP == "" {
			c.IP = ip
		}
	}
	if prof != nil {
		for id, cam := range prof.Cameras {
			add(id, cam.Name, cam.MACAddress, cam.IPAddress)
		}
	}
	if streamMgr != nil {
		for _, mc := range streamMgr.GetAllCameras() {
			mc.mu.RLock()
			id, name, ip := mc.UUID, mc.Name, mc.IP
			mc.mu.RUnlock()
			add(id, name, "", ip)
		}
	}
	learned := map[string]string{}
	for _, id := range order {
		c := byID[id]
		if c.MAC == "" && c.IP != "" {
			if mac := policy.CanonicalMAC(lookupMAC(c.IP)); mac != "" {
				c.MAC = mac
				learned[id] = mac
			} else {
				noteMACMissing(c.Name, c.IP)
			}
		}
		if c.Name == "" {
			c.Name = id
		}
	}
	if len(learned) > 0 && pm != nil && prof != nil {
		_, _ = pm.Update(ctx, func(p *profile.Profile) error {
			for id, mac := range learned {
				if cam, ok := p.Cameras[id]; ok && cam.MACAddress == "" {
					cam.MACAddress = mac
					p.Cameras[id] = cam
				}
			}
			return nil
		})
	}
	var ps profile.PrivacySettings
	if prof != nil {
		ps = prof.Privacy
	}
	applied := map[string]bool{}
	for _, m := range ps.AppliedCameras {
		applied[m] = true
	}
	out := make([]privacyCamera, 0, len(order))
	for _, id := range order {
		c := byID[id]
		c.Blocked = desiredBlocked(ps, id)
		c.OnRouter = c.MAC != "" && applied[c.MAC]
		c.Covered = c.OnRouter
		switch {
		case c.Blocked && c.MAC == "":
			c.State = camStateWaitingForMAC
		case c.Blocked == c.OnRouter && c.Blocked:
			c.State = camStateBlocked
		case c.Blocked == c.OnRouter:
			c.State = camStateNotBlocked
		case ps.RouterConnected:
			c.State = camStatePending
		default:
			c.State = camStateNotApplied
		}
		out = append(out, *c)
	}
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name) })
	return out
}

// routerCameras are the cameras the router should block (chosen, MAC known),
// sorted by MAC.
func routerCameras(cams []privacyCamera) []openwrt.Camera {
	var out []openwrt.Camera
	for _, c := range cams {
		if c.Blocked && c.MAC != "" {
			out = append(out, openwrt.Camera{Name: c.Name, MAC: c.MAC})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].MAC < out[j].MAC })
	return out
}

func sameMACs(want []openwrt.Camera, applied []string) bool {
	if len(want) != len(applied) {
		return false
	}
	set := map[string]bool{}
	for _, m := range applied {
		set[m] = true
	}
	for _, c := range want {
		if !set[c.MAC] {
			return false
		}
	}
	return true
}

func masterState(cams []privacyCamera) string {
	if len(cams) == 0 {
		return "empty"
	}
	n := 0
	for _, c := range cams {
		if c.Blocked {
			n++
		}
	}
	switch n {
	case 0:
		return "none"
	case len(cams):
		return "all"
	}
	return "some"
}

func recordRouterOutcome(o routerOutcome, auto bool) {
	routerLast.Lock()
	routerLast.outcome, routerLast.at, routerLast.auto = o, time.Now(), auto
	routerLast.Unlock()
}

// outcomeFromError turns an SSH failure into advice.
func outcomeFromError(err error) routerOutcome {
	o := routerOutcome{Code: "router_unreachable",
		Message: "Couldn't reach the router over SSH. Check that this PC is on the router's network and that the router is on."}
	switch {
	case errors.Is(err, routerpush.ErrAuth):
		o.Code, o.Message = "router_auth", "The router rejected the password. Use the router's admin password (user root)."
	case errors.Is(err, routerpush.ErrHostKeyChanged):
		o.Code, o.Message = "router_identity_changed",
			"This router's identity changed since BombeCam last connected. If you reset or replaced the router, choose \"Forget router\" and connect again; otherwise something else may be answering at this address."
	case errors.Is(err, routerpush.ErrBadAddress):
		o.Code, o.Message = "invalid_router_address", "Enter the router's address, for example 192.168.8.1."
	}
	return o
}

// syncRouter brings the router in line with the per-camera choices over
// BombeCam's key. force sends the list even when it looks unchanged (after
// connecting, the router's actual state is unknown).
func syncRouter(ctx context.Context, streamMgr *StreamManager, pm profile.ProfileManager, force bool) routerOutcome {
	routerMu.Lock()
	defer routerMu.Unlock()
	o := syncRouterLocked(ctx, streamMgr, pm, force)
	recordRouterOutcome(o, false)
	return o
}

func syncRouterLocked(ctx context.Context, streamMgr *StreamManager, pm profile.ProfileManager, force bool) routerOutcome {
	if pm == nil || pm.GetProfile() == nil {
		return routerOutcome{Code: "not_configured", Message: "Sign in and connect your cameras first."}
	}
	cams := collectPrivacyCameras(ctx, streamMgr, pm)
	prof := pm.GetProfile() // re-read: MAC learning may have updated it
	ps := prof.Privacy
	region := privacyRegion(ps)
	want := routerCameras(cams)
	if !force && sameMACs(want, ps.AppliedCameras) && appliedRegion(ps) == string(region) {
		return routerOutcome{OK: true}
	}
	if !ps.RouterConnected || prof.RouterKey.IsEmpty() {
		return routerOutcome{Code: "router_not_connected", Message: "Connect your router so BombeCam can apply this."}
	}
	ks, err := routerpush.ParseKeySet(prof.RouterKey.Expose())
	if err != nil {
		markRouterDisconnected(ctx, pm)
		return routerOutcome{Code: "router_not_connected", Message: "BombeCam's router key is unreadable. Connect the router again."}
	}
	spec, err := openwrt.ApplySpec(want, policy.Options{BlockStreamSetup: ps.BlockStreamSetup}, region)
	if err != nil {
		return routerOutcome{Code: "invalid_camera", Message: err.Error()}
	}
	res, err := routerRun(ctx, routerpush.Target{
		Address: ps.RouterAddress, User: ps.RouterUser, Signers: ks.Signers(),
		PinnedHostKey: ps.RouterHostKey, PinnedHostKeyType: ps.RouterHostKeyType,
	}, openwrt.GateApply, spec)
	if err != nil {
		o := outcomeFromError(err)
		if errors.Is(err, routerpush.ErrAuth) {
			markRouterDisconnected(ctx, pm)
			o.Code = "router_password_needed"
			o.Message = "The router no longer accepts BombeCam's key (it may have been reset). Connect it again with its password."
		}
		fmt.Printf("[privacy] router update failed: %s (%v)\n", o.Code, err)
		return o
	}
	result, found := openwrt.ParseResult(res.Output)
	if res.ExitCode != 0 || !found || result.Status != "ok" {
		fmt.Printf("[privacy] router script failed (exit %d)\n", res.ExitCode)
		return routerOutcome{Code: "router_script_failed", Message: routerFailureMessage(res.Output), Output: res.Output}
	}
	if msg := routerReportMismatch(len(want), result); msg != "" {
		// what the router says it did is not what was asked: keep the old
		// "applied" record so the next sync tries again, and say so
		fmt.Printf("[privacy] router %s: %s\n", ps.RouterAddress, msg)
		return routerOutcome{Code: "router_state_mismatch", Message: msg, Output: res.Output, Firewall: result.Firewall}
	}
	recordApplied(ctx, pm, want, result, res)
	fmt.Printf("[privacy] router %s reports: %s\n", ps.RouterAddress, routerReportSummary(result))
	warnings := append(scriptWarnings(res.Output), routerReportWarnings(result)...)
	warnings = append(warnings, routerApplyWarnings(result)...)
	for _, w := range warnings {
		fmt.Printf("[privacy] router %s warning: %s\n", ps.RouterAddress, w)
	}
	return routerOutcome{OK: true, Changed: true, Warnings: warnings, Output: res.Output, Firewall: result.Firewall}
}

// routerReportMismatch compares what the router script reports after an
// apply with what BombeCam asked for, so the router's own answer decides.
func routerReportMismatch(want int, r openwrt.Result) string {
	if c, ok := r.Fields["cameras"]; ok && c != strconv.Itoa(want) {
		return fmt.Sprintf("BombeCam asked the router to block %d camera(s), but the router reports %s.", want, c)
	}
	if want == 0 {
		if r.Block != "" && r.Block != "no" {
			return "BombeCam asked the router to block no camera, but the router still reports blocking."
		}
		if left, ok := r.Fields["residue"]; ok && left != "0" {
			return fmt.Sprintf("The router removed the camera list, but %s BombeCam rule(s) are still in its firewall. Restart the router to clear them.", left)
		}
	}
	return ""
}

// routerReportSummary is the router's own account, for the log.
func routerReportSummary(r openwrt.Result) string {
	s := fmt.Sprintf("blocking %s camera(s)", orDefault(r.Fields["cameras"], "?"))
	if left, ok := r.Fields["residue"]; ok {
		s += fmt.Sprintf(", BombeCam rules left in the firewall: %s (checked)", left)
	}
	switch r.Fields["cleared"] {
	case "yes":
		s += ", cameras' open connections cleared"
	case "none":
		s += ", no camera connection to clear"
	case "no":
		s += ", open connections could not be cleared"
	}
	if v := r.Fields["version"]; v != "" {
		s += ", script " + v
	}
	return s
}

// routerReportWarnings notes when the router's script is too old to confirm
// that unblocking really cleared its firewall.
func routerReportWarnings(r openwrt.Result) []string {
	if r.Block == "no" {
		if _, ok := r.Fields["residue"]; !ok {
			return []string{"This router's BombeCam script is too old to confirm that its firewall is clean. Press Update router to install the new one, then Check router."}
		}
	}
	return nil
}

// routerApplyWarnings notes when blocking went on with a router script too old
// to clear connections the cameras opened before the block.
func routerApplyWarnings(r openwrt.Result) []string {
	if r.Block == "yes" {
		if _, ok := r.Fields["cleared"]; !ok {
			return []string{"This router's BombeCam script doesn't clear connections the cameras opened before the block. Press Update router to install the new one."}
		}
	}
	return nil
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

// handleRouterCheck handles POST /api/v1/privacy/router/check: it asks the
// router (over BombeCam's key) for its status, which reads the live firewall,
// and returns the router's own report.
func handleRouterCheck(w http.ResponseWriter, r *http.Request, streamMgr *StreamManager, optPM ...profile.ProfileManager) {
	pm, ok := privacyGuard(w, r, optPM)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), changeTimeout)
	defer cancel()
	o, result := checkRouter(ctx, pm)
	writeJSON(w, statusForOutcome(o), map[string]any{"ok": o.OK, "router": o, "error": o.Code, "message": o.Message,
		"result": result.Fields, "status": privacyStatus(r.Context(), streamMgr, pm)})
}

func checkRouter(ctx context.Context, pm profile.ProfileManager) (routerOutcome, openwrt.Result) {
	routerMu.Lock()
	defer routerMu.Unlock()
	prof := pm.GetProfile()
	if prof == nil {
		return routerOutcome{Code: "not_configured", Message: "Sign in and connect your cameras first."}, openwrt.Result{}
	}
	ps := prof.Privacy
	if !ps.RouterConnected || prof.RouterKey.IsEmpty() {
		return routerOutcome{Code: "router_not_connected", Message: "Connect the router first."}, openwrt.Result{}
	}
	ks, err := routerpush.ParseKeySet(prof.RouterKey.Expose())
	if err != nil {
		return routerOutcome{Code: "router_not_connected", Message: "BombeCam's router key is unreadable. Connect the router again."}, openwrt.Result{}
	}
	res, err := routerRun(ctx, routerpush.Target{Address: ps.RouterAddress, User: ps.RouterUser, Signers: ks.Signers(),
		PinnedHostKey: ps.RouterHostKey, PinnedHostKeyType: ps.RouterHostKeyType}, openwrt.GateStatus, nil)
	if err != nil {
		o := outcomeFromError(err)
		if errors.Is(err, routerpush.ErrAuth) {
			o.Code, o.Message = "router_password_needed", "The router no longer accepts BombeCam's key. Connect it again with its password."
		}
		return o, openwrt.Result{}
	}
	result, found := openwrt.ParseResult(res.Output)
	if res.ExitCode != 0 || !found || result.Status != "ok" {
		return routerOutcome{Code: "router_script_failed", Message: routerFailureMessage(res.Output), Output: res.Output}, result
	}
	fmt.Printf("[privacy] router %s check: %s\n", ps.RouterAddress, routerReportSummary(result))
	want := len(ps.AppliedCameras)
	if msg := routerReportMismatch(want, result); msg != "" {
		fmt.Printf("[privacy] router %s: %s\n", ps.RouterAddress, msg)
		return routerOutcome{Code: "router_state_mismatch", Message: msg, Output: res.Output, Firewall: result.Firewall}, result
	}
	return routerOutcome{OK: true, Output: res.Output, Warnings: routerReportWarnings(result), Firewall: result.Firewall}, result
}

func recordApplied(ctx context.Context, pm profile.ProfileManager, want []openwrt.Camera, result openwrt.Result, res routerpush.Result) {
	macs := make([]string, 0, len(want))
	for _, c := range want {
		macs = append(macs, c.MAC)
	}
	now := time.Now().UTC()
	_, _ = pm.Update(ctx, func(p *profile.Profile) error {
		p.Privacy.AppliedCameras = macs
		p.Privacy.BlockCloudVideo = profile.BoolPtr(len(macs) > 0)
		p.Privacy.AppliedAt = now
		if result.Firewall != "" {
			p.Privacy.RouterFirewall = result.Firewall
		}
		if v := result.Fields["version"]; v != "" {
			p.Privacy.RouterScriptVersion = v
		}
		if v := result.Fields["region"]; v != "" {
			p.Privacy.AppliedRegion = v
		}
		if p.Privacy.RouterHostKeyType == "" && res.HostKey == p.Privacy.RouterHostKey {
			p.Privacy.RouterHostKeyType = res.HostKeyType
		}
		return nil
	})
}

func markRouterDisconnected(ctx context.Context, pm profile.ProfileManager) {
	_, _ = pm.Update(ctx, func(p *profile.Profile) error {
		p.Privacy.RouterConnected = false
		return nil
	})
}

// privacyAutoSyncLoop keeps the router in line in the background: a camera
// that was chosen before its MAC was known, or a change the router missed
// while unreachable, is applied as soon as possible. Failures back off.
func privacyAutoSyncLoop(ctx context.Context, streamMgr *StreamManager) {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		pm := ActiveProfileManager()
		if pm == nil {
			continue
		}
		prof := pm.GetProfile()
		if prof == nil || !prof.Privacy.RouterConnected {
			continue
		}
		routerLast.Lock()
		failedRecently := !routerLast.outcome.OK && !routerLast.at.IsZero() && time.Since(routerLast.at) < 2*time.Minute
		routerLast.Unlock()
		if failedRecently || !routerMu.TryLock() {
			continue
		}
		func() {
			defer routerMu.Unlock()
			cctx, cancel := context.WithTimeout(ctx, 90*time.Second)
			defer cancel()
			if o := syncRouterLocked(cctx, streamMgr, pm, false); o.Changed || !o.OK {
				recordRouterOutcome(o, true)
			}
		}()
	}
}

// handlePrivacyStatus handles GET /api/v1/privacy.
func handlePrivacyStatus(w http.ResponseWriter, r *http.Request, streamMgr *StreamManager, optPM ...profile.ProfileManager) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET required", http.StatusMethodNotAllowed)
		return
	}
	pm := getEffectivePM(optPM...)
	writeJSON(w, http.StatusOK, privacyStatus(r.Context(), streamMgr, pm))
}

func privacyStatus(ctx context.Context, streamMgr *StreamManager, pm profile.ProfileManager) map[string]any {
	cams := collectPrivacyCameras(ctx, streamMgr, pm)
	var ps profile.PrivacySettings
	var prof *profile.Profile
	if pm != nil {
		if prof = pm.GetProfile(); prof != nil {
			ps = prof.Privacy
		}
	}
	want := routerCameras(cams)
	region := privacyRegion(ps)
	inSync := sameMACs(want, ps.AppliedCameras) && appliedRegion(ps) == string(region)
	blocked, waiting := 0, []string{}
	for _, c := range cams {
		if c.Blocked {
			blocked++
			if c.MAC == "" {
				waiting = append(waiting, c.Name)
			}
		}
	}
	notReady := ""
	if len(waiting) > 0 {
		notReady = fmt.Sprintf("BombeCam hasn't learned the network (MAC) address of %s yet. "+
			"It appears once the camera has streamed to this PC over your local network; open its live view once.",
			strings.Join(waiting, ", "))
	}
	// The cameras' own addresses point at the router they are on.
	var camIPs []string
	for _, c := range cams {
		if c.IP != "" {
			camIPs = append(camIPs, c.IP)
		}
	}
	if len(camIPs) == 0 && prof != nil {
		for _, c := range prof.Cameras {
			if c.IPAddress != "" {
				camIPs = append(camIPs, c.IPAddress)
			}
		}
	}
	last := ps.LastRouterAddress
	if last == "" {
		last = ps.RouterAddress
	}
	suggestion := suggestRouterAddress(last, camIPs)
	scriptVersion := openwrt.ScriptVersion()
	resp := map[string]any{
		"headline":                 policy.Headline,
		"cameras":                  cams,
		"cameras_connected":        len(cams),
		"camera_count":             len(cams),
		"blocked_count":            blocked,
		"master":                   masterState(cams),
		"block_new_cameras":        ps.BlockNewCameras,
		"in_sync":                  inSync,
		"needs_reapply":            !inSync,
		"ready":                    len(waiting) == 0,
		"not_ready_reason":         notReady,
		"block_cloud_video":        privacyChoice(prof),
		"suggested_router_address": suggestion.Address,
		"suggested_router_source":  suggestion.Source,
		"cap_bytes_per_second":     policy.CapBytesPerSecond,
		"region":                   string(region),
		"applied_region":           appliedRegion(ps),
		"allowlist":                allowlistView(ps.BlockStreamSetup, region),
		"block_stream_setup":       ps.BlockStreamSetup,
		"router": map[string]any{
			"connected":       ps.RouterConnected && prof != nil && !prof.RouterKey.IsEmpty(),
			"known":           ps.RouterAddress != "",
			"address":         ps.RouterAddress,
			"user":            ps.RouterUser,
			"host_key":        ps.RouterHostKey,
			"firewall":        ps.RouterFirewall,
			"script_version":  ps.RouterScriptVersion,
			"script_outdated": ps.RouterConnected && ps.RouterScriptVersion != "" && ps.RouterScriptVersion != scriptVersion,
		},
		"script_version": scriptVersion,
	}
	if !ps.AppliedAt.IsZero() {
		resp["applied_at"] = ps.AppliedAt.UTC().Format(time.RFC3339)
	}
	routerLast.Lock()
	if !routerLast.at.IsZero() && !routerLast.outcome.OK {
		resp["last_error"] = map[string]any{
			"error": routerLast.outcome.Code, "message": routerLast.outcome.Message,
			"at": routerLast.at.UTC().Format(time.RFC3339), "background": routerLast.auto,
		}
	}
	routerLast.Unlock()
	if len(want) > 0 {
		args := []string{"apply", "yes"}
		for _, c := range want {
			args = append(args, "--camera", policy.SanitizeName(c.Name)+"="+c.MAC)
		}
		if region != policy.RegionUS {
			args = append(args, "--region", string(region))
		}
		resp["manual_command"] = openwrt.ManualCommand(args)
	}
	resp["manual_remove_command"] = "bombecam-router uninstall"
	return resp
}

// privacyRegion returns the saved region, falling back to the default when it
// is unset or unrecognised (old profiles carry no region).
func privacyRegion(ps profile.PrivacySettings) policy.Region {
	r, err := policy.ParseRegion(ps.Region)
	if err != nil {
		return policy.DefaultRegion
	}
	return r
}

// appliedRegion returns the region the router last applied, treating an empty
// record as the default so old profiles do not force a pointless reapply.
func appliedRegion(ps profile.PrivacySettings) string {
	if ps.AppliedRegion == "" {
		return string(policy.DefaultRegion)
	}
	return ps.AppliedRegion
}

func allowlistView(blockStreamSetup bool, region policy.Region) []map[string]any {
	doc, err := policy.Compile([]policy.Subject{{ID: "x", MAC: "02:00:00:00:00:01"}},
		policy.Setting{BlockCloudVideo: true, Region: region}, policy.Options{BlockStreamSetup: blockStreamSetup})
	if err != nil {
		return nil
	}
	out := make([]map[string]any, 0, len(doc.Allow))
	for _, a := range doc.Allow {
		protos := make([]string, len(a.Protocols))
		for i, p := range a.Protocols {
			protos[i] = strings.ToUpper(string(p))
		}
		out = append(out, map[string]any{
			"id": a.ID, "purpose": a.Purpose, "host": a.Host, "port": a.Port, "protocols": protos,
		})
	}
	return out
}

// privacyGuard checks method, CSRF/session and profile for state changes.
func privacyGuard(w http.ResponseWriter, r *http.Request, optPM []profile.ProfileManager) (profile.ProfileManager, bool) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return nil, false
	}
	if om := GatewayOperatorManager(); om != nil {
		// See signInRequired: other devices (and this PC, if the administrator
		// chose so) need a signed-in session or API token.
		if !om.ValidateSecurityBoundary(w, r, signInRequired(r)) {
			return nil, false
		}
	}
	pm := getEffectivePM(optPM...)
	if pm == nil || pm.GetProfile() == nil {
		writeJSON(w, http.StatusConflict, map[string]any{"error": "not_configured", "message": "Sign in and connect your cameras first."})
		return nil, false
	}
	return pm, true
}

// isLoopbackRequest reports whether a request came from this PC.
func isLoopbackRequest(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}

// validateOperatorRequest lets a request through when it needs no sign-in
// (this PC, unless the administrator asked for sign-in here too) or comes
// from a signed-in administrator. A visitor cookie and CSRF token only
// establish a browser boundary, not permission to operate the gateway.
func validateOperatorRequest(w http.ResponseWriter, r *http.Request) bool {
	om := GatewayOperatorManager()
	return om == nil || om.ValidateSecurityBoundary(w, r, signInRequired(r))
}

// changeTimeout bounds a router update made on behalf of a click.
const changeTimeout = 90 * time.Second

// handlePrivacyCamera handles POST /api/v1/privacy/camera
// {"camera_id": "...", "blocked": true|false}.
func handlePrivacyCamera(w http.ResponseWriter, r *http.Request, streamMgr *StreamManager, optPM ...profile.ProfileManager) {
	pm, ok := privacyGuard(w, r, optPM)
	if !ok {
		return
	}
	var req struct {
		CameraID string `json:"camera_id"`
		Blocked  *bool  `json:"blocked"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil || req.CameraID == "" || req.Blocked == nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_request", "message": `Send {"camera_id": "...", "blocked": true|false}.`})
		return
	}
	found := false
	for _, c := range collectPrivacyCameras(r.Context(), streamMgr, pm) {
		if c.ID == req.CameraID {
			found = true
		}
	}
	if !found {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "unknown_camera", "message": "That camera isn't set up in BombeCam."})
		return
	}
	_, _ = pm.Update(r.Context(), func(p *profile.Profile) error {
		if p.Privacy.Blocked == nil {
			p.Privacy.Blocked = map[string]bool{}
		}
		p.Privacy.Blocked[req.CameraID] = *req.Blocked
		return nil
	})
	respondAfterChange(w, r, streamMgr, pm)
}

// handlePrivacyBlockAll handles POST /api/v1/privacy/block-all
// {"blocked": true|false}: every camera, and cameras added later.
func handlePrivacyBlockAll(w http.ResponseWriter, r *http.Request, streamMgr *StreamManager, optPM ...profile.ProfileManager) {
	pm, ok := privacyGuard(w, r, optPM)
	if !ok {
		return
	}
	var req struct {
		Blocked *bool `json:"blocked"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil || req.Blocked == nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_request", "message": `Send {"blocked": true|false}.`})
		return
	}
	setAllBlocked(r.Context(), streamMgr, pm, *req.Blocked)
	respondAfterChange(w, r, streamMgr, pm)
}

func setAllBlocked(ctx context.Context, streamMgr *StreamManager, pm profile.ProfileManager, block bool) {
	cams := collectPrivacyCameras(ctx, streamMgr, pm)
	_, _ = pm.Update(ctx, func(p *profile.Profile) error {
		p.Privacy.Blocked = map[string]bool{}
		for _, c := range cams {
			p.Privacy.Blocked[c.ID] = block
		}
		p.Privacy.BlockNewCameras = block
		return nil
	})
}

// respondAfterChange pushes the new choices to the router (when connected)
// and replies with the outcome and the fresh status. The choice itself is
// saved either way, so the reply is 200 unless the request was invalid.
func respondAfterChange(w http.ResponseWriter, r *http.Request, streamMgr *StreamManager, pm profile.ProfileManager) {
	// Not tied to the browser request: a half-applied router is worse than a
	// late reply.
	ctx, cancel := context.WithTimeout(context.Background(), changeTimeout)
	defer cancel()
	o := syncRouter(ctx, streamMgr, pm, false)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "router": o, "status": privacyStatus(r.Context(), streamMgr, pm)})
}

type routerConnectRequest struct {
	RouterAddress  string `json:"router_address"`
	RouterUser     string `json:"router_user"`
	RouterPassword string `json:"router_password"`
	// Region is optional ("us" or "eu"). Empty keeps the saved choice.
	Region string `json:"region,omitempty"`
}

func (req routerConnectRequest) validate() (addr, user string, o *routerOutcome) {
	addr, err := routerpush.NormalizeAddress(req.RouterAddress)
	if err != nil {
		return "", "", &routerOutcome{Code: "invalid_router_address", Message: "Enter the router's address, for example 192.168.8.1."}
	}
	user = strings.TrimSpace(req.RouterUser)
	if user == "" {
		user = "root"
	}
	if strings.ContainsAny(user, " \t\r\n'\"`$;&|<>") || len(user) > 32 {
		return "", "", &routerOutcome{Code: "invalid_router_user", Message: "Router user name is not valid."}
	}
	if req.Region != "" {
		if _, err := policy.ParseRegion(req.Region); err != nil {
			return "", "", &routerOutcome{Code: "invalid_region", Message: err.Error()}
		}
	}
	return addr, user, nil
}

func statusForOutcome(o routerOutcome) int {
	switch o.Code {
	case "":
		return http.StatusOK
	case "router_auth":
		return http.StatusUnauthorized
	case "router_identity_changed", "not_configured", "busy":
		return http.StatusConflict
	case "invalid_router_address", "invalid_router_user", "invalid_region", "invalid_request":
		return http.StatusBadRequest
	}
	return http.StatusBadGateway
}

// connectRouter uploads the router script over the password, installs
// BombeCam's key and then applies the current choices over the key. If the
// router won't accept the key, the choices are applied over the password
// instead and the router is left "not connected" (each change then asks for
// the password).
func connectRouter(ctx context.Context, streamMgr *StreamManager, pm profile.ProfileManager, req routerConnectRequest) (routerOutcome, routerpush.Result) {
	addr, user, bad := req.validate()
	if bad != nil {
		return *bad, routerpush.Result{}
	}
	routerMu.Lock()
	defer routerMu.Unlock()

	prof := pm.GetProfile()
	prev := prof.Privacy
	// Pin the host key per router address: a different address is a
	// different router and starts a fresh trust-on-first-use.
	pinned, pinnedType := "", ""
	if prev.RouterHostKey != "" && sameRouter(prev.RouterAddress, addr) {
		pinned, pinnedType = prev.RouterHostKey, prev.RouterHostKeyType
	}
	var ks *routerpush.KeySet
	if !prof.RouterKey.IsEmpty() {
		ks, _ = routerpush.ParseKeySet(prof.RouterKey.Expose())
	}
	if ks == nil {
		var err error
		if ks, err = newRouterKeySet(); err != nil {
			return routerOutcome{Code: "internal_error", Message: "Couldn't create BombeCam's router key: " + err.Error()}, routerpush.Result{}
		}
	}
	passwordTarget := routerpush.Target{Address: addr, User: user, Password: req.RouterPassword,
		PinnedHostKey: pinned, PinnedHostKeyType: pinnedType}
	res, err := routerRun(ctx, passwordTarget, openwrt.RemoteCommand(openwrt.ConnectArgs(ks.AuthorizedKeys())), openwrt.Script())
	if err != nil {
		o := outcomeFromError(err)
		fmt.Printf("[privacy] connecting router %s failed: %s (%v)\n", addr, o.Code, err)
		return o, res
	}
	result, found := openwrt.ParseResult(res.Output)
	if res.ExitCode != 0 || !found || result.Status != "ok" {
		return routerOutcome{Code: "router_script_failed", Message: routerFailureMessage(res.Output), Output: res.Output}, res
	}
	_, _ = pm.Update(ctx, func(p *profile.Profile) error {
		if !sameRouter(p.Privacy.RouterAddress, addr) {
			p.Privacy.AppliedCameras = nil // a different router: nothing known about it
			p.Privacy.AppliedRegion = ""
			p.Privacy.BlockCloudVideo = nil
		}
		if req.Region != "" {
			if r, err := policy.ParseRegion(req.Region); err == nil {
				p.Privacy.Region = string(r)
			}
		}
		p.Privacy.RouterAddress = addr
		p.Privacy.LastRouterAddress = addr
		p.Privacy.RouterUser = user
		p.Privacy.RouterHostKey = res.HostKey
		p.Privacy.RouterHostKeyType = res.HostKeyType
		p.Privacy.RouterFirewall = result.Firewall
		p.Privacy.RouterScriptVersion = result.Fields["version"]
		p.Privacy.RouterConnected = true
		p.RouterKey = profile.SecretString(ks.PEM())
		return nil
	})
	fmt.Printf("[privacy] router %s connected (%s); BombeCam's key installed\n", addr, result.Firewall)
	connectOutput := res.Output

	o := syncRouterLocked(ctx, streamMgr, pm, true)
	if o.Code == "router_password_needed" {
		// The router took the key but won't let it in (e.g. key logins
		// disabled). Apply over the password this once.
		o = applyOverPassword(ctx, streamMgr, pm, passwordTarget, res.HostKey, res.HostKeyType)
		o.Warnings = append(o.Warnings, "The router doesn't accept BombeCam's key, so each change will ask for the router password.")
	}
	o.Output = strings.TrimSpace(connectOutput + "\n" + o.Output)
	if o.Firewall == "" {
		o.Firewall = result.Firewall
	}
	return o, res
}

// applyOverPassword applies the current choices with the uploaded script
// over the password (routers that refuse BombeCam's key).
func applyOverPassword(ctx context.Context, streamMgr *StreamManager, pm profile.ProfileManager, t routerpush.Target, hostKey, hostKeyType string) routerOutcome {
	t.PinnedHostKey, t.PinnedHostKeyType = hostKey, hostKeyType
	cams := collectPrivacyCameras(ctx, streamMgr, pm)
	want := routerCameras(cams)
	ps := pm.GetProfile().Privacy
	region := privacyRegion(ps)
	args := []string{"apply", "no"}
	if len(want) > 0 {
		args = []string{"apply", "yes"}
		for _, c := range want {
			args = append(args, "--camera", policy.SanitizeName(c.Name)+"="+c.MAC)
		}
		if ps.BlockStreamSetup {
			args = append(args, "--block-stream-setup")
		} else {
			args = append(args, "--allow-stream-setup")
		}
		if region != policy.RegionUS {
			args = append(args, "--region", string(region))
		}
	}
	res, err := routerRun(ctx, t, openwrt.RemoteCommand(args), openwrt.Script())
	if err != nil {
		return outcomeFromError(err)
	}
	result, found := openwrt.ParseResult(res.Output)
	if res.ExitCode != 0 || !found || result.Status != "ok" {
		return routerOutcome{Code: "router_script_failed", Message: routerFailureMessage(res.Output), Output: res.Output}
	}
	if msg := routerReportMismatch(len(want), result); msg != "" {
		return routerOutcome{Code: "router_state_mismatch", Message: msg, Output: res.Output, Firewall: result.Firewall}
	}
	recordApplied(ctx, pm, want, result, res)
	return routerOutcome{OK: true, Changed: true, Warnings: scriptWarnings(res.Output), Output: res.Output, Firewall: result.Firewall}
}

// handleRouterConnect handles POST /api/v1/privacy/router/connect
// {"router_address", "router_user", "router_password"}.
func handleRouterConnect(w http.ResponseWriter, r *http.Request, streamMgr *StreamManager, optPM ...profile.ProfileManager) {
	pm, ok := privacyGuard(w, r, optPM)
	if !ok {
		return
	}
	var req routerConnectRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_request",
			"message": `Send {"router_address": "...", "router_password": "..."}.`})
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	o, res := connectRouter(ctx, streamMgr, pm, req)
	recordRouterOutcome(o, false)
	resp := map[string]any{"ok": o.OK, "router": o, "host_key": res.HostKey,
		"error": o.Code, "message": o.Message, "warnings": o.Warnings, "output": o.Output, "firewall": o.Firewall,
		"status": privacyStatus(r.Context(), streamMgr, pm), "headline": policy.Headline}
	writeJSON(w, statusForOutcome(o), resp)
}

// handleRouterDisconnect handles POST /api/v1/privacy/router/disconnect: it
// removes everything BombeCam put on the router (rules and key) over the key,
// then forgets the router and sets every camera to not blocked.
func handleRouterDisconnect(w http.ResponseWriter, r *http.Request, streamMgr *StreamManager, optPM ...profile.ProfileManager) {
	pm, ok := privacyGuard(w, r, optPM)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), changeTimeout)
	defer cancel()
	o := uninstallFromRouter(ctx, pm)
	if o.OK {
		forgetRouter(ctx, pm, true)
	}
	recordRouterOutcome(o, false)
	writeJSON(w, statusForOutcome(o), map[string]any{"ok": o.OK, "router": o, "error": o.Code, "message": o.Message,
		"manual_remove_command": "bombecam-router uninstall", "status": privacyStatus(r.Context(), streamMgr, pm)})
}

// uninstallFromRouter runs the gate's uninstall over BombeCam's key.
func uninstallFromRouter(ctx context.Context, pm profile.ProfileManager) routerOutcome {
	routerMu.Lock()
	defer routerMu.Unlock()
	prof := pm.GetProfile()
	if prof == nil {
		return routerOutcome{OK: true}
	}
	ps := prof.Privacy
	if !ps.RouterConnected || prof.RouterKey.IsEmpty() {
		if len(ps.AppliedCameras) == 0 && ps.RouterAddress == "" {
			return routerOutcome{OK: true} // nothing on any router
		}
		return routerOutcome{Code: "router_not_connected",
			Message: "BombeCam can't reach the router without its password. Connect the router first, or remove the rules by hand with: bombecam-router uninstall"}
	}
	ks, err := routerpush.ParseKeySet(prof.RouterKey.Expose())
	if err != nil {
		return routerOutcome{Code: "router_not_connected", Message: "BombeCam's router key is unreadable. Remove the rules by hand with: bombecam-router uninstall"}
	}
	res, err := routerRun(ctx, routerpush.Target{Address: ps.RouterAddress, User: ps.RouterUser, Signers: ks.Signers(),
		PinnedHostKey: ps.RouterHostKey, PinnedHostKeyType: ps.RouterHostKeyType}, openwrt.GateUninstall, nil)
	if err != nil {
		o := outcomeFromError(err)
		if errors.Is(err, routerpush.ErrAuth) {
			o.Code, o.Message = "router_password_needed", "The router no longer accepts BombeCam's key. Remove the rules by hand with: bombecam-router uninstall"
		}
		return o
	}
	result, found := openwrt.ParseResult(res.Output)
	if res.ExitCode != 0 || !found || result.Status != "ok" {
		return routerOutcome{Code: "router_script_failed", Message: routerFailureMessage(res.Output), Output: res.Output}
	}
	if msg := routerReportMismatch(0, result); msg != "" {
		fmt.Printf("[privacy] router %s: %s\n", ps.RouterAddress, msg)
		return routerOutcome{Code: "router_state_mismatch", Message: msg, Output: res.Output, Firewall: result.Firewall}
	}
	fmt.Printf("[privacy] BombeCam removed from router %s (%s)\n", ps.RouterAddress, routerReportSummary(result))
	return routerOutcome{OK: true, Changed: true, Output: res.Output, Firewall: result.Firewall}
}

// forgetRouter drops the router's address, identity and BombeCam's key. With
// unblock, every camera is also set to not blocked (the router no longer has
// rules); without it the choices are kept for the next router.
func forgetRouter(ctx context.Context, pm profile.ProfileManager, unblock bool) {
	_, _ = pm.Update(ctx, func(p *profile.Profile) error {
		keep, last := p.Privacy.BlockStreamSetup, p.Privacy.LastRouterAddress
		region := p.Privacy.Region
		blocked, blockNew := p.Privacy.Blocked, p.Privacy.BlockNewCameras
		p.Privacy = profile.PrivacySettings{BlockStreamSetup: keep, LastRouterAddress: last, Region: region}
		if !unblock {
			p.Privacy.Blocked, p.Privacy.BlockNewCameras = blocked, blockNew
		}
		p.RouterKey = ""
		return nil
	})
}

// handlePrivacyForgetRouter handles POST /api/v1/privacy/forget-router: after a
// router reset or replacement, forget its address, identity and BombeCam's key
// without contacting it. The per-camera choices are kept.
func handlePrivacyForgetRouter(w http.ResponseWriter, r *http.Request, optPM ...profile.ProfileManager) {
	pm, ok := privacyGuard(w, r, optPM)
	if !ok {
		return
	}
	routerMu.Lock()
	forgetRouter(r.Context(), pm, false)
	routerMu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

type privacyApplyRequest struct {
	BlockCloudVideo *bool `json:"block_cloud_video"`
	routerConnectRequest
}

// handlePrivacyApply handles POST /api/v1/privacy/apply (single-step form):
// set every camera to blocked or not, connect the router with the password,
// and apply.
func handlePrivacyApply(w http.ResponseWriter, r *http.Request, streamMgr *StreamManager, optPM ...profile.ProfileManager) {
	pm, ok := privacyGuard(w, r, optPM)
	if !ok {
		return
	}
	var req privacyApplyRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&req); err != nil || req.BlockCloudVideo == nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error":   "invalid_request",
			"message": `Send {"block_cloud_video": true|false, "router_address": "...", "router_password": "..."}.`,
		})
		return
	}
	if _, _, bad := req.validate(); bad != nil {
		writeJSON(w, statusForOutcome(*bad), map[string]any{"error": bad.Code, "message": bad.Message})
		return
	}
	setAllBlocked(r.Context(), streamMgr, pm, *req.BlockCloudVideo)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	o, res := connectRouter(ctx, streamMgr, pm, req.routerConnectRequest)
	recordRouterOutcome(o, false)
	st := privacyStatus(r.Context(), streamMgr, pm)
	onRouter := len(pm.GetProfile().Privacy.AppliedCameras)
	resp := map[string]any{"ok": o.OK, "router": o, "block_cloud_video": *req.BlockCloudVideo,
		"error": o.Code, "message": o.Message, "firewall": o.Firewall, "host_key": res.HostKey,
		"cameras": onRouter, "warnings": o.Warnings, "output": o.Output, "headline": policy.Headline, "status": st}
	writeJSON(w, statusForOutcome(o), resp)
}

// sameRouter compares two router addresses (host:port normalised).
func sameRouter(a, b string) bool {
	na, err1 := routerpush.NormalizeAddress(a)
	nb, err2 := routerpush.NormalizeAddress(b)
	return err1 == nil && err2 == nil && na == nb
}

func scriptWarnings(out string) []string {
	var ws []string
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		if l := strings.TrimSpace(sc.Text()); strings.HasPrefix(l, "WARNING: ") {
			ws = append(ws, strings.TrimPrefix(l, "WARNING: "))
		}
	}
	return ws
}

func routerFailureMessage(out string) string {
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		if l := strings.TrimSpace(sc.Text()); strings.HasPrefix(l, "ERROR: ") {
			return "The router script stopped: " + strings.TrimPrefix(l, "ERROR: ")
		}
	}
	if strings.Contains(out, "not found") {
		return "The router doesn't look like an OpenWrt-based router (a required command is missing). See Other ways to block for alternatives."
	}
	return "The router script did not finish. Details are below."
}

// handlePrivacyRouterScript handles GET /api/v1/privacy/router-script: the
// router script as a download, for applying it by hand.
func handlePrivacyRouterScript(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET required", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "text/x-shellscript; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+openwrt.ScriptName+`"`)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = w.Write(openwrt.Script())
}

var (
	macMissingMu   sync.Mutex
	macMissingSeen = map[string]bool{}
)

// noteMACMissing logs once per camera address that this PC's ARP table has
// no entry for it, which is what a "waiting for its MAC" report needs.
func noteMACMissing(name, ip string) {
	macMissingMu.Lock()
	defer macMissingMu.Unlock()
	if macMissingSeen[ip] {
		return
	}
	macMissingSeen[ip] = true
	fmt.Printf("[privacy] no MAC address for %s: %s is not in this PC's ARP table (the camera must stream to this PC over the same network)\n", name, ip)
}
