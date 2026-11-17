package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/Fever-r/BombeCam/pkg/bridge"
	shadowshim "github.com/Fever-r/BombeCam/pkg/bridge/shadow_shim"
)

// legacyControlTarget resolves a legacy route's ?cam= (UUID or name) to a
// camera and the channel that controls it, answering 503 when there is none.
func legacyControlTarget(w http.ResponseWriter, cam string, streamMgr *StreamManager) (uuid, name string, cc bridge.ControlChannel, ok bool) {
	switch target := findSignaling(cam); {
	case target != nil:
		uuid, name = target.dev.UUID, target.dev.Name
	case GatewayControlChannel() != nil && cam != "":
		uuid, name = cam, cam
	default:
		http.Error(w, "camera not found or not connected", 503)
		return "", "", nil, false
	}
	if cc = cameraControl(streamMgr, uuid); cc == nil {
		http.Error(w, "camera not found or not connected", 503)
		return "", "", nil, false
	}
	return uuid, name, cc, true
}

// controlFailed reports a command the control channel could not send.
func controlFailed(w http.ResponseWriter, what string, err error) {
	writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": fmt.Sprintf("%s failed: %v", what, err)})
}

// handleCameraControl handles POST /api/v1/cameras/{id}/control
func handleCameraControl(w http.ResponseWriter, r *http.Request, camID string, streamMgr *StreamManager) {
	mc, ok := streamMgr.GetCamera(camID)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "camera not found"})
		return
	}

	cc := cameraControl(streamMgr, camID)
	if cc == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "control transport unavailable"})
		return
	}

	var req struct {
		Action     string         `json:"action"`
		Value      any            `json:"value,omitempty"`
		Mode       any            `json:"mode,omitempty"`
		Direction  any            `json:"direction,omitempty"`
		DurationMs any            `json:"duration_ms,omitempty"`
		Params     map[string]any `json:"params,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid json payload"})
		return
	}

	// resolveParam searches for a parameter across top-level fields and nested req.Params keys.
	resolveParam := func(topLevel1 any, topLevel2 any, nestedKeys ...string) any {
		if topLevel1 != nil {
			return topLevel1
		}
		if topLevel2 != nil {
			return topLevel2
		}
		if req.Params != nil {
			for _, k := range nestedKeys {
				if v, exists := req.Params[k]; exists && v != nil {
					return v
				}
			}
		}
		return nil
	}

	start := time.Now()
	var requestedState map[string]any

	normAction := strings.ToLower(strings.TrimSpace(req.Action))
	switch normAction {
	case "ir", "set_ir", "set_ir_auto", "set_ir_on", "set_ir_off":
		// Mode first: "value" is a 0/1 convenience field for on/off switches and
		// cannot express IR's three states.
		rawMode := resolveParam(req.Mode, req.Value, "mode", "value", "IrLedMode")
		if rawMode == nil {
			switch normAction {
			case "set_ir_off":
				rawMode = "off"
			case "set_ir_on":
				rawMode = "on"
			default:
				rawMode = "auto"
			}
		}
		modeVal, okMode := bridge.ParseIRMode(rawMode)
		if !okMode {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid ir mode: use auto, on (night vision) or off"})
			return
		}

		if err := cc.SetIR(r.Context(), camID, modeVal); err != nil {
			controlFailed(w, "ir control", err)
			return
		}
		requestedState = map[string]any{"IrLedMode": modeVal}

	case "led", "set_led", "set_led_on", "set_led_off":
		ledVal := 1

		rawLed := resolveParam(req.Value, req.Mode, "mode", "value", "on", "LedOnOff")
		if rawLed != nil {
			switch v := rawLed.(type) {
			case string:
				switch strings.ToLower(strings.TrimSpace(v)) {
				case "off", "0", "false":
					ledVal = 0
				case "on", "1", "true", "":
					ledVal = 1
				}
			case bool:
				if !v {
					ledVal = 0
				} else {
					ledVal = 1
				}
			case float64:
				if int(v) == 0 {
					ledVal = 0
				} else {
					ledVal = 1
				}
			case int:
				if v == 0 {
					ledVal = 0
				} else {
					ledVal = 1
				}
			}
		} else {
			// Action-name fallback if value was omitted in compound actions
			switch normAction {
			case "set_led_off":
				ledVal = 0
			case "set_led_on":
				ledVal = 1
			default:
				mc.mu.RLock()
				cur := mc.ObservedLED
				mc.mu.RUnlock()
				if cur == "on" {
					ledVal = 0
				} else {
					ledVal = 1
				}
			}
		}

		if err := cc.SetLED(r.Context(), camID, ledVal == 1); err != nil {
			controlFailed(w, "led control", err)
			return
		}
		requestedState = map[string]any{"LedOnOff": ledVal}

	case "light", "set_light":
		lightVal := 1
		rawLight := resolveParam(req.Value, req.Mode, "mode", "value", "on", "LightSW")
		if rawLight != nil {
			switch v := rawLight.(type) {
			case string:
				switch strings.ToLower(strings.TrimSpace(v)) {
				case "off", "0", "false":
					lightVal = 0
				case "on", "1", "true", "":
					lightVal = 1
				}
			case bool:
				if !v {
					lightVal = 0
				} else {
					lightVal = 1
				}
			case float64:
				if int(v) == 0 {
					lightVal = 0
				} else {
					lightVal = 1
				}
			case int:
				if v == 0 {
					lightVal = 0
				} else {
					lightVal = 1
				}
			}
		}

		if err := cc.SetLight(r.Context(), camID, lightVal == 1); err != nil {
			controlFailed(w, "light control", err)
			return
		}
		requestedState = map[string]any{"LightSW": lightVal}

	case "ptz", "set_ptz":
		dir := 0
		dur := 400
		// The same spelling the legacy /ptz route accepts, read in one place.
		rawDir := resolveParam(req.Direction, req.Value, "direction", "value")
		if rawDir != nil {
			switch v := rawDir.(type) {
			case string:
				if d, ok := ptzDirection(v); ok {
					dir = d
				}
			case float64:
				dir = int(v)
			case int:
				dir = v
			}
		}
		rawDur := resolveParam(req.DurationMs, nil, "duration_ms", "duration")
		if rawDur != nil {
			switch v := rawDur.(type) {
			case float64:
				if v >= 0 {
					dur = int(v)
				}
			case int:
				if v >= 0 {
					dur = v
				}
			}
		}

		if err := cc.MovePTZ(r.Context(), camID, dir, dur); err != nil {
			controlFailed(w, "ptz move", err)
			return
		}
		requestedState = map[string]any{"direction": dir}

	case "motion", "motion_detection", "sound", "sound_detection":
		// The camera's own detection switches (the ones in the Osaio app):
		// MotionDetectSW / SoundDetectSW, 1 = on, 0 = off.
		key := "MotionDetectSW"
		if strings.HasPrefix(normAction, "sound") {
			key = "SoundDetectSW"
		}
		val, okVal := parseOnOff(resolveParam(req.Value, req.Mode, "mode", "value", "on", key))
		if !okVal {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid value: use on or off"})
			return
		}
		fmt.Printf("[%s] [SETTINGS] dispatching %s=%d\n", mc.Name, key, val)
		if sendErr := cc.SetAttribute(r.Context(), camID, key, val); sendErr != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": fmt.Sprintf("%s change failed: %v", map[string]string{"MotionDetectSW": "motion detection", "SoundDetectSW": "sound detection"}[key], sendErr)})
			return
		}
		requestedState = map[string]any{key: val}

	default:
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": fmt.Sprintf("unsupported action: %s (supported: ir, led, light, ptz, motion, sound)", req.Action),
		})
		return
	}

	// A successful dispatch is enough for these controls. Camera telemetry is
	// optional and belongs to the separate shadow/status path, not this click.
	durMs := time.Since(start).Milliseconds()
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":              true,
		"camera":          mc.Name,
		"uuid":            mc.UUID,
		"action":          req.Action,
		"requested_state": requestedState,
		"status":          "sent",
		"confirmed_state": map[string]any{},
		"confirmation":    "not_requested",
		"duration_ms":     durMs,
	})
}

// handleCameraShadow handles GET /api/v1/cameras/{id}/shadow
func handleCameraShadow(w http.ResponseWriter, r *http.Request, camID string, streamMgr *StreamManager) {
	mc, ok := streamMgr.GetCamera(camID)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "camera not found"})
		return
	}

	cc := GatewayControlChannel()
	if local, ok := bridge.AsLocalControl(cc); ok {
		doc, err := local.ReportedShadow(camID)
		if err != nil {
			doc = &shadowshim.ShadowDocument{State: shadowshim.ShadowState{Reported: map[string]any{}}}
		}
		writeJSON(w, http.StatusOK, map[string]any{"state": doc.State, "metadata": doc.Metadata, "timestamp": doc.Timestamp, "version": doc.Version, "control": local.LocalStatus(camID)})
		return
	}
	if cc == nil {
		cc = cameraControl(streamMgr, camID)
	}
	ctx, cancel := context.WithTimeout(r.Context(), 1500*time.Millisecond)
	defer cancel()
	reported, err := readCameraAttributes(ctx, cc, camID, "IrLedMode", "LedOnOff", "LightSW", "MotionDetectSW", "SoundDetectSW")
	updateCameraObservations(mc, reported)
	ts := int64(0)
	if len(reported) > 0 {
		ts = time.Now().Unix()
	}
	readbackError := ""
	if err != nil {
		readbackError = err.Error()
	}
	writeJSON(w, http.StatusOK, map[string]any{"state": map[string]any{"reported": reported}, "timestamp": ts, "readback_error": readbackError})
}

// The SD card is read on the control channel (atr.get SdCardInfo / RecordDays);
// see docs/technical/SDCARD_P2P_PLAN.md. When the active channel cannot answer,
// saying so beats a call that can only fail.
const sdCardUnavailable = "The camera did not answer over the control channel. SD card state and the days it holds need a live camera connection."

// sdcardFetcher is the optional capability of a control channel that can query
// the camera's SD card. Like GetAttributes, it stays off bridge.ControlChannel.
type sdcardFetcher interface {
	GetSDCardInfo(ctx context.Context, cameraUUID string) (map[string]any, error)
	GetRecordDays(ctx context.Context, cameraUUID string) (map[string]any, error)
}

func sdcardChannel(cc bridge.ControlChannel) (sdcardFetcher, bool) {
	f, ok := cc.(sdcardFetcher)
	return f, ok && f != nil
}

// sdcardField finds a reply value, tolerating one extra map nesting
// ("data" or any other single wrapper) around the fields.
func sdcardField(raw map[string]any, keys ...string) (any, bool) {
	nested := make([]map[string]any, 0, 1)
	for _, v := range raw {
		if m, ok := v.(map[string]any); ok {
			nested = append(nested, m)
		}
	}
	for _, key := range keys {
		if v, ok := raw[key]; ok && v != nil {
			return v, true
		}
		for _, m := range nested {
			if v, ok := m[key]; ok && v != nil {
				return v, true
			}
		}
	}
	return nil, false
}

func sdcardText(raw map[string]any, keys ...string) (string, bool) {
	v, ok := sdcardField(raw, keys...)
	if !ok {
		return "", false
	}
	switch s := v.(type) {
	case string:
		return s, s != ""
	case float64, int, int64, json.Number:
		return fmt.Sprint(s), true
	case []any:
		parts := make([]string, 0, len(s))
		for _, p := range s {
			parts = append(parts, fmt.Sprint(p))
		}
		joined := strings.Join(parts, ",")
		return joined, joined != ""
	}
	return "", false
}

func sdcardNumber(raw map[string]any, keys ...string) (float64, bool) {
	v, ok := sdcardField(raw, keys...)
	if !ok {
		return 0, false
	}
	switch n := v.(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case json.Number:
		if f, err := n.Float64(); err == nil {
			return f, true
		}
	case string:
		if f, err := strconv.ParseFloat(strings.TrimSpace(n), 64); err == nil {
			return f, true
		}
	}
	return 0, false
}

func sdcardContext(r *http.Request) (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.Context(), 5*time.Second)
}

// handleSDCardInfo handles GET /api/v1/cameras/{id}/sdcard
func handleSDCardInfo(w http.ResponseWriter, r *http.Request, camID string, streamMgr *StreamManager) {
	if _, ok := streamMgr.GetCamera(camID); !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "camera not found"})
		return
	}
	fetcher, ok := sdcardChannel(cameraSignalingControl(streamMgr, camID))
	if !ok {
		writeJSON(w, http.StatusNotImplemented, map[string]any{"error": "sdcard_unavailable", "message": "The camera is not connected over its control channel. Start a stream, then try again."})
		return
	}
	ctx, cancel := sdcardContext(r)
	defer cancel()
	raw, err := fetcher.GetSDCardInfo(ctx, camID)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": "sdcard_unavailable", "message": err.Error()})
		return
	}
	out := map[string]any{"raw": raw}
	if v, ok := sdcardText(raw, "Status", "status"); ok {
		out["status"] = v
	}
	if v, ok := sdcardNumber(raw, "TotalSizeK", "total_kb", "totalSizeK"); ok {
		out["total_kb"] = v
	}
	if v, ok := sdcardNumber(raw, "UsedSizeK", "used_kb", "usedSizeK"); ok {
		out["used_kb"] = v
	}
	writeJSON(w, http.StatusOK, out)
}

// handleSDCardRecordings handles GET /api/v1/cameras/{id}/sdcard/recordings
func handleSDCardRecordings(w http.ResponseWriter, r *http.Request, camID string, streamMgr *StreamManager) {
	if _, ok := streamMgr.GetCamera(camID); !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "camera not found"})
		return
	}
	fetcher, ok := sdcardChannel(cameraSignalingControl(streamMgr, camID))
	if !ok {
		writeJSON(w, http.StatusNotImplemented, map[string]any{"error": "sdcard_unavailable", "message": "The camera is not connected over its control channel. Start a stream, then try again."})
		return
	}
	ctx, cancel := sdcardContext(r)
	defer cancel()
	raw, err := fetcher.GetRecordDays(ctx, camID)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": "sdcard_unavailable", "message": err.Error()})
		return
	}
	out := map[string]any{"raw": raw}
	for field, keys := range map[string][]string{
		"cur_month":  {"curMonth", "cur_month"},
		"cur_days":   {"curMonDays", "cur_days"},
		"prev_month": {"preMonth", "prevMonth", "pre_month"},
		"prev_days":  {"preMonDays", "prev_days"},
	} {
		if v, ok := sdcardText(raw, keys...); ok {
			out[field] = v
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// handleCameraCloudDays handles GET /api/v1/cameras/{id}/cloud/days?month=YYYY-MM
func handleCameraCloudDays(w http.ResponseWriter, r *http.Request, camID, month string, streamMgr *StreamManager) {
	if _, ok := streamMgr.GetCamera(camID); !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "camera not found"})
		return
	}
	cloud := streamMgr.CameraCloud(camID)
	if cloud == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "not_signed_in", "message": "Sign the camera's Osaio login in first."})
		return
	}
	if month == "" {
		month = time.Now().Format("2006-01")
	}
	days, err := cloud.ListCloudRecordingDays(camID, cloud.UID, month, 1)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"month": month, "days": days})
}

// handleCameraCloudPlayback handles POST /api/v1/cameras/{id}/cloud/playback
// {day: "YYYY-MM-DD", start_time: 1712345678}
func handleCameraCloudPlayback(w http.ResponseWriter, r *http.Request, camID string, streamMgr *StreamManager) {
	if _, ok := streamMgr.GetCamera(camID); !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "camera not found"})
		return
	}
	cloud := streamMgr.CameraCloud(camID)
	if cloud == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "not_signed_in", "message": "Sign the camera's Osaio login in first."})
		return
	}
	var req struct {
		Day       string `json:"day"`
		StartTime int64  `json:"start_time"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil || req.Day == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "send {\"day\": \"YYYY-MM-DD\", \"start_time\": 1234567890}"})
		return
	}
	u, err := cloud.GetPlaybackURL(camID, cloud.UID, req.Day, req.StartTime, 1)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"playback_url": u})
}

// handleSDCardPlayback handles POST /api/v1/cameras/{id}/sdcard/playback
func handleSDCardPlayback(w http.ResponseWriter, r *http.Request, camID string, streamMgr *StreamManager) {
	handleSDCardInfo(w, r, camID, streamMgr)
}

// armTalk turns the camera's talkback on or off. With a live media session it
// goes through the viewer, which is the only path that carries audio; without
// one it arms the control channel, which is all a firewall backend can do.
// It answers in the common shape and reports whether the camera replied.
func armTalk(w http.ResponseWriter, r *http.Request, mc *ManagedCamera, active bool) {
	mc.mu.RLock()
	viewer := mc.Viewer
	mc.mu.RUnlock()

	cc := GatewayControlChannel()
	answered := false
	if viewer != nil {
		ret, err := viewer.SetTalk(active)
		if err != nil {
			action := "stop"
			if active {
				action = "start"
			}
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": fmt.Sprintf("failed to %s talk: %v", action, err)})
			return
		}
		if active && ret != 0 {
			writeJSON(w, http.StatusBadGateway, map[string]any{"error": fmt.Sprintf("the camera refused talkback (code %d); it may already be in a call from the vendor app", ret)})
			return
		}
		answered = true
		if active {
			answered, _, _, _, _ = viewer.TalkDiagnostics()
		}
	} else if cc != nil {
		sessionID := ""
		if active {
			sessionID = fmt.Sprintf("session-%s-%d", mc.UUID, time.Now().Unix())
		}
		if err := cc.ArmTalk(r.Context(), mc.UUID, active, sessionID); err != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": fmt.Sprintf("failed to arm talk: %v", err)})
			return
		}
	} else {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "camera not found or media session not active"})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":              true,
		"camera":          mc.Name,
		"uuid":            mc.UUID,
		"talk_active":     active,
		"camera_answered": answered,
	})
}

// handleTalkStart handles POST /api/v1/cameras/{id}/talk/start
func handleTalkStart(w http.ResponseWriter, r *http.Request, camID string, streamMgr *StreamManager) {
	mc, ok := streamMgr.GetCamera(camID)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "camera not found"})
		return
	}
	armTalk(w, r, mc, true)
}

// handleTalkStop handles POST /api/v1/cameras/{id}/talk/stop
func handleTalkStop(w http.ResponseWriter, r *http.Request, camID string, streamMgr *StreamManager) {
	mc, ok := streamMgr.GetCamera(camID)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "camera not found"})
		return
	}
	armTalk(w, r, mc, false)
}

// handleTalkStream handles POST /api/v1/cameras/{id}/talk/stream
func handleTalkStream(w http.ResponseWriter, r *http.Request, camID string, streamMgr *StreamManager) {
	mc, ok := streamMgr.GetCamera(camID)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "camera not found"})
		return
	}

	mc.mu.RLock()
	viewer := mc.Viewer
	streaming := mc.Streaming
	mc.mu.RUnlock()

	if !streaming || viewer == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "camera not found or media session not active"})
		return
	}

	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": fmt.Sprintf("read audio body: %v", err)})
		return
	}
	if len(bodyBytes) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "empty audio stream (0 bytes)"})
		return
	}

	// Reject WebM/EBML uploads: only raw PCM and AAC are accepted
	if len(bodyBytes) >= 4 && bodyBytes[0] == 0x1A && bodyBytes[1] == 0x45 && bodyBytes[2] == 0xDF && bodyBytes[3] == 0xA3 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid audio payload: legacy WebM payload not supported"})
		return
	}

	// Uploads belong to an explicitly started session. A late upload after
	// Stop must never re-arm the speaker or revive the encoder.
	if !viewer.IsTalkActive() {
		writeJSON(w, http.StatusConflict, map[string]any{"error": "talkback is stopped; start Talk before uploading audio"})
		return
	}

	var frames [][]byte
	ct := strings.ToLower(r.Header.Get("Content-Type"))
	isPCM := strings.Contains(ct, "pcm") || strings.Contains(ct, "audio/l16") || strings.Contains(ct, "audio/raw")

	// Browser PCM (16 kHz s16le mono): one long-running encoder per talk
	// session, matched to the camera's own audio format and paced in real time.
	if isPCM || !(len(bodyBytes) >= 7 && bodyBytes[0] == 0xFF && (bodyBytes[1]&0xF0) == 0xF0) {
		if !viewer.TalkAudioRunning() {
			_ = viewer.SetTalkFormat(bridge.DefaultTalkFormatID)
		}
		if err := viewer.StartActiveTalkAudio(streamMgr.ffmpegPath); err != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": err.Error()})
			return
		}
		if err := viewer.WriteTalkPCM(bodyBytes); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": fmt.Sprintf("talkback encoder: %v", err)})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":             true,
			"camera":         mc.Name,
			"uuid":           mc.UUID,
			"pcm_bytes":      len(bodyBytes),
			"encoder_active": true,
		})
		return
	}

	// Pre-encoded ADTS AAC (API clients): forwarded frame by frame.
	frames, err = bridge.ReadAllADTSFrames(bytes.NewReader(bodyBytes))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": fmt.Sprintf("invalid audio payload: %v", err),
		})
		return
	}

	viewer.TouchAudioUp()

	totalBytes := 0
	for _, frame := range frames {
		totalBytes += len(frame)
		if err := viewer.WriteAudioFrame(frame); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{
				"error": fmt.Sprintf("stream audio frame: %v", err),
			})
			return
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":              true,
		"camera":          mc.Name,
		"uuid":            mc.UUID,
		"frames_streamed": len(frames),
		"bytes_streamed":  totalBytes,
	})
}

// transcodePCMToADTS converts raw 16kHz s16le mono PCM into 16kHz mono AAC-LC ADTS frames via FFmpeg.
func transcodePCMToADTS(ffmpegPath string, pcmData []byte) ([][]byte, error) {
	if ffmpegPath == "" {
		ffmpegPath = "ffmpeg"
	}
	cmd := exec.Command(ffmpegPath,
		"-hide_banner", "-loglevel", "error",
		"-f", "s16le", "-ar", "16000", "-ac", "1",
		"-i", "pipe:0",
		"-c:a", "aac", "-b:a", "32k", "-ar", "16000", "-ac", "1",
		"-f", "adts", "pipe:1",
	)
	hideConsole(cmd)
	cmd.Stdin = bytes.NewReader(pcmData)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("ffmpeg pcm transcode failed: %w (stderr: %s)", err, strings.TrimSpace(stderr.String()))
	}
	return bridge.ReadAllADTSFrames(&stdout)
}

// handleWHEP proxies WebRTC HTTP Egress Protocol requests between browser client and MediaMTX WebRTC server.
func handleWHEP(w http.ResponseWriter, r *http.Request, camID string, streamMgr *StreamManager) {
	mc, ok := streamMgr.GetCamera(camID)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "camera not found"})
		return
	}

	// Same-origin only: the web UI calls this with its session cookie + CSRF
	// token, so no CORS headers are offered.
	w.Header().Set("Accept-Post", "application/sdp")

	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	// ?video=only (the page's muted live view): the video-only copy, which
	// skips the audio/video alignment delay, when it is live.
	path, copyUsed := mc.UUID, "main"
	if r.Method == http.MethodPost && r.URL.Query().Get("video") == "only" {
		mc.mu.RLock()
		viewer := mc.Viewer
		mc.mu.RUnlock()
		if viewer != nil && viewer.ViewerCopyReady() {
			path, copyUsed = mc.UUID+bridge.ViewerPathSuffix, "video-only"
		}
	}
	w.Header().Set("X-BombeCam-Stream", copyUsed)
	targetURL := fmt.Sprintf("%s/%s/whep", strings.TrimRight(streamMgr.WebRTCBase(), "/"), path)

	proxyReq, err := http.NewRequestWithContext(r.Context(), r.Method, targetURL, r.Body)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": fmt.Sprintf("create whep proxy req: %v", err)})
		return
	}

	// Forward only WHEP-relevant headers; never the browser's cookies or CSRF token.
	for _, k := range []string{"Content-Type", "Accept", "If-Match"} {
		if v := r.Header.Get(k); v != "" {
			proxyReq.Header.Set(k, v)
		}
	}

	whepClient := &http.Client{Timeout: 10 * time.Second}
	resp, err := whepClient.Do(proxyReq)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": fmt.Sprintf("whep upstream error: %v", err)})
		return
	}
	defer resp.Body.Close()

	for _, k := range []string{"Content-Type", "ETag", "Link", "Accept-Patch"} {
		if v := resp.Header.Get(k); v != "" {
			w.Header().Set(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

// parseOnOff reads an on/off switch value (1/0, true/false, "on"/"off").
func parseOnOff(v any) (int, bool) {
	switch x := v.(type) {
	case nil:
		return 0, false
	case bool:
		if x {
			return 1, true
		}
		return 0, true
	case float64:
		if x != 0 {
			return 1, true
		}
		return 0, true
	case int:
		if x != 0 {
			return 1, true
		}
		return 0, true
	case string:
		switch strings.ToLower(strings.TrimSpace(x)) {
		case "on", "1", "true", "yes":
			return 1, true
		case "off", "0", "false", "no":
			return 0, true
		}
	}
	return 0, false
}
