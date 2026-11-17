package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"github.com/Fever-r/BombeCam/internal/version"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"

	"github.com/Fever-r/BombeCam/internal/mediamtx"
	"github.com/Fever-r/BombeCam/pkg/bridge"
	"github.com/Fever-r/BombeCam/pkg/profile"
)

// Home Assistant MQTT discovery. When the user enters their MQTT broker
// (usually Home Assistant's Mosquitto add-on), every camera appears in Home
// Assistant as a device with controls and status, no YAML needed. Commands
// from Home Assistant run through the same code as the buttons in BombeCam's
// viewer, so they reach the camera the same way (through Osaio's cloud).
//
// This is separate from the experimental local-control MQTT code
// (pkg/bridge/mosquitto_bridge.go), which talks to the cameras themselves.

const (
	mqttBaseTopic   = "bombecam"
	mqttStatusTopic = mqttBaseTopic + "/status"
	mqttDefaultPort = 1883
	mqttDefaultDisc = "homeassistant"
)

type haBridgeState struct {
	mu        sync.Mutex
	cfg       profile.MQTTSettings
	client    mqtt.Client
	status    string // off, connecting, connected, error
	detail    string
	announced map[string]string // camera ID -> discovery payload last published
	published map[string]string // state topic -> payload last published
	sm        *StreamManager
	loop      bool
}

var haMQTT = &haBridgeState{}

// mqttNewClient is a seam for tests.
var mqttNewClient = mqtt.NewClient

func sameMQTTConfig(a, b profile.MQTTSettings) bool {
	return a.Enabled == b.Enabled && a.Host == b.Host && a.Port == b.Port && a.Username == b.Username &&
		a.Password.Equal(b.Password) && a.DiscoveryPrefix == b.DiscoveryPrefix
}

func discoveryPrefix(c profile.MQTTSettings) string {
	if p := strings.Trim(c.DiscoveryPrefix, "/ "); p != "" {
		return p
	}
	return mqttDefaultDisc
}

func mqttView(c profile.MQTTSettings) integrationMQTTView {
	haMQTT.mu.Lock()
	status, detail := haMQTT.status, haMQTT.detail
	haMQTT.mu.Unlock()
	if !c.Enabled {
		status, detail = "off", ""
	} else if status == "" || status == "off" {
		status = "connecting"
	}
	port := c.Port
	if port == 0 {
		port = mqttDefaultPort
	}
	return integrationMQTTView{Enabled: c.Enabled, Host: c.Host, Port: port, Username: c.Username,
		HasPassword: !c.Password.IsEmpty(), DiscoveryPrefix: discoveryPrefix(c), Status: status, Detail: detail}
}

func mqttClientID() string {
	h, _ := os.Hostname()
	sum := sha256.Sum256([]byte("bombecam:" + h))
	return "bombecam-" + hex.EncodeToString(sum[:4])
}

// mqttSync connects, reconnects or disconnects to match the settings, then
// announces the cameras and publishes their state. It is called by the
// integration loop (every 20 s and after changes).
func mqttSync(sm *StreamManager) {
	cfg := currentIntegrationSettings().MQTT
	b := haMQTT
	b.mu.Lock()
	b.sm = sm
	if !b.loop && sm != nil {
		b.loop = true
		go b.stateLoop()
	}
	if b.client != nil && !sameMQTTConfig(b.cfg, cfg) {
		old, oldPrefix, announced := b.client, discoveryPrefix(b.cfg), b.announced
		b.client = nil
		b.announced, b.published = nil, nil
		b.mu.Unlock()
		if old.IsConnected() {
			if !cfg.Enabled {
				// turned off: take BombeCam's devices out of Home Assistant
				for id := range announced {
					old.Publish(discoveryTopic(oldPrefix, id), 1, true, "").WaitTimeout(2 * time.Second)
				}
			}
			old.Publish(mqttStatusTopic, 1, true, "offline").WaitTimeout(2 * time.Second)
		}
		old.Disconnect(250)
		b.mu.Lock()
	}
	b.cfg = cfg
	if !cfg.Enabled || cfg.Host == "" {
		b.status, b.detail = "off", ""
		b.mu.Unlock()
		return
	}
	if b.client == nil {
		port := cfg.Port
		if port == 0 {
			port = mqttDefaultPort
		}
		opts := mqtt.NewClientOptions().
			AddBroker(fmt.Sprintf("tcp://%s", joinHostPort(cfg.Host, port))).
			SetClientID(mqttClientID()).
			SetUsername(cfg.Username).
			SetPassword(cfg.Password.Expose()).
			SetWill(mqttStatusTopic, "offline", 1, true).
			SetKeepAlive(30 * time.Second).
			SetAutoReconnect(true).
			SetConnectRetry(true).
			SetConnectRetryInterval(10 * time.Second).
			SetMaxReconnectInterval(time.Minute).
			SetConnectTimeout(8 * time.Second).
			SetOrderMatters(false)
		opts.SetOnConnectHandler(b.onConnect)
		opts.SetConnectionLostHandler(func(_ mqtt.Client, err error) {
			b.mu.Lock()
			b.status, b.detail = "connecting", "connection lost: "+err.Error()
			b.announced, b.published = nil, nil
			b.mu.Unlock()
			fmt.Printf("[mqtt] connection to %s lost (%v); reconnecting\n", cfg.Host, err)
		})
		opts.SetReconnectingHandler(func(_ mqtt.Client, _ *mqtt.ClientOptions) {
			b.mu.Lock()
			if b.status != "error" {
				b.status = "connecting"
			}
			b.mu.Unlock()
		})
		b.client = mqttNewClient(opts)
		b.status, b.detail = "connecting", ""
		client := b.client
		b.mu.Unlock()
		fmt.Printf("[mqtt] connecting to the MQTT broker at %s (Home Assistant discovery)\n", joinHostPort(cfg.Host, port))
		tok := client.Connect()
		go func() {
			if tok.WaitTimeout(15*time.Second) && tok.Error() != nil {
				b.mu.Lock()
				if b.client == client && !client.IsConnected() {
					b.status, b.detail = "error", friendlyMQTTError(tok.Error())
				}
				b.mu.Unlock()
			}
		}()
		return
	}
	connected := b.client.IsConnected()
	b.mu.Unlock()
	if connected {
		b.announceAll()
		b.publishStates()
	}
}

func joinHostPort(host string, port int) string {
	if strings.Contains(host, ":") && !strings.HasPrefix(host, "[") {
		return fmt.Sprintf("[%s]:%d", host, port)
	}
	return fmt.Sprintf("%s:%d", host, port)
}

func friendlyMQTTError(err error) string {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "not Authorized") || strings.Contains(msg, "bad user name or password"):
		return "the broker refused the user name or password"
	case strings.Contains(msg, "connection refused"):
		return "nothing answers on that address and port (is the broker running?)"
	case strings.Contains(msg, "i/o timeout") || strings.Contains(msg, "no route"):
		return "the broker did not answer (check the address, and that both devices are on the same network)"
	}
	return msg
}

func (b *haBridgeState) onConnect(c mqtt.Client) {
	b.mu.Lock()
	b.status, b.detail = "connected", ""
	b.announced, b.published = map[string]string{}, map[string]string{}
	prefix := discoveryPrefix(b.cfg)
	b.mu.Unlock()
	fmt.Printf("[mqtt] connected; cameras are announced to Home Assistant under %s/\n", prefix)
	c.Publish(mqttStatusTopic, 1, true, "online")
	c.Subscribe(mqttBaseTopic+"/+/+/set", 1, func(_ mqtt.Client, m mqtt.Message) {
		go b.handleCommand(m.Topic(), string(m.Payload()))
	})
	// Home Assistant says "online" here when it (re)starts: announce again.
	c.Subscribe(prefix+"/status", 1, func(_ mqtt.Client, m mqtt.Message) {
		if string(m.Payload()) == "online" {
			b.mu.Lock()
			b.announced, b.published = map[string]string{}, map[string]string{}
			b.mu.Unlock()
			go func() {
				time.Sleep(2 * time.Second)
				b.announceAll()
				b.publishStates()
			}()
		}
	})
	go func() {
		b.announceAll()
		b.publishStates()
	}()
}

// stateLoop publishes changed states every few seconds.
func (b *haBridgeState) stateLoop() {
	for range time.Tick(4 * time.Second) {
		b.mu.Lock()
		ok := b.client != nil && b.client.IsConnected() && b.status == "connected"
		b.mu.Unlock()
		if ok {
			b.publishStates()
		}
	}
}

// cameraModels is pan/tilt and white spotlight by Osaio model code. It must
// match CAMERA_CAPABILITY_MATRIX in web/app.js (TestCameraModelsMatchWebPage).
var cameraModels = map[string]struct{ ptz, spotlight bool }{
	// Indoor pan/tilt
	"WS03": {true, false}, "P1": {true, false}, "P1PRO": {true, false}, "P5": {true, false}, "P10": {true, false},
	// Indoor fixed
	"WS01": {false, false}, "C1": {false, false}, "C1PRO": {false, false}, "C2": {false, false},
	"GC2": {false, false}, "GC3": {false, false},
	// Outdoor pan/tilt with spotlight
	"K1": {true, true}, "K1PRO": {true, true}, "GK1": {true, true}, "GK1PRO": {true, true}, "GK2": {true, true},
	"GL1": {true, true}, "WS04": {true, true}, "GW30": {true, true}, "GW40": {true, true},
	// Outdoor fixed with spotlight
	"GW1": {false, true},
	// Outdoor fixed (infrared only)
	"WS02": {false, false}, "T1": {false, false}, "T1PRO": {false, false}, "GT1": {false, false}, "GT1PRO": {false, false},
}

// cameraModelKey reduces Osaio's model to a cameraModels key: the part before
// "_" (a hardware revision, as in "GC3_A3S11A3"), upper case, letters and
// digits only. An exact key wins; otherwise the longest key inside it (the
// first alphabetically on a tie, as in app.js).
func cameraModelKey(model string) string {
	base, _, _ := strings.Cut(strings.ToUpper(model), "_")
	base = strings.Map(func(r rune) rune {
		if (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			return r
		}
		return -1
	}, base)
	if base == "" {
		return ""
	}
	if _, ok := cameraModels[base]; ok {
		return base
	}
	best := ""
	for key := range cameraModels {
		if strings.Contains(base, key) && (len(key) > len(best) || len(key) == len(best) && key < best) {
			best = key
		}
	}
	return best
}

// cameraCaps is what a model has. An unknown model gets pan/tilt and no
// spotlight, as on the web page.
func cameraCaps(model string) (ptz, spotlight bool) {
	if c, ok := cameraModels[cameraModelKey(model)]; ok {
		return c.ptz, c.spotlight
	}
	return true, false
}

func camTopic(id, part string) string { return mqttBaseTopic + "/" + id + "/" + part }

// haDiscoveryPayload is the device-based discovery message for one camera
// (Home Assistant 2024.12+: <prefix>/device/<id>/config with "cmps").
func haDiscoveryPayload(id, name, model string) map[string]any {
	ptz, spot := cameraCaps(model)
	uid := "bombecam_" + id
	always := []map[string]string{{"topic": mqttStatusTopic}}
	controls := []map[string]string{{"topic": mqttStatusTopic}, {"topic": camTopic(id, "available")}}
	cmps := map[string]any{
		"night_vision": map[string]any{
			"p": "select", "name": "Night vision", "unique_id": uid + "_night_vision", "icon": "mdi:weather-night",
			"options": []string{"Auto", "Off"}, "state_topic": camTopic(id, "night_vision"), "command_topic": camTopic(id, "night_vision/set"),
			"availability": controls, "availability_mode": "all",
		},
		"status_light": map[string]any{
			"p": "switch", "name": "Status light", "unique_id": uid + "_status_light", "icon": "mdi:led-on",
			"state_topic": camTopic(id, "status_light"), "command_topic": camTopic(id, "status_light/set"),
			"availability": controls, "availability_mode": "all", "entity_category": "config",
		},
		"motion_detection": map[string]any{
			"p": "switch", "name": "Motion detection", "unique_id": uid + "_motion_detection", "icon": "mdi:motion-sensor",
			"state_topic": camTopic(id, "motion_detection"), "command_topic": camTopic(id, "motion_detection/set"),
			"availability": controls, "availability_mode": "all", "entity_category": "config",
		},
		"sound_detection": map[string]any{
			"p": "switch", "name": "Sound detection", "unique_id": uid + "_sound_detection", "icon": "mdi:ear-hearing",
			"state_topic": camTopic(id, "sound_detection"), "command_topic": camTopic(id, "sound_detection/set"),
			"availability": controls, "availability_mode": "all", "entity_category": "config",
		},
		"stream": map[string]any{
			"p": "binary_sensor", "name": "Stream online", "unique_id": uid + "_stream", "device_class": "connectivity",
			"state_topic": camTopic(id, "stream"), "availability": always,
		},
		"controls": map[string]any{
			"p": "binary_sensor", "name": "Controls connected", "unique_id": uid + "_controls", "device_class": "connectivity",
			"state_topic": camTopic(id, "controls"), "availability": always, "entity_category": "diagnostic",
		},
		"connection": map[string]any{
			"p": "sensor", "name": "Connection", "unique_id": uid + "_connection", "icon": "mdi:lan-connect",
			"state_topic": camTopic(id, "connection"), "availability": always, "entity_category": "diagnostic",
		},
	}
	if spot {
		cmps["spotlight"] = map[string]any{
			"p": "switch", "name": "Spotlight", "unique_id": uid + "_spotlight", "icon": "mdi:light-flood-down",
			"state_topic": camTopic(id, "spotlight"), "command_topic": camTopic(id, "spotlight/set"),
			"availability": controls, "availability_mode": "all",
		}
	}
	if ptz {
		for _, d := range []struct{ key, name, icon string }{
			{"left", "Pan left", "mdi:arrow-left-bold"}, {"right", "Pan right", "mdi:arrow-right-bold"},
			{"up", "Tilt up", "mdi:arrow-up-bold"}, {"down", "Tilt down", "mdi:arrow-down-bold"},
		} {
			key := map[string]string{"left": "pan_left", "right": "pan_right", "up": "tilt_up", "down": "tilt_down"}[d.key]
			cmps[key] = map[string]any{
				"p": "button", "name": d.name, "unique_id": uid + "_" + key, "icon": d.icon,
				"command_topic": camTopic(id, "ptz/set"), "payload_press": d.key,
				"availability": controls, "availability_mode": "all",
			}
		}
	}
	return map[string]any{
		"dev": map[string]any{
			"ids": []string{uid}, "name": name, "mf": "Osaio", "mdl": model, "sw": "BombeCam " + version.Version,
		},
		"o":    map[string]any{"name": "BombeCam", "sw": version.Version},
		"cmps": cmps,
		"qos":  1,
	}
}

func discoveryTopic(prefix, id string) string {
	return prefix + "/device/bombecam_" + id + "/config"
}

// announceAll publishes discovery for every camera (only when it changed)
// and removes cameras that are gone.
func (b *haBridgeState) announceAll() {
	b.mu.Lock()
	client, sm := b.client, b.sm
	if client == nil || sm == nil || !client.IsConnected() {
		b.mu.Unlock()
		return
	}
	prefix := discoveryPrefix(b.cfg)
	if b.announced == nil {
		b.announced = map[string]string{}
	}
	announced := make(map[string]string, len(b.announced))
	for k, v := range b.announced {
		announced[k] = v
	}
	b.mu.Unlock()

	present := map[string]bool{}
	for _, mc := range sm.GetAllCameras() {
		mc.mu.RLock()
		id, name, model := mc.UUID, mc.Name, mc.Model
		mc.mu.RUnlock()
		present[id] = true
		payload, _ := json.Marshal(haDiscoveryPayload(id, name, model))
		if announced[id] == string(payload) {
			continue
		}
		if t := client.Publish(discoveryTopic(prefix, id), 1, true, payload); t.WaitTimeout(5*time.Second) && t.Error() == nil {
			announced[id] = string(payload)
		}
	}
	// the profile remembers removed cameras nowhere; forget any we announced
	for id := range announced {
		if !present[id] {
			client.Publish(discoveryTopic(prefix, id), 1, true, "").WaitTimeout(5 * time.Second)
			for _, part := range []string{"night_vision", "status_light", "motion_detection", "sound_detection", "spotlight", "stream", "controls", "connection", "available"} {
				client.Publish(camTopic(id, part), 1, true, "")
			}
			delete(announced, id)
		}
	}
	b.mu.Lock()
	b.announced = announced
	b.mu.Unlock()
}

func onOffPayload(v any) string {
	if n, ok := parseOnOff(v); ok {
		if n == 1 {
			return "ON"
		}
		return "OFF"
	}
	return ""
}

// cameraMQTTState reads a camera's state from what the camera last reported
// (the same readback the viewer shows). It never asks the camera itself.
func cameraMQTTState(sm *StreamManager, id string, ready map[string]bool) map[string]string {
	st := map[string]string{}
	mc, ok := sm.GetCamera(id)
	if !ok {
		return st
	}
	mc.mu.RLock()
	transport := mc.Transport
	mc.mu.RUnlock()
	stream := ready[id]
	st["stream"] = map[bool]string{true: "ON", false: "OFF"}[stream]
	switch {
	case strings.Contains(transport, "LAN"):
		st["connection"] = "Direct (LAN)"
	case strings.Contains(transport, "internet"):
		st["connection"] = "Direct (internet)"
	case strings.Contains(transport, "Relay"):
		st["connection"] = "Relayed via Osaio"
	case strings.EqualFold(transport, "Stopped"):
		st["connection"] = "Stopped"
	default:
		st["connection"] = "Connecting"
	}
	controls := false
	cs := signalingFor(id)
	if cs != nil && cs.sig != nil && cs.sig.Connected() {
		controls = true
	}
	st["controls"] = map[bool]string{true: "ON", false: "OFF"}[controls]
	st["available"] = map[bool]string{true: "online", false: "offline"}[controls]

	if st["night_vision"] == "" {
		if v, _, ok := cameraSetting(id, "IrLedMode"); ok {
			if m, ok := bridge.ParseIRMode(v); ok {
				st["night_vision"] = map[string]string{"auto": "Auto", "off": "Off"}[bridge.IRModeName(m)]
			}
		}
	}
	if st["status_light"] == "" {
		if v, _, ok := cameraSetting(id, "LedOnOff"); ok {
			st["status_light"] = onOffPayload(v)
		}
	}
	for key, part := range map[string]string{"MotionDetectSW": "motion_detection", "SoundDetectSW": "sound_detection", "LightSW": "spotlight"} {
		if v, _, ok := cameraSetting(id, key); ok {
			st[part] = onOffPayload(v)
		}
	}
	for k, v := range st {
		if v == "" {
			delete(st, k)
		}
	}
	return st
}

// readyPaths lists the camera paths MediaMTX has live (one API call).
func readyPaths() map[string]bool {
	out := map[string]bool{}
	_, apiBase := currentMediaRuntime()
	resp, err := (&http.Client{Timeout: 2 * time.Second}).Get(strings.TrimRight(apiBase, "/") + "/v3/paths/list?itemsPerPage=1000")
	if err != nil {
		return out
	}
	defer resp.Body.Close()
	var list struct {
		Items []mediamtx.PathInfo `json:"items"`
	}
	if json.NewDecoder(resp.Body).Decode(&list) == nil {
		for _, it := range list.Items {
			if it.Ready {
				out[it.Name] = true
			}
		}
	}
	return out
}

var lastAttrRefresh sync.Map // camera ID -> time.Time

// publishStates publishes every state that changed (retained, so Home
// Assistant has it after a restart).
func (b *haBridgeState) publishStates() {
	b.mu.Lock()
	client, sm := b.client, b.sm
	if client == nil || sm == nil || !client.IsConnected() {
		b.mu.Unlock()
		return
	}
	if b.published == nil {
		b.published = map[string]string{}
	}
	b.mu.Unlock()
	ready := readyPaths()
	cams := sm.GetAllCameras()
	sort.Slice(cams, func(i, j int) bool { return cams[i].UUID < cams[j].UUID })
	for _, mc := range cams {
		id := mc.UUID
		// keep the camera's reported settings fresh (at most once a minute)
		if t, ok := lastAttrRefresh.Load(id); !ok || time.Since(t.(time.Time)) > time.Minute {
			lastAttrRefresh.Store(id, time.Now())
			refreshCameraSettings(id, "IrLedMode", "LedOnOff", "MotionDetectSW", "SoundDetectSW")
		}
		for part, v := range cameraMQTTState(sm, id, ready) {
			topic := camTopic(id, part)
			b.mu.Lock()
			same := b.published[topic] == v
			b.mu.Unlock()
			if same {
				continue
			}
			if t := client.Publish(topic, 1, true, v); t.WaitTimeout(5*time.Second) && t.Error() == nil {
				b.mu.Lock()
				if b.published != nil {
					b.published[topic] = v
				}
				b.mu.Unlock()
			}
		}
	}
}

// discardWriter is a minimal http.ResponseWriter for running a control
// handler in-process.
type discardWriter struct {
	header http.Header
	code   int
	body   bytes.Buffer
}

func (w *discardWriter) Header() http.Header {
	if w.header == nil {
		w.header = http.Header{}
	}
	return w.header
}
func (w *discardWriter) Write(p []byte) (int, error) { return w.body.Write(p) }
func (w *discardWriter) WriteHeader(code int)        { w.code = code }

// runControl runs one of the viewer's control handlers in-process.
func runControl(handler func(http.ResponseWriter, *http.Request), body any) (int, string) {
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPost, "http://127.0.0.1/internal/mqtt", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	w := &discardWriter{code: http.StatusOK}
	handler(w, req)
	return w.code, w.body.String()
}

// handleCommand runs a command from Home Assistant
// (bombecam/<camera ID>/<entity>/set).
func (b *haBridgeState) handleCommand(topic, payload string) {
	parts := strings.Split(topic, "/")
	if len(parts) != 4 || parts[0] != mqttBaseTopic || parts[3] != "set" {
		return
	}
	id, entity := parts[1], parts[2]
	b.mu.Lock()
	sm := b.sm
	b.mu.Unlock()
	if sm == nil {
		return
	}
	if _, ok := sm.GetCamera(id); !ok {
		return
	}
	payload = strings.TrimSpace(payload)
	onoff := func() (string, int, bool) {
		switch strings.ToUpper(payload) {
		case "ON":
			return "on", 1, true
		case "OFF":
			return "off", 0, true
		}
		return "", 0, false
	}
	control := func(w http.ResponseWriter, r *http.Request) { handleCameraControl(w, r, id, sm) }
	var code int
	var resp string
	switch entity {
	case "night_vision":
		mode := strings.ToLower(payload)
		if mode != "auto" && mode != "off" {
			return
		}
		code, resp = runControl(control, map[string]any{"action": "ir", "mode": mode})
	case "status_light", "motion_detection", "sound_detection", "spotlight":
		mode, val, ok := onoff()
		if !ok {
			return
		}
		action := map[string]string{"status_light": "led", "motion_detection": "motion", "sound_detection": "sound", "spotlight": "light"}[entity]
		code, resp = runControl(control, map[string]any{"action": action, "mode": mode, "value": val})
	case "ptz":
		dir := strings.ToLower(payload)
		if dir != "left" && dir != "right" && dir != "up" && dir != "down" {
			return
		}
		code, resp = runControl(func(w http.ResponseWriter, r *http.Request) { handleDedicatedPTZ(w, r, id, sm) },
			map[string]any{"direction": dir, "duration_ms": 400})
	default:
		return
	}
	if code >= 300 {
		msg := resp
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal([]byte(resp), &e) == nil && e.Error != "" {
			msg = e.Error
		}
		fmt.Printf("[mqtt] Home Assistant %s -> %s: failed (%s)\n", entity, payload, strings.TrimSpace(msg))
	}
	// publish what the camera now reports (and correct an optimistic UI)
	b.mu.Lock()
	if b.published != nil {
		delete(b.published, camTopic(id, entity))
	}
	connected := b.client != nil && b.client.IsConnected()
	b.mu.Unlock()
	if connected {
		time.Sleep(700 * time.Millisecond)
		b.publishStates()
	}
}
