package mediamtx

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"
)

// Supervisor oversees the lifecycle and health of the MediaMTX streaming server.
//
// A managed MediaMTX that exits is restarted with backoff, and its output is
// kept (log file + in-memory tail) so a startup failure is reported with the
// reason MediaMTX printed instead of disappearing into io.Discard.
type Supervisor struct {
	cfg       Config
	opMu      sync.Mutex
	mu        sync.Mutex
	cmd       *exec.Cmd
	isManaged bool
	running   bool
	binPath   string
	cfgPath   string
	logPath   string
	stdout    io.Writer
	stderr    io.Writer
	tail      *lineTail
	lastErr   string
	restarts  int
	stopCh    chan struct{}
	exited    chan struct{}
}

// Status is a point-in-time snapshot of the media server for the UI and API.
type Status struct {
	Running   bool   `json:"running"`
	Managed   bool   `json:"managed"`
	Listening bool   `json:"listening"`
	PID       int    `json:"pid,omitempty"`
	Binary    string `json:"binary,omitempty"`
	Config    string `json:"config,omitempty"`
	LogFile   string `json:"log_file,omitempty"`
	Restarts  int    `json:"restarts"`
	LastError string `json:"last_error,omitempty"`
}

// NewSupervisor creates a new MediaMTX supervisor with the provided configuration.
func NewSupervisor(cfg Config) *Supervisor {
	if cfg.RTSPPort <= 0 {
		cfg.RTSPPort = DefaultRTSPPort
	}
	if cfg.HTTPPort <= 0 {
		cfg.HTTPPort = DefaultHTTPPort
	}
	if cfg.WebRTCPort <= 0 {
		cfg.WebRTCPort = DefaultWebRTCPort
	}
	if cfg.APIPort <= 0 {
		cfg.APIPort = DefaultAPIPort
	}
	if cfg.WebRTCICEPort <= 0 {
		cfg.WebRTCICEPort = DefaultWebRTCICEPort
	}
	if cfg.StartupTimeout <= 0 {
		cfg.StartupTimeout = DefaultStartupTimeout
	}
	if cfg.ProbeTimeout <= 0 {
		cfg.ProbeTimeout = DefaultProbeTimeout
	}
	if cfg.CacheDir == "" {
		cfg.CacheDir = DefaultCacheDir()
	}
	if cfg.LogDir == "" {
		cfg.LogDir = filepath.Join(filepath.Dir(cfg.CacheDir), "logs")
	}
	if cfg.DownloadURL == "" {
		cfg.DownloadURL = DefaultDownloadURL()
	}

	return &Supervisor{
		cfg:    cfg,
		stdout: io.Discard,
		stderr: io.Discard,
		tail:   newLineTail(40),
	}
}

// CheckPortListening probes a TCP port on localhost to determine if a service is actively listening.
func CheckPortListening(port int, timeout time.Duration) bool {
	if timeout <= 0 {
		timeout = DefaultProbeTimeout
	}
	target := fmt.Sprintf("127.0.0.1:%d", port)
	conn, err := net.DialTimeout("tcp", target, timeout)
	if err == nil {
		_ = conn.Close()
		return true
	}
	return false
}

// IsManaged reports whether the supervised MediaMTX process was spawned by this supervisor.
func (s *Supervisor) IsManaged() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.isManaged
}

// IsRunning reports whether the supervisor is currently tracking an active MediaMTX instance.
func (s *Supervisor) IsRunning() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.running
}

// IsListening probes whether the RTSP port is actively listening.
func (s *Supervisor) IsListening() bool {
	cfg := s.Config()
	return CheckPortListening(cfg.RTSPPort, cfg.ProbeTimeout)
}

// GetPID returns the process ID of the managed subprocess, or 0 if unmanaged/stopped.
func (s *Supervisor) GetPID() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cmd != nil && s.cmd.Process != nil {
		return s.cmd.Process.Pid
	}
	return 0
}

// APIBase returns the loopback URL of the MediaMTX management API.
func (s *Supervisor) APIBase() string {
	return fmt.Sprintf("http://127.0.0.1:%d", s.Config().APIPort)
}

// Status returns a snapshot of the media server state.
func (s *Supervisor) Status() Status {
	s.mu.Lock()
	st := Status{
		Running:   s.running,
		Managed:   s.isManaged,
		Binary:    s.binPath,
		Config:    s.cfgPath,
		LogFile:   s.logPath,
		Restarts:  s.restarts,
		LastError: s.lastErr,
	}
	if s.cmd != nil && s.cmd.Process != nil {
		st.PID = s.cmd.Process.Pid
	}
	s.mu.Unlock()
	st.Listening = s.IsListening()
	if st.Listening && !st.Managed && st.LastError == "" {
		st.Running = true
	}
	return st
}

// Start checks if MediaMTX is already listening on the configured RTSP port.
// If it is, it adopts it as an unmanaged instance without spawning.
// Otherwise it resolves or fetches the binary, writes the generated
// configuration, launches the subprocess and waits until the RTSP port is
// listening. A managed process that later exits is restarted automatically.
func (s *Supervisor) Start(ctx context.Context) error {
	s.opMu.Lock()
	defer s.opMu.Unlock()
	return s.start(ctx)
}

func (s *Supervisor) start(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.running {
		return nil
	}

	// 1. Pre-flight port detection
	if CheckPortListening(s.cfg.RTSPPort, s.cfg.ProbeTimeout) {
		s.isManaged = false
		s.running = true
		if apiReachable(fmt.Sprintf("http://127.0.0.1:%d", s.cfg.APIPort)) {
			fmt.Printf("[mediamtx] found a running MediaMTX on port %d; using it (not managed by BombeCam)\n", s.cfg.RTSPPort)
			s.lastErr = ""
		} else {
			s.lastErr = fmt.Sprintf("RTSP port %d is already used by another program (Frigate's go2rtc uses it too). Stop that program, or choose other ports on BombeCam's Frigate / Home Assistant page", s.cfg.RTSPPort)
			fmt.Printf("[mediamtx] WARNING: %s\n", s.lastErr)
		}
		return nil
	}
	check := s.cfg
	if s.customConfigLocked() {
		check.RTPPort = 0 // the user's own file decides; its ports are unknown here
	} else {
		s.pickRTPPortsLocked()
		check.RTPPort = s.cfg.RTPPort
	}
	if busy := PortConflicts(check); len(busy) > 0 {
		s.lastErr = fmt.Sprintf("%s already used by another program. Stop that program, or choose other ports on BombeCam's Frigate / Home Assistant page", strings.Join(busy, ", "))
		fmt.Printf("[mediamtx] ERROR: %s\n", s.lastErr)
		return fmt.Errorf("%s", s.lastErr)
	}

	// 2. Multi-stage resolution for mediamtx binary
	binPath, err := ResolveBinary(s.cfg)
	if err != nil {
		s.lastErr = err.Error()
		return fmt.Errorf("mediamtx supervisor pre-flight failed: %w", err)
	}
	s.binPath = binPath

	// 3. Configuration: a user-supplied file if it exists, else the generated one.
	cfgPath, err := s.prepareConfigLocked()
	if err != nil {
		s.lastErr = err.Error()
		return err
	}
	s.cfgPath = cfgPath

	// 4. Launch and wait for the RTSP listener.
	s.stopCh = make(chan struct{})
	s.running = true
	s.isManaged = true
	if err := s.launchLocked(ctx); err != nil {
		s.running = false
		s.isManaged = s.cmd != nil
		close(s.stopCh)
		s.lastErr = err.Error()
		return err
	}
	s.lastErr = ""
	return nil
}

var bindErrRe = regexp.MustCompile(`listen (tcp|udp) [^\s]*:(\d+): bind`)

// friendlyBindError puts "port N (UDP) is used by another program" in front
// of MediaMTX's raw bind error, which is what a user can act on.
func friendlyBindError(reason string) string {
	m := bindErrRe.FindStringSubmatch(reason)
	if m == nil {
		return reason
	}
	return fmt.Sprintf("port %s (%s) is already used by another program; stop it or change BombeCam's ports. %s", m[2], strings.ToUpper(m[1]), reason)
}

// customConfigLocked reports whether a user-supplied mediamtx.yml is in use.
func (s *Supervisor) customConfigLocked() bool {
	p := strings.TrimSpace(s.cfg.ConfigPath)
	if p == "" {
		return false
	}
	info, err := os.Stat(p)
	return err == nil && !info.IsDir()
}

// pickRTPPortsLocked keeps RTSP over UDP from blocking the whole video
// server: when another program holds the RTP/RTCP pair (UDP 8000/8001 by
// default), it moves to the next free even/odd pair. Clients are told the
// ports during RTSP setup, so no NVR setting changes.
func (s *Supervisor) pickRTPPortsLocked() {
	want := s.cfg.RTPPort
	if want <= 0 {
		want = DefaultRTPPort
	}
	if want%2 == 1 {
		want++
	}
	if p, ok := freeRTPPair(want, s.cfg.WebRTCICEPort); ok {
		if p != want {
			fmt.Printf("[mediamtx] UDP ports %d/%d are used by another program; RTSP over UDP uses %d/%d instead (nothing to change in players or NVRs)\n", want, want+1, p, p+1)
		}
		s.cfg.RTPPort = p
		return
	}
	s.cfg.RTPPort = want // no free pair nearby: PortConflicts reports it
}

// freeRTPPair returns the first even port from start on whose pair
// (port, port+1) is free for UDP, skipping the WebRTC media port.
func freeRTPPair(start, avoid int) (int, bool) {
	for p := start; p <= start+400 && p < 65535; p += 2 {
		if p == avoid || p+1 == avoid {
			continue
		}
		if udpPortFree(p) && udpPortFree(p+1) {
			return p, true
		}
	}
	return 0, false
}

func udpPortFree(port int) bool {
	c, err := net.ListenPacket("udp", fmt.Sprintf(":%d", port))
	if err != nil {
		return false
	}
	_ = c.Close()
	return true
}

// PortConflicts lists the MediaMTX ports (other than RTSP, which Start
// checks itself) that another program already holds, e.g. "HLS port 8888".
func PortConflicts(cfg Config) []string {
	var busy []string
	tcp := func(name string, port int) {
		if port <= 0 {
			return
		}
		// Windows permits wildcard and specific-address listeners to coexist.
		// Check each local IPv4 address too, so a different service cannot receive
		// requests on an address the gateway advertises for this media server.
		addresses := map[string]bool{"0.0.0.0": true, "127.0.0.1": true}
		if local, err := net.InterfaceAddrs(); err == nil {
			for _, address := range local {
				if ip, _, err := net.ParseCIDR(address.String()); err == nil && ip.To4() != nil && !ip.IsLinkLocalUnicast() && !ip.IsUnspecified() {
					addresses[ip.String()] = true
				}
			}
		}
		for address := range addresses {
			l, err := net.Listen("tcp4", net.JoinHostPort(address, fmt.Sprint(port)))
			if err != nil {
				busy = append(busy, fmt.Sprintf("%s port %d (%v)", name, port, err))
				return
			}
			l.Close()
		}
		probe, err := net.Listen("tcp6", "[::]:0")
		if err != nil {
			return
		}
		probe.Close()
		for _, address := range []string{"::", "::1"} {
			l, err := net.Listen("tcp6", net.JoinHostPort(address, fmt.Sprint(port)))
			if err != nil {
				busy = append(busy, fmt.Sprintf("%s port %d (IPv6)", name, port))
				return
			}
			l.Close()
		}
	}
	udp := func(name string, port int) {
		if port <= 0 {
			return
		}
		c, err := net.ListenPacket("udp", fmt.Sprintf(":%d", port))
		if err != nil {
			busy = append(busy, fmt.Sprintf("%s port %d (UDP)", name, port))
			return
		}
		_ = c.Close()
	}
	tcp("HLS", cfg.HTTPPort)
	tcp("WebRTC", cfg.WebRTCPort)
	udp("WebRTC media", cfg.WebRTCICEPort)
	if cfg.RTPPort > 0 {
		udp("RTSP-over-UDP", cfg.RTPPort)
		udp("RTSP-over-UDP", cfg.RTPPort+1)
	}
	if l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", cfg.APIPort)); err != nil {
		busy = append(busy, fmt.Sprintf("MediaMTX API port %d", cfg.APIPort))
	} else {
		_ = l.Close()
	}
	return busy
}

// Config returns a copy of the supervisor's configuration.
func (s *Supervisor) Config() Config {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cfg
}

// CanReconfigure rejects configurations that the gateway cannot change.
func (s *Supervisor) CanReconfigure() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.canReconfigureLocked()
}

func (s *Supervisor) canReconfigureLocked() error {
	if strings.TrimSpace(s.cfg.ConfigPath) != "" {
		return fmt.Errorf("MediaMTX uses a custom configuration; change ports and passwords in that file")
	}
	if s.running && !s.isManaged {
		return fmt.Errorf("MediaMTX was not started by BombeCam; change its own configuration")
	}
	return nil
}

// Reconfigure applies and verifies the new read policy before returning success.
// A failed change restores the previous configuration with an independent
// cleanup deadline, even if the initiating request was cancelled.
func (s *Supervisor) Reconfigure(ctx context.Context, update func(*Config)) error {
	s.opMu.Lock()
	defer s.opMu.Unlock()
	s.mu.Lock()
	if err := s.canReconfigureLocked(); err != nil {
		s.mu.Unlock()
		return err
	}
	old := s.cfg
	if err := s.stopLocked(); err != nil {
		s.mu.Unlock()
		return err
	}
	update(&s.cfg)
	s.mu.Unlock()
	err := s.start(ctx)
	if err == nil {
		if !s.IsManaged() {
			err = fmt.Errorf("the replacement listener is not owned by BombeCam")
		} else {
			err = s.VerifyReadPolicy(ctx)
		}
	}
	if err == nil {
		return nil
	}
	s.mu.Lock()
	stopErr := s.stopLocked()
	if stopErr == nil {
		s.cfg = old
	}
	s.mu.Unlock()
	if stopErr != nil {
		return errors.Join(err, fmt.Errorf("failed to stop rejected configuration: %w", stopErr))
	}
	cleanup, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	restoreErr := s.start(cleanup)
	if restoreErr == nil {
		if !s.IsManaged() {
			restoreErr = fmt.Errorf("restored listener is not owned by BombeCam")
		} else {
			restoreErr = s.VerifyReadPolicy(cleanup)
		}
	}
	if restoreErr != nil {
		return errors.Join(err, fmt.Errorf("previous media configuration could not be verified: %w", restoreErr))
	}
	return fmt.Errorf("change rejected; previous media configuration restored: %w", err)
}

func (s *Supervisor) prepareConfigLocked() (string, error) {
	if p := strings.TrimSpace(s.cfg.ConfigPath); p != "" {
		if info, err := os.Stat(p); err == nil && !info.IsDir() {
			if abs, err := filepath.Abs(p); err == nil {
				p = abs
			}
			fmt.Printf("[mediamtx] using custom configuration %s\n", p)
			return p, nil
		}
		fmt.Printf("[mediamtx] configuration %s not found; using the built-in configuration\n", p)
	}
	if err := os.MkdirAll(s.cfg.CacheDir, 0o755); err != nil {
		return "", fmt.Errorf("cannot create %s: %w", s.cfg.CacheDir, err)
	}
	p := filepath.Join(s.cfg.CacheDir, GeneratedConfigName)
	if err := os.WriteFile(p, []byte(GenerateConfig(s.cfg)), 0o644); err != nil {
		return "", fmt.Errorf("cannot write MediaMTX configuration %s: %w", p, err)
	}
	return p, nil
}

func (s *Supervisor) openLogLocked() io.Writer {
	if s.cfg.LogDir == "" {
		return io.Discard
	}
	if err := os.MkdirAll(s.cfg.LogDir, 0o755); err != nil {
		return io.Discard
	}
	p := filepath.Join(s.cfg.LogDir, "mediamtx.log")
	// Keep one previous log; each gateway launch starts a fresh file.
	if s.logPath == "" {
		if _, err := os.Stat(p); err == nil {
			_ = os.Rename(p, filepath.Join(s.cfg.LogDir, "mediamtx.prev.log"))
		}
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return io.Discard
	}
	s.logPath = p
	return f
}

// launchLocked spawns MediaMTX and blocks until the RTSP port is listening,
// the process exits, or the startup timeout elapses.
func (s *Supervisor) launchLocked(ctx context.Context) error {
	logW := s.openLogLocked()
	capture := &lineCapture{tail: s.tail}
	out := io.MultiWriter(logW, capture, s.stdout)
	errOut := io.MultiWriter(logW, capture, s.stderr)

	cmd := exec.Command(s.binPath, s.cfgPath)
	if err := os.MkdirAll(s.cfg.CacheDir, 0o755); err == nil {
		cmd.Dir = s.cfg.CacheDir // relative paths in a user config resolve here, not in the user's CWD
	}
	cmd.Stdout = out
	cmd.Stderr = errOut
	hideWindow(cmd)
	if err := cmd.Start(); err != nil {
		if c, ok := logW.(io.Closer); ok {
			_ = c.Close()
		}
		return fmt.Errorf("failed to start mediamtx process (%s %s): %w", s.binPath, s.cfgPath, err)
	}
	s.cmd = cmd
	exited := make(chan struct{})
	s.exited = exited
	pid := cmd.Process.Pid
	fmt.Printf("[mediamtx] started MediaMTX (PID %d, config %s, log %s)\n", pid, s.cfgPath, s.logPath)

	var exitErr error
	go func() {
		exitErr = cmd.Wait()
		if c, ok := logW.(io.Closer); ok {
			_ = c.Close()
		}
		close(exited)
		s.onExit(cmd, exitErr)
	}()

	deadline := time.Now().Add(s.cfg.StartupTimeout)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return errors.Join(ctx.Err(), s.killLocked())
		case <-exited:
			s.cmd = nil // reported here; onExit must not also schedule a restart
			reason := s.tail.Summary()
			if reason == "" {
				reason = fmt.Sprintf("%v", exitErr)
			}
			return fmt.Errorf("MediaMTX exited during startup: %s", friendlyBindError(reason))
		default:
		}
		if CheckPortListening(s.cfg.RTSPPort, 150*time.Millisecond) {
			fmt.Printf("[mediamtx] ready: RTSP :%d, HLS :%d, WebRTC :%d\n", s.cfg.RTSPPort, s.cfg.HTTPPort, s.cfg.WebRTCPort)
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}

	stopErr := s.killLocked()
	return errors.Join(fmt.Errorf("MediaMTX (PID %d) started but port %d was not listening within %v. %s", pid, s.cfg.RTSPPort, s.cfg.StartupTimeout, s.tail.Summary()), stopErr)
}

// onExit runs when a managed MediaMTX process exits. Unexpected exits are
// recorded and the process is restarted with backoff.
func (s *Supervisor) onExit(cmd *exec.Cmd, exitErr error) {
	s.mu.Lock()
	if s.cmd != cmd {
		s.mu.Unlock()
		return
	}
	s.cmd = nil
	stopCh := s.stopCh
	if !s.running || !s.isManaged {
		s.mu.Unlock()
		return
	}
	reason := s.tail.Summary()
	if reason == "" {
		reason = fmt.Sprintf("%v", exitErr)
	}
	s.lastErr = "MediaMTX stopped unexpectedly: " + reason
	s.restarts++
	attempt := s.restarts
	s.mu.Unlock()

	fmt.Printf("[mediamtx] %s\n", s.lastErrSnapshot())

	go func() {
		delay := time.Duration(attempt) * 2 * time.Second
		if delay > 30*time.Second {
			delay = 30 * time.Second
		}
		for {
			select {
			case <-stopCh:
				return
			case <-time.After(delay):
			}
			s.mu.Lock()
			if !s.running || !s.isManaged || s.cmd != nil {
				s.mu.Unlock()
				return
			}
			if CheckPortListening(s.cfg.RTSPPort, s.cfg.ProbeTimeout) {
				s.isManaged = false
				s.lastErr = ""
				s.mu.Unlock()
				fmt.Printf("[mediamtx] another MediaMTX now owns port %d; using it\n", s.cfg.RTSPPort)
				return
			}
			fmt.Printf("[mediamtx] restarting MediaMTX (attempt %d)...\n", attempt)
			err := s.launchLocked(context.Background())
			if err == nil {
				s.lastErr = ""
				s.mu.Unlock()
				return
			}
			s.lastErr = err.Error()
			s.restarts++
			attempt = s.restarts
			s.mu.Unlock()
			fmt.Printf("[mediamtx] restart failed: %v\n", err)
			delay = time.Duration(attempt) * 2 * time.Second
			if delay > 30*time.Second {
				delay = 30 * time.Second
			}
		}
	}()
}

func (s *Supervisor) lastErrSnapshot() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastErr
}

// Stop shuts down the MediaMTX supervisor.
// If the instance is managed, it kills the subprocess cleanly.
// If the instance is unmanaged, it leaves the external process running.
func (s *Supervisor) Stop() error {
	s.opMu.Lock()
	defer s.opMu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stopLocked()
}

func (s *Supervisor) stopLocked() error {
	if !s.running && (s.cmd == nil || !s.isManaged) {
		return nil
	}
	s.running = false
	if s.stopCh != nil {
		select {
		case <-s.stopCh:
		default:
			close(s.stopCh)
		}
	}

	if !s.isManaged {
		fmt.Printf("[mediamtx] unmanaged instance on port %d left running\n", s.cfg.RTSPPort)
		return nil
	}
	if err := s.killLocked(); err != nil {
		s.lastErr = err.Error()
		return err
	}
	s.isManaged = false
	return nil
}

// killLocked terminates the managed process and waits for it to exit.
func (s *Supervisor) killLocked() error {
	cmd, exited := s.cmd, s.exited
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	if exited != nil {
		select {
		case <-exited:
			s.cmd = nil
			return nil
		default:
		}
	}
	pid := cmd.Process.Pid
	var killErr error
	if runtime.GOOS == "windows" {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		kill := exec.CommandContext(ctx, "taskkill", "/F", "/T", "/PID", fmt.Sprint(pid))
		hideWindow(kill)
		killErr = kill.Run()
		cancel()
	}
	if runtime.GOOS != "windows" || killErr != nil {
		killErr = cmd.Process.Kill()
	}
	if exited != nil {
		select {
		case <-exited:
			s.cmd = nil
			fmt.Printf("[mediamtx] stopped MediaMTX (PID %d)\n", pid)
			return nil
		case <-time.After(5 * time.Second):
			return fmt.Errorf("MediaMTX (PID %d) exit was not confirmed: %v", pid, killErr)
		}
	}
	return fmt.Errorf("MediaMTX (PID %d) has no exit notification: %v", pid, killErr)
}

func apiReachable(base string) bool {
	client := &http.Client{Timeout: 700 * time.Millisecond}
	resp, err := client.Get(base + "/v3/paths/list")
	if err != nil {
		return false
	}
	_ = resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// PathInfo is the subset of MediaMTX's /v3/paths/get response BombeCam uses.
type PathInfo struct {
	Name   string   `json:"name"`
	Ready  bool     `json:"ready"`
	Tracks []string `json:"tracks"`
}

// GetPath queries the MediaMTX API for a path. found=false means the path is
// not being published; err means the API itself could not be reached.
// It lists paths rather than calling /v3/paths/get/<name>, which makes
// MediaMTX log an error line for every poll of a camera that is not live yet.
func GetPath(apiBase, name string) (info PathInfo, found bool, err error) {
	client := &http.Client{Timeout: 700 * time.Millisecond}
	resp, err := client.Get(strings.TrimRight(apiBase, "/") + "/v3/paths/list?itemsPerPage=1000")
	if err != nil {
		return PathInfo{}, false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return PathInfo{}, false, fmt.Errorf("mediamtx api returned %d", resp.StatusCode)
	}
	var list struct {
		Items []PathInfo `json:"items"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		return PathInfo{}, false, err
	}
	for _, it := range list.Items {
		if it.Name == name {
			return it, true, nil
		}
	}
	return PathInfo{}, false, nil
}

// lineTail keeps the last N lines of process output.
type lineTail struct {
	mu    sync.Mutex
	max   int
	lines []string
}

func newLineTail(max int) *lineTail { return &lineTail{max: max} }

func (t *lineTail) add(line string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.lines = append(t.lines, line)
	if len(t.lines) > t.max {
		t.lines = t.lines[len(t.lines)-t.max:]
	}
}

// Summary returns the most relevant recent lines: errors if any, else the last line.
func (t *lineTail) Summary() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	var errs []string
	for _, l := range t.lines {
		if isErrorLine(l) {
			errs = append(errs, strings.TrimSpace(l))
		}
	}
	if len(errs) > 3 {
		errs = errs[len(errs)-3:]
	}
	if len(errs) > 0 {
		return strings.Join(errs, " | ")
	}
	if n := len(t.lines); n > 0 {
		return strings.TrimSpace(t.lines[n-1])
	}
	return ""
}

func isErrorLine(l string) bool {
	return strings.Contains(l, "ERR") || strings.Contains(l, "panic")
}

// lineCapture splits output into lines, stores them in the tail and echoes
// warnings and errors to the gateway console.
type lineCapture struct {
	tail *lineTail
	buf  bytes.Buffer
	mu   sync.Mutex
}

func (c *lineCapture) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.buf.Write(p)
	for {
		line, err := c.buf.ReadString('\n')
		if err != nil {
			// incomplete line: put it back
			c.buf.Reset()
			c.buf.WriteString(line)
			break
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			continue
		}
		c.tail.add(line)
		// "skipping track" is expected: HLS readers skip the G.711 track and
		// WebRTC readers skip the AAC one.
		if (isErrorLine(line) || strings.Contains(line, "WAR")) && !strings.Contains(line, "skipping track") {
			fmt.Printf("[mediamtx] %s\n", line)
		}
	}
	return len(p), nil
}
