package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"github.com/Fever-r/BombeCam/internal/version"
	"io"
	"math/rand"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/Fever-r/BombeCam/internal/mediamtx"
	"github.com/Fever-r/BombeCam/pkg/auth"
	"github.com/Fever-r/BombeCam/pkg/bridge"
	shadowshim "github.com/Fever-r/BombeCam/pkg/bridge/shadow_shim"
	"github.com/Fever-r/BombeCam/pkg/netstack"
	"github.com/Fever-r/BombeCam/pkg/profile"
)

var (
	gwCtrlMu      sync.RWMutex
	gwControlChan bridge.ControlChannel
)

// SetGatewayControlChannel binds the active ControlChannel for the gateway
func SetGatewayControlChannel(cc bridge.ControlChannel) {
	gwCtrlMu.Lock()
	gwControlChan = cc
	gwCtrlMu.Unlock()

	sigMu.RLock()
	defer sigMu.RUnlock()
	for _, cs := range sigMap {
		if cs.viewer != nil {
			cs.viewer.SetControlChannel(cc)
		}
	}
}

// GatewayControlChannel returns the currently bound ControlChannel
func GatewayControlChannel() bridge.ControlChannel {
	gwCtrlMu.RLock()
	defer gwCtrlMu.RUnlock()
	return gwControlChan
}

var (
	gwAuthMu          sync.RWMutex
	gwOperatorManager *auth.OperatorManager
)

// SetGatewayOperatorManager registers the local operator auth manager.
func SetGatewayOperatorManager(om *auth.OperatorManager) {
	gwAuthMu.Lock()
	defer gwAuthMu.Unlock()
	gwOperatorManager = om
}

// GatewayOperatorManager returns the active operator auth manager.
func GatewayOperatorManager() *auth.OperatorManager {
	gwAuthMu.RLock()
	defer gwAuthMu.RUnlock()
	return gwOperatorManager
}

type CamSignaling struct {
	dev    bridge.Device
	sig    *bridge.Signaling
	viewer *bridge.Viewer

	mu            sync.Mutex
	lastAttrQuery time.Time
}

// cameraSetting exposes only fresh, correlated camera readbacks.
func cameraSetting(uuid, key string) (any, string, bool) {
	sigMu.RLock()
	cs := sigMap[uuid]
	sigMu.RUnlock()
	if cs == nil || cs.sig == nil {
		return nil, "", false
	}
	v, ok := cs.sig.FreshAttributes(uuid, 30*time.Second)[key]
	return v, "camera", ok
}

// refreshCameraSettings asks the camera for attributes (at most every 3 s).
func refreshCameraSettings(uuid string, keys ...string) {
	sigMu.RLock()
	cs := sigMap[uuid]
	sigMu.RUnlock()
	if cs == nil || cs.sig == nil {
		return
	}
	cs.mu.Lock()
	if time.Since(cs.lastAttrQuery) < 3*time.Second {
		cs.mu.Unlock()
		return
	}
	cs.lastAttrQuery = time.Now()
	cs.mu.Unlock()
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_, _ = cs.sig.QueryAttributes(ctx, uuid, keys...)
	}()
}

var (
	sigMu  sync.RWMutex
	sigMap = make(map[string]*CamSignaling)
)

func registerSig(dev bridge.Device, sig *bridge.Signaling, v *bridge.Viewer) {
	if sig != nil {
		sig.BindCamera(dev.UUID, dev.Type)
	}
	sigMu.Lock()
	defer sigMu.Unlock()
	sigMap[dev.UUID] = &CamSignaling{
		dev:    dev,
		sig:    sig,
		viewer: v,
	}
}

func unregisterSig(uuid string) {
	sigMu.Lock()
	defer sigMu.Unlock()
	delete(sigMap, uuid)
}

// cameraControl returns the channel that carries commands for a camera: the
// gateway's configured channel or, before one is set (some tests), the
// camera's own signaling connection. Handlers send commands only through it.
func cameraControl(streamMgr *StreamManager, camID string) bridge.ControlChannel {
	if cc := GatewayControlChannel(); cc != nil {
		return cc
	}
	if streamMgr != nil {
		if mc, ok := streamMgr.GetCamera(camID); ok && mc != nil {
			mc.mu.RLock()
			sig := mc.Signaling
			mc.mu.RUnlock()
			if sig != nil {
				return bridge.NewSignalingControlChannel(sig)
			}
		}
	}
	if cs := findTargetCam(camID); cs != nil && cs.sig != nil {
		return bridge.NewSignalingControlChannel(cs.sig)
	}
	return nil
}

func getEnvOrDefault(envKey, defVal string) string {
	if val := os.Getenv(envKey); val != "" {
		return val
	}
	return defVal
}

// envInt reads a whole number from the environment.
func envInt(key string, def int) int {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

// anyFlagSet reports whether any of the named flags was given on the command line.
func anyFlagSet(names ...string) bool {
	set := false
	flag.Visit(func(f *flag.Flag) {
		for _, n := range names {
			if f.Name == n {
				set = true
			}
		}
	})
	return set
}

// anyEnvSet reports whether any of the environment variables is non-empty.
func anyEnvSet(keys ...string) bool {
	for _, k := range keys {
		if strings.TrimSpace(os.Getenv(k)) != "" {
			return true
		}
	}
	return false
}

// envDuration reads a duration such as "90s" or "2m" from the environment.
func envDuration(key string, def time.Duration) time.Duration {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return def
}

func getEnvFirst(keys []string, defVal string) string {
	for _, k := range keys {
		if val := os.Getenv(k); val != "" {
			return val
		}
	}
	return defVal
}

var (
	sessionsMu sync.RWMutex
	sessions   = make(map[string]*CameraSession)
)

func registerCameraSession(s *CameraSession) {
	sessionsMu.Lock()
	defer sessionsMu.Unlock()
	sessions[s.UUID] = s
}

func unregisterCameraSession(uuid string) {
	sessionsMu.Lock()
	defer sessionsMu.Unlock()
	delete(sessions, uuid)
}

func findCameraSession(cam string) *CameraSession {
	sessionsMu.RLock()
	defer sessionsMu.RUnlock()
	if cam != "" {
		for uuid, s := range sessions {
			if uuid == cam || strings.EqualFold(s.Name, cam) {
				return s
			}
		}
		camLower := strings.ToLower(cam)
		for uuid, s := range sessions {
			if strings.Contains(strings.ToLower(uuid), camLower) || strings.Contains(strings.ToLower(s.Name), camLower) {
				return s
			}
		}
	}
	return nil
}

func findTargetCam(cam string) *CamSignaling {
	sigMu.RLock()
	defer sigMu.RUnlock()
	if cam != "" {
		for uuid, cs := range sigMap {
			if uuid == cam || strings.EqualFold(cs.dev.Name, cam) {
				return cs
			}
		}
		camLower := strings.ToLower(cam)
		for uuid, cs := range sigMap {
			if strings.Contains(strings.ToLower(uuid), camLower) || strings.Contains(strings.ToLower(cs.dev.Name), camLower) {
				return cs
			}
		}
	} else {
		for _, cs := range sigMap {
			return cs
		}
	}
	return nil
}

func startControlAPI(addr string, sessionMgr *SessionManager, streamMgr *StreamManager) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		fmt.Printf("[control] HTTP server error: %v\n", err)
		return
	}
	serveControlAPI(ln, sessionMgr, streamMgr)
}

func serveControlAPI(ln net.Listener, sessionMgr *SessionManager, streamMgr *StreamManager) {
	mux := SetupAPIMux(sessionMgr, streamMgr)
	handler := SecurityBoundaryHandler(mux)
	fmt.Printf("[control] HTTP API listening on %s (try: GET /api/v1/onboarding/status, POST /api/v1/onboarding/setup, GET /api/v1/privacy, GET /api/v1/inventory/discover)\n", ln.Addr())
	if err := http.Serve(ln, handler); err != nil {
		fmt.Printf("[control] HTTP server error: %v\n", err)
	}
}

// browserURL turns a listen address into the URL to open locally.
func browserURL(listenAddr string) string {
	launchAddr := listenAddr
	if strings.HasPrefix(launchAddr, "0.0.0.0") || strings.HasPrefix(launchAddr, ":") {
		_, port, _ := net.SplitHostPort(launchAddr)
		if port == "" {
			port = "8654"
		}
		launchAddr = "127.0.0.1:" + port
	}
	return fmt.Sprintf("http://%s/", launchAddr)
}

func openBrowser(targetURL string) {
	if runtime.GOOS != "windows" {
		return
	}
	cmd := exec.Command("rundll32", "url.dll,FileProtocolHandler", targetURL)
	cmd.Dir = os.TempDir()
	if err := cmd.Start(); err != nil {
		fmt.Printf("[ui] failed to auto-launch browser: %v (open %s manually)\n", err, targetURL)
	} else {
		fmt.Printf("[ui] launched web interface in default browser: %s\n", targetURL)
	}
}

// isBombeCamStatus reports whether resp is BombeCam's onboarding status reply.
func isBombeCamStatus(resp *http.Response) bool {
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false
	}
	var body map[string]any
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&body); err != nil {
		return false
	}
	_, ok := body["has_profile"]
	return ok
}

// claimControlPort binds the web UI port before anything else starts. If
// another BombeCam already owns it (e.g. a window left open), that instance's
// page is shown (an open tab is reused) and this one exits, instead of starting a second
// gateway whose UI can never be reached.
func claimControlPort(addr string, headless bool) net.Listener {
	ln, err := net.Listen("tcp", addr)
	if err == nil {
		return ln
	}
	target := browserURL(addr)
	client := &http.Client{Timeout: 2 * time.Second}
	if resp, herr := client.Get(target + "api/v1/onboarding/status"); herr == nil && isBombeCamStatus(resp) {
		fmt.Printf("[control] BombeCam is already running at %s; opening it and exiting this copy.\n", target)
		fmt.Printf("[control] (Close the other BombeCam window first if you meant to restart it.)\n")
		if !headless {
			showPage(target)
		}
		time.Sleep(3 * time.Second)
		os.Exit(0)
	}
	fmt.Printf("[control] ERROR: cannot use %s for the web interface: %v\n", addr, err)
	fmt.Printf("[control] Another program is using that port. Close it, or start BombeCam with -http 127.0.0.1:<other port>.\n")
	if !headless {
		fatalNotice(fmt.Sprintf("BombeCam can't start: another program is using %s, the address of BombeCam's page.\n\nClose that program and open BombeCam again.", addr))
	}
	time.Sleep(10 * time.Second)
	os.Exit(1)
	return nil
}

// SecurityBoundaryHandler validates Host headers against DNS rebinding,
// enforces Origin and CSRF boundaries on state-mutating requests, and asks
// for the administrator's sign-in wherever signInRequired says so. The page's
// own files and the sign-in endpoints stay reachable so the page can sign in.
func SecurityBoundaryHandler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		om := GatewayOperatorManager()
		if om != nil {
			isCSRFEndpoint := r.URL.Path == "/api/v1/auth/csrf" || r.URL.Path == "/api/v1/operator/csrf"
			switch {
			case isCSRFEndpoint || isStaticAsset(r):
				if !om.ValidateHost(r.Host) {
					w.WriteHeader(http.StatusForbidden)
					return
				}
			case publicAPIPaths[r.URL.Path]:
				if !om.ValidateSecurityBoundary(w, r, false) {
					return
				}
			default:
				if !om.ValidateSecurityBoundary(w, r, signInRequired(r)) {
					return
				}
			}
		}
		next.ServeHTTP(w, r)
	})
}

// applyTimezoneConfig applies timezone name and numeric offset to the cloud client.
// An empty tzFlag or offsetFlag string indicates unconfigured / auto-detected status.
func applyTimezoneConfig(c *bridge.Cloud, tzFlag, offsetFlag string) {
	if c == nil {
		return
	}
	if tz := strings.TrimSpace(tzFlag); tz != "" {
		c.TimezoneName = tz
	}
	if rawOffset := strings.TrimSpace(offsetFlag); rawOffset != "" {
		if offset, err := strconv.ParseFloat(rawOffset, 64); err == nil {
			if offset >= -12.0 && offset <= 14.0 {
				c.ZoneOffset = offset
			} else {
				fmt.Printf("[config] timezone offset %.2f out of valid range [-12, +14] (using auto-detected offset %.1f)\n", offset, c.ZoneOffset)
			}
		} else {
			fmt.Printf("[config] invalid timezone offset %q: %v (using auto-detected offset %.1f)\n", rawOffset, err, c.ZoneOffset)
		}
	}
}

func main() {
	attachParentConsole()
	emailF := flag.String("email", getEnvOrDefault("OSAIO_EMAIL", ""), "OSAIO account (or env OSAIO_EMAIL)")
	passF := flag.String("password", getEnvOrDefault("OSAIO_PASSWORD", ""), "password (or env OSAIO_PASSWORD)")
	country := flag.String("country", getEnvOrDefault("OSAIO_COUNTRY", ""), "account country dialing code (required with credentials; or env OSAIO_COUNTRY)")
	deviceSel := flag.String("device", getEnvOrDefault("OSAIO_DEVICE", ""), "camera uuid (default: none, selective enrollment via API)")
	enrolledF := flag.String("enrolled", getEnvOrDefault("OSAIO_ENROLLED", ""), "comma-separated list of enrolled camera UUIDs")
	ffmpeg := flag.String("ffmpeg", getEnvOrDefault("FFMPEG_PATH", "ffmpeg"), "path to ffmpeg")
	rtspBase := flag.String("rtsp-base", getEnvOrDefault("OSAIO_RTSP_BASE", "rtsp://127.0.0.1:8554"), "mediamtx RTSP base (or env OSAIO_RTSP_BASE)")
	publishRTSPBase := flag.String("publish-rtsp-base", getEnvFirst([]string{"BOMBECAM_RTSP_PUBLISH_BASE", "OSAIO_RTSP_PUBLISH_BASE"}, "rtsp://127.0.0.1:8554"), "Internal MediaMTX RTSP publishing target for FFmpeg (default: rtsp://127.0.0.1:8554)")
	consumerRTSPBase := flag.String("consumer-rtsp-base", getEnvFirst([]string{"BOMBECAM_RTSP_CONSUMER_BASE", "OSAIO_ADVERTISED_RTSP_BASE"}, ""), "External consumer RTSP base for LAN consumers/NVRs (default: auto-derived from Host header)")
	consumerHLSBase := flag.String("consumer-hls-base", getEnvFirst([]string{"BOMBECAM_HLS_CONSUMER_BASE", "OSAIO_ADVERTISED_HLS_BASE"}, ""), "External consumer HLS base for LAN consumers (default: auto-derived from Host header)")
	hlsBase := flag.String("hls-base", getEnvOrDefault("OSAIO_HLS_BASE", "http://127.0.0.1:8888"), "mediamtx HLS base (or env OSAIO_HLS_BASE)")
	webrtcBase := flag.String("webrtc-base", getEnvOrDefault("BOMBECAM_WEBRTC_BASE", "http://127.0.0.1:8889"), "mediamtx WebRTC/WHEP base (or env BOMBECAM_WEBRTC_BASE)")
	mediamtxPathFlag := flag.String("mediamtx-path", getEnvOrDefault("BOMBECAM_MEDIAMTX_PATH", ""), "path to mediamtx binary; empty auto-resolves")
	mediamtxConfigFlag := flag.String("mediamtx-config", getEnvOrDefault("BOMBECAM_MEDIAMTX_CONFIG", ""), "custom mediamtx.yml (default: a built-in configuration generated on each launch)")
	mediamtxAPIFlag := flag.String("mediamtx-api", getEnvOrDefault("BOMBECAM_MEDIAMTX_API", "http://127.0.0.1:9997"), "MediaMTX management API base URL")
	publisherFlag := flag.String("publisher", getEnvOrDefault("BOMBECAM_PUBLISHER", "auto"), "how camera media reaches MediaMTX: auto/native (built-in, default) or ffmpeg (legacy FFmpeg pipeline)")
	noSupervisorEnv := strings.ToLower(getEnvOrDefault("BOMBECAM_NO_MEDIAMTX_SUPERVISOR", "false"))
	noMediamtxSupervisorFlag := flag.Bool("no-mediamtx-supervisor", noSupervisorEnv == "true" || noSupervisorEnv == "1" || noSupervisorEnv == "yes", "disable automatic MediaMTX supervisor")
	httpAddr := flag.String("http", getEnvOrDefault("OSAIO_HTTP", "127.0.0.1:8654"), "HTTP control API listen address (or env OSAIO_HTTP)")
	// Camera controls go through the vendor signaling WebSocket by default on
	// every platform. The local MQTT shadow shim only works once cameras trust a
	// local broker, so it stays opt-in (-control-transport mqtt).
	localTestFlag := flag.Bool("local-control-test", false, "use local MQTT controls without cloud fallback; close signaling after LAN media starts; manual stream recovery")
	defaultControlTransport := "cloud"
	controlTransport := flag.String("control-transport", getEnvOrDefault("CONTROL_TRANSPORT", defaultControlTransport), "control transport: 'mqtt' (local shadow shim) or 'cloud' (vendor signaling)")
	cameraIPFlag := flag.String("camera-ip", getEnvOrDefault("CAMERA_IP", ""), "explicit camera LAN IPs, comma-separated; empty means learn them from the stream")
	cameraSubnetFlag := flag.String("camera-subnet", getEnvOrDefault("CAMERA_SUBNET", ""), "Camera LAN subnet(s), comma-separated; empty auto-detects host subnets")
	timezoneFlag := flag.String("timezone", getEnvOrDefault("OSAIO_TIMEZONE", ""), "Account timezone name (e.g. America/New_York); empty auto-detects local timezone")
	zoneOffsetFlag := flag.String("timezone-offset", getEnvOrDefault("OSAIO_ZONE_OFFSET", ""), "Account timezone offset hours (e.g. -5, 0, 5.5); empty auto-detects local offset")
	mosquittoURLFlag := flag.String("mosquitto-url", getEnvOrDefault("MOSQUITTO_URL", "tcp://127.0.0.1:1883"), "Mosquitto broker URL for local shadow shim")
	rtspPortFlag := flag.Int("rtsp-port", envInt("BOMBECAM_RTSP_PORT", 0), "RTSP port for the built-in MediaMTX (default 8554, or the port chosen in the web UI)")
	hlsPortFlag := flag.Int("hls-port", envInt("BOMBECAM_HLS_PORT", 0), "HLS port for the built-in MediaMTX (default 8888)")
	webrtcPortFlag := flag.Int("webrtc-port", envInt("BOMBECAM_WEBRTC_PORT", 0), "WebRTC port for the built-in MediaMTX (default 8889)")
	webrtcUDPPortFlag := flag.Int("webrtc-udp-port", envInt("BOMBECAM_WEBRTC_UDP_PORT", 0), "WebRTC media UDP port for the built-in MediaMTX (default 8189)")
	recordingsDirFlag := flag.String("recordings-dir", getEnvOrDefault("BOMBECAM_RECORDINGS_DIR", ""), "directory to record published streams to (one subfolder per stream); empty disables recording")
	recordingsRetentionFlag := flag.Int("recordings-retention-days", envInt("BOMBECAM_RECORDINGS_RETENTION_DAYS", 0), "delete recorded segments older than this many days (0 keeps everything)")
	recordingsCleanupFlag := flag.Duration("recordings-cleanup-interval", envDuration("BOMBECAM_RECORDINGS_CLEANUP_INTERVAL", time.Hour), "how often to prune old recorded segments")
	retryMaxWaitFlag := flag.Duration("retry-max-wait", envDuration("BOMBECAM_RETRY_MAX_WAIT", DefaultRetryMaxWait), "longest wait between automatic stream retries after 3 failed starts (30s, 1m, then this)")
	strictManualFlag := flag.Bool("strict-manual", getEnvOrDefault("STRICT_MANUAL", "false") == "true", "after 3 failed stream attempts, wait for a manual retry instead of backing off and retrying")
	headlessEnv := getEnvOrDefault("BOMBECAM_HEADLESS", "false")
	headlessFlag := flag.Bool("headless", headlessEnv == "true" || headlessEnv == "1", "Disable automatic browser launch on Windows")
	startupFlag := flag.Bool("startup", false, "started with Windows at sign-in: don't open the browser (the tray icon opens the page)")
	versionFlag := flag.Bool("version", false, "print the version and exit")
	flag.Parse()
	if *versionFlag {
		fmt.Println("BombeCam " + version.Version)
		return
	}
	gatewayHeadless.Store(*headlessFlag)
	if *localTestFlag {
		*controlTransport = "mqtt"
		*strictManualFlag = true
	}
	if *controlTransport != "mqtt" && *controlTransport != "cloud" {
		fmt.Fprintln(os.Stderr, "invalid control transport: use mqtt or cloud")
		os.Exit(2)
	}
	localControlTestActive.Store(*localTestFlag)

	controlListener := claimControlPort(*httpAddr, *headlessFlag)
	if logPath := setupGatewayLog(); logPath != "" {
		fmt.Printf("[bombecam] v%s starting; log file: %s\n", version.Version, logPath)
	}

	// The encrypted profile first: its Frigate / Home Assistant settings
	// (ports, stream password) decide how MediaMTX is configured.
	pm, pmErr := profile.NewDefaultManager(*profilePathFlag, *profileKeyFlag, *profileKeyFileFlag)
	var startProf *profile.Profile
	if pmErr == nil && pm != nil {
		if has, herr := pm.HasProfile(context.Background()); herr == nil && has {
			startProf, _ = pm.Load(context.Background())
		}
	}
	integ := profile.IntegrationSettings{}
	if startProf != nil {
		integ = startProf.Integrations
	}
	portFlagsSet := false
	for _, f := range []struct {
		v   int
		dst *int
	}{{*rtspPortFlag, &integ.RTSPPort}, {*hlsPortFlag, &integ.HLSPort}, {*webrtcPortFlag, &integ.WebRTCPort}, {*webrtcUDPPortFlag, &integ.WebRTCICEPort}} {
		if f.v > 0 {
			*f.dst = f.v
			portFlagsSet = true
		}
	}
	mediaBasesSet := anyFlagSet("rtsp-base", "publish-rtsp-base", "hls-base", "webrtc-base") ||
		anyEnvSet("OSAIO_RTSP_BASE", "BOMBECAM_RTSP_PUBLISH_BASE", "OSAIO_RTSP_PUBLISH_BASE", "OSAIO_HLS_BASE", "BOMBECAM_WEBRTC_BASE")
	switch {
	case *noMediamtxSupervisorFlag:
		setPortsLocked(true, "MediaMTX runs outside BombeCam here (Docker): its ports and passwords are set in its own configuration (deploy/mediamtx.yml).")
	case *mediamtxConfigFlag != "":
		setPortsLocked(true, "BombeCam is using your own MediaMTX configuration (-mediamtx-config): set ports and passwords there.")
	case mediaBasesSet:
		setPortsLocked(true, "The video server addresses were set when BombeCam was started (command line or environment), so ports can't be changed here.")
	case portFlagsSet:
		setPortsLocked(true, "Ports were set when BombeCam was started (-rtsp-port and friends), so they can't be changed here.")
	}

	var mtxSupervisor *mediamtx.Supervisor
	if !*noMediamtxSupervisorFlag {
		mtxCfg := mediamtx.DefaultConfig()
		mtxCfg.APIPort = portOfURL(*mediamtxAPIFlag, mediamtx.DefaultAPIPort)
		if *mediamtxPathFlag != "" {
			mtxCfg.BinaryPath = *mediamtxPathFlag
		}
		if *mediamtxConfigFlag != "" {
			mtxCfg.ConfigPath = *mediamtxConfigFlag
		}
		mtxCfg.GatewayOrigin = strings.TrimRight(browserURL(*httpAddr), "/")
		applyMediaSettings(&mtxCfg, integ)
		if *recordingsDirFlag != "" {
			mtxCfg.RecordDir = *recordingsDirFlag
		}
		mtxSupervisor = mediamtx.NewSupervisor(mtxCfg)
		if err := mtxSupervisor.Start(context.Background()); err != nil {
			fmt.Printf("[mediamtx] ERROR: the video server could not start: %v\n", err)
			fmt.Printf("[mediamtx] live video will not work until this is fixed; details are also shown in the web UI\n")
		}
		setMediaRuntime(mtxSupervisor, mtxSupervisor.APIBase())
	} else {
		setMediaRuntime(nil, *mediamtxAPIFlag)
	}

	setRecordingsRoot(*recordingsDirFlag)
	setRecordingsRetention(*recordingsRetentionFlag)
	if *recordingsDirFlag != "" && *recordingsRetentionFlag > 0 {
		// Prune once immediately, then on the interval.
		go cleanRecordings()
		StartRecordingCleanup(*recordingsCleanupFlag)
	}

	// The data folder holds the profile, operator tokens and the saved server key and app ID.
	var profDir string
	if profilePathFlag != nil && *profilePathFlag != "" {
		profDir = filepath.Dir(*profilePathFlag)
	} else {
		profDir = filepath.Dir(defaultProfPath)
	}
	_ = os.MkdirAll(profDir, 0700)
	keys := loadServerKeys(profDir)
	loadAppIDs(profDir)

	phoneCode := fmt.Sprintf("%032x", rand.Int63())
	sessionMgr := NewSessionManager(*country, phoneCode, keys)
	if c := sessionMgr.Cloud(); c != nil {
		applyTimezoneConfig(c, *timezoneFlag, *zoneOffsetFlag)
	}

	var camIPs []string
	if *cameraIPFlag != "" {
		for _, ip := range strings.Split(*cameraIPFlag, ",") {
			if trimmed := strings.TrimSpace(ip); trimmed != "" {
				camIPs = append(camIPs, trimmed)
			}
		}
	}

	effectivePublishRTSP := *publishRTSPBase
	if effectivePublishRTSP == "" {
		effectivePublishRTSP = *rtspBase
	}
	ffmpegPath := resolveFFmpeg(*ffmpeg)
	publisher := choosePublisher(*publisherFlag, ffmpegPath)
	switch {
	case publisher == bridge.PublisherFFmpeg:
		fmt.Printf("[media] publishing camera video with FFmpeg (%s) [legacy mode]\n", ffmpegPath)
	case ffmpegPath != "":
		fmt.Printf("[media] publishing camera video with the built-in publisher; FFmpeg (%s) adds browser audio and talkback\n", ffmpegPath)
	default:
		fmt.Printf("[media] publishing camera video with the built-in publisher; FFmpeg not found, so browsers get sound via HLS and talkback is unavailable\n")
	}
	streamMgr := NewStreamManager(sessionMgr.Cloud(), effectivePublishRTSP, *hlsBase, camIPs, *strictManualFlag, ffmpegPath)
	streamMgr.SetPublisher(publisher)
	streamMgr.SetRetryMaxWait(*retryMaxWaitFlag)
	if mtxSupervisor != nil && !mediaBasesSet && *mediamtxConfigFlag == "" {
		setStreamManagerPorts(streamMgr, integ)
	}
	setCurrentStreamManager(streamMgr)
	// Keeps the router's camera list in line with the Firewall choices.
	go privacyAutoSyncLoop(context.Background(), streamMgr)

	// The Osaio login pool: every stored login gets its own session, and each
	// camera streams through the login it was added with. sessionMgr is only
	// the template new sessions are made from; no login is the main one.
	sessionRegistry := NewSessionRegistry(sessionMgr, *country, phoneCode, keys)
	SetGatewaySessions(sessionRegistry)
	streamMgr.SetLoginRenewer(renewCloudLogin)
	streamMgr.SetCloudResolver(func(uuid string) *bridge.Cloud {
		if pm := ActiveProfileManager(); pm != nil {
			if prof := pm.GetProfile(); prof != nil {
				if creds, ok := prof.AccountForCamera(uuid); ok {
					return sessionRegistry.CloudFor(creds.AccountEmail)
				}
			}
		}
		// A camera saved without its login: whichever signed-in login lists it.
		if _, session, ok := sessionRegistry.FindDevice(uuid); ok && session.IsAuthenticated() {
			return session.Cloud()
		}
		return nil // wait until the camera's login is signed in
	})
	if *consumerRTSPBase != "" {
		streamMgr.SetConsumerRTSPBase(*consumerRTSPBase)
	}
	if *consumerHLSBase != "" {
		streamMgr.SetConsumerHLSBase(*consumerHLSBase)
	}
	if *webrtcBase != "" && (mediaBasesSet || mtxSupervisor == nil || *mediamtxConfigFlag != "") {
		streamMgr.SetWebRTCBase(*webrtcBase)
	}
	// NVR address, stream password, snapshots, MQTT (see integration_settings.go)
	applyIntegrationRuntime(streamMgr, integ)
	go runIntegrationLoop(context.Background(), streamMgr)

	sigProvider := func(cameraUUID string) (*bridge.Signaling, error) {
		if streamMgr != nil {
			if mc, ok := streamMgr.GetCamera(cameraUUID); ok && mc != nil {
				mc.mu.RLock()
				sig := mc.Signaling
				mc.mu.RUnlock()
				if sig != nil {
					return sig, nil
				}
			}
		}
		if cs := findTargetCam(cameraUUID); cs != nil && cs.sig != nil {
			return cs.sig, nil
		}
		return nil, fmt.Errorf("camera %s has no active signaling connection", cameraUUID)
	}
	sigControl := bridge.NewSignalingControlChannelWithProvider(sigProvider)

	if *controlTransport == "cloud" {
		SetGatewayControlChannel(sigControl)
		fmt.Printf("[control] vendor cloud signaling control channel initialized\n")
	} else if *controlTransport == "mqtt" {
		localShimDaemon := shadowshim.NewDaemon(shadowshim.Config{DefaultTimeout: 2 * time.Second})
		_ = localShimDaemon.Start(context.Background())
		mqttControl := bridge.NewMQTTControlChannel(localShimDaemon, 2*time.Second)
		SetGatewayControlChannel(mqttControl)

		// Start bidirectional Mosquitto MQTT adapter into shadow shim
		mosquittoURL := *mosquittoURLFlag
		bridgeCfg := bridge.DefaultMosquittoBridgeConfig()
		bridgeCfg.BrokerURL = mosquittoURL
		mosqBridge := bridge.NewMosquittoBridge(bridgeCfg, localShimDaemon)
		mqttControl.AttachBroker(mosqBridge)
		if err := mosqBridge.Start(context.Background()); err != nil {
			fmt.Printf("[local-control] startup: %v\n", err)
		}
		defer localShimDaemon.Close()
		defer mqttControl.Close()
		defer mosqBridge.Close()

		fmt.Printf("[control] local MQTT shadow control channel initialized on port 8883 shim (broker: %s)\n", mosquittoURL)
	} else {
		SetGatewayControlChannel(sigControl)
		fmt.Printf("[control] default cloud signaling control channel initialized\n")
	}

	// Configure camera subnets (explicit or auto-detected)
	var allowedSubnets []*net.IPNet
	if *cameraSubnetFlag != "" {
		for _, s := range strings.Split(*cameraSubnetFlag, ",") {
			if s = strings.TrimSpace(s); s != "" {
				if _, ipNet, err := net.ParseCIDR(s); err == nil {
					allowedSubnets = append(allowedSubnets, ipNet)
				} else {
					fmt.Printf("[config] invalid camera subnet %q: %v\n", s, err)
				}
			}
		}
	} else {
		if detected, err := netstack.DetectHostSubnets(); err == nil && len(detected) > 0 {
			allowedSubnets = detected
			fmt.Printf("[config] auto-detected %d host LAN subnet(s) for camera discovery: %v\n", len(allowedSubnets), allowedSubnets)
		}
	}
	streamMgr.SetAllowedSubnets(allowedSubnets)

	// Initialize local operator auth & security boundary manager
	keyFile := filepath.Join(profDir, "api_tokens.json")
	opMgr := auth.NewOperatorManager(auth.OperatorConfig{
		KeyFile: keyFile,
	})
	SetGatewayOperatorManager(opMgr)

	// The encrypted profile manager was created above, before the HTTP API opens.
	if pmErr != nil || pm == nil {
		fmt.Printf("[profile] error: failed to initialize key provider: %v\n", pmErr)
		sessionMgr.SetStatus(SessionStatusUnavailable, fmt.Sprintf("profile key initialization failed: %v", pmErr))
	} else {
		SetActiveProfileManager(pm)
		ensureAdminFromEnv(pm, os.Getenv)
		// The browser opens before the saved session is restored; report
		// "authenticating" instead of the constructor's "unavailable" so the UI
		// doesn't flash the login screen on every launch.
		if has, herr := pm.HasProfile(context.Background()); herr == nil && has {
			sessionMgr.SetStatus(SessionStatusAuthenticating, "restoring saved session")
		}
	}

	// Decouple HTTP server from login: start HTTP listener immediately on :8654
	go serveControlAPI(controlListener, sessionMgr, streamMgr)

	if !*headlessFlag && !*startupFlag && runtime.GOOS == "windows" {
		go func() {
			time.Sleep(250 * time.Millisecond)
			showPage(browserURL(*httpAddr))
		}()
	}

	go runGatewayStartupSafely(sessionMgr, streamMgr, *emailF, *passF, *country, *enrolledF, *deviceSel, *httpAddr, pm)

	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM)
	// On Windows the tray icon runs here (it needs the main thread) until
	// BombeCam is told to stop.
	runMainLoop(trayConfig{
		disabled: *headlessFlag,
		uiURL:    browserURL(*httpAddr),
		logDir:   gatewayLogDir(),
		streaming: func() (int, int) {
			return streamMgr.ActiveStreamCount(), streamMgr.CameraCount()
		},
	}, func() {
		select {
		case <-ch:
			fmt.Println("shutting down gateway...")
		case <-shutdownRequested:
			fmt.Println("shutting down gateway (Shut down pressed)...")
		}
	})
	streamMgr.CloseAll()
	if mtxSupervisor != nil {
		if err := mtxSupervisor.Stop(); err != nil {
			fmt.Printf("[mediamtx] stop error: %v\n", err)
		}
	}
	fmt.Println("bye.")
	flushGatewayLog()
}
