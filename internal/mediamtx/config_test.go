package mediamtx

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Keys that MediaMTX v1.9.3 rejects; with any of them MediaMTX refuses to start.
func TestGenerateConfig_NoInvalidKeys(t *testing.T) {
	cfg := GenerateConfig(DefaultConfig())
	for _, bad := range []string{"AllowCrossOrigin", "logFile:"} {
		if strings.Contains(cfg, bad) {
			t.Fatalf("generated config contains %q", bad)
		}
	}
	for _, want := range []string{"apiAddress: 127.0.0.1:9997", "apiAllowOrigin: 'http://127.0.0.1:8654'", "webrtcAllowOrigin: 'http://127.0.0.1:8654'", "hlsAllowOrigin: 'http://127.0.0.1:8654'", "hlsAddress: :8888", "rtspAddress: :8554"} {
		if !strings.Contains(cfg, want) {
			t.Fatalf("generated config missing %q:\n%s", want, cfg)
		}
	}
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	return p
}

// When a real MediaMTX binary is available (BOMBECAM_TEST_MEDIAMTX or PATH),
// the generated configuration must load and serve every port.
func TestGenerateConfig_LoadsInPinnedMediaMTX(t *testing.T) {
	bin := os.Getenv("BOMBECAM_TEST_MEDIAMTX")
	if bin == "" {
		if p, err := exec.LookPath(BinaryName()); err == nil {
			bin = p
		}
	}
	if bin == "" {
		t.Skip("no mediamtx binary available (set BOMBECAM_TEST_MEDIAMTX)")
	}
	cfg := testConfig(t)
	cfg.BinaryPath = bin
	cfg.CacheDir = t.TempDir()
	cfg.LogDir = t.TempDir()
	cfg.RTSPPort, cfg.HTTPPort, cfg.WebRTCPort, cfg.APIPort = freePort(t), freePort(t), freePort(t), freePort(t)
	cfg.StartupTimeout = 8 * time.Second
	sup := NewSupervisor(cfg)
	if err := sup.Start(context.Background()); err != nil {
		t.Fatalf("mediamtx did not start with the generated config: %v", err)
	}
	defer sup.Stop()
	for _, p := range []int{cfg.HTTPPort, cfg.WebRTCPort, cfg.APIPort} {
		if !CheckPortListening(p, time.Second) {
			t.Errorf("port %d not listening", p)
		}
	}
	if _, found, err := GetPath(sup.APIBase(), "nothing-here"); err != nil || found {
		t.Errorf("GetPath on empty server: found=%v err=%v", found, err)
	}
}

// Another program holding UDP 8000/8001 (RTSP over UDP) must not stop the
// video server: the supervisor moves to a free pair and MediaMTX starts.
// Seen on a user's PC: "listen udp :8000: bind: Only one usage of each socket
// address".
func TestSupervisor_MovesRTPWhenUDPPortTaken(t *testing.T) {
	bin := os.Getenv("BOMBECAM_TEST_MEDIAMTX")
	if bin == "" {
		t.Skip("no mediamtx binary available (set BOMBECAM_TEST_MEDIAMTX)")
	}
	cfg := testConfig(t)
	cfg.BinaryPath = bin
	cfg.CacheDir = t.TempDir()
	cfg.LogDir = t.TempDir()
	cfg.RTSPPort, cfg.HTTPPort, cfg.WebRTCPort, cfg.APIPort = freePort(t), freePort(t), freePort(t), freePort(t)
	cfg.StartupTimeout = 8 * time.Second
	taken, ok := freeRTPPair(30000, 0)
	if !ok {
		t.Skip("no free UDP pair")
	}
	hold, err := net.ListenPacket("udp", fmt.Sprintf(":%d", taken))
	if err != nil {
		t.Fatal(err)
	}
	defer hold.Close()
	cfg.RTPPort = taken
	sup := NewSupervisor(cfg)
	if err := sup.Start(context.Background()); err != nil {
		t.Fatalf("mediamtx did not start with UDP %d taken: %v", taken, err)
	}
	defer sup.Stop()
	moved := sup.Config().RTPPort
	if moved == taken || moved%2 != 0 {
		t.Fatalf("RTP port %d, want a free even port other than %d", moved, taken)
	}
	if b, _ := os.ReadFile(filepath.Join(cfg.CacheDir, GeneratedConfigName)); !strings.Contains(string(b), fmt.Sprintf("rtpAddress: :%d\nrtcpAddress: :%d\n", moved, moved+1)) {
		t.Fatalf("generated config does not use the new pair:\n%s", b)
	}
	// and a player that insists on UDP still gets the stream
	ffmpeg, err1 := exec.LookPath("ffmpeg")
	ffprobe, err2 := exec.LookPath("ffprobe")
	if err1 != nil || err2 != nil {
		t.Log("ffmpeg/ffprobe not found; skipping the UDP playback check")
		return
	}
	url := fmt.Sprintf("rtsp://127.0.0.1:%d/udpcheck", cfg.RTSPPort)
	pub := exec.Command(ffmpeg, "-hide_banner", "-loglevel", "error", "-re", "-f", "lavfi", "-i", "testsrc=size=320x240:rate=10",
		"-c:v", "libx264", "-t", "20", "-f", "rtsp", "-rtsp_transport", "tcp", url)
	if err := pub.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = pub.Process.Kill(); _, _ = pub.Process.Wait() }()
	time.Sleep(3 * time.Second)
	out, err := exec.Command(ffprobe, "-v", "error", "-rtsp_transport", "udp", "-show_entries", "stream=codec_name", "-of", "csv=p=0", url).CombinedOutput()
	if err != nil || !strings.Contains(string(out), "h264") {
		t.Fatalf("UDP playback on the moved ports failed: %v %s", err, out)
	}
}

func TestFreeRTPPair_SkipsTakenAndWebRTCPort(t *testing.T) {
	start, ok := freeRTPPair(31000, 0)
	if !ok {
		t.Skip("no free UDP pair")
	}
	hold, err := net.ListenPacket("udp", fmt.Sprintf(":%d", start+1)) // RTCP half taken
	if err != nil {
		t.Fatal(err)
	}
	defer hold.Close()
	got, ok := freeRTPPair(start, start+3) // and the next pair collides with WebRTC media
	if !ok || got%2 != 0 || got == start || got == start+2 {
		t.Fatalf("got %d (ok=%v), want a free even port past %d and %d", got, ok, start, start+2)
	}
}

// A MediaMTX that rejects its config must surface the reason, not a generic timeout.
func TestSupervisor_ReportsStartupErrorOutput(t *testing.T) {
	tmp := t.TempDir()
	src := filepath.Join(tmp, "bad.go")
	bin := filepath.Join(tmp, BinaryName())
	code := `package main
import ("fmt";"os")
func main(){ fmt.Println("ERR: json: unknown field \"webrtcAllowCrossOrigin\""); os.Exit(1) }`
	if err := os.WriteFile(src, []byte(code), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("go", "build", "-o", bin, src).CombinedOutput(); err != nil {
		t.Fatalf("build mock: %v %s", err, out)
	}
	cfg := testConfig(t)
	cfg.BinaryPath = bin
	cfg.CacheDir = t.TempDir()
	cfg.LogDir = t.TempDir()
	cfg.RTSPPort = freePort(t)
	cfg.StartupTimeout = 5 * time.Second
	sup := NewSupervisor(cfg)
	err := sup.Start(context.Background())
	if err == nil {
		sup.Stop()
		t.Fatal("expected startup error")
	}
	if !strings.Contains(err.Error(), "webrtcAllowCrossOrigin") {
		t.Fatalf("error does not include MediaMTX's reason: %v", err)
	}
	if st := sup.Status(); st.Running || !strings.Contains(st.LastError, "unknown field") {
		t.Fatalf("status = %+v", st)
	}
	logData, _ := os.ReadFile(filepath.Join(cfg.LogDir, "mediamtx.log"))
	if !strings.Contains(string(logData), "unknown field") {
		t.Fatalf("log file missing output: %q", logData)
	}
}

// A managed MediaMTX that dies after startup is restarted.
func TestSupervisor_RestartsAfterCrash(t *testing.T) {
	tmp := t.TempDir()
	port := freePort(t)
	marker := filepath.Join(tmp, "runs")
	src := filepath.Join(tmp, "flaky.go")
	bin := filepath.Join(tmp, BinaryName())
	code := fmt.Sprintf(`package main
import ("net";"os";"time")
func main(){
	f,_ := os.OpenFile(%q, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644); f.WriteString("x"); f.Close()
	b,_ := os.ReadFile(%q)
	l,err := net.Listen("tcp","127.0.0.1:%d"); if err!=nil {os.Exit(1)}
	if len(b)==1 { time.Sleep(300*time.Millisecond); l.Close(); os.Exit(2) }
	time.Sleep(30*time.Second)
}`, marker, marker, port)
	if err := os.WriteFile(src, []byte(code), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("go", "build", "-o", bin, src).CombinedOutput(); err != nil {
		t.Fatalf("build mock: %v %s", err, out)
	}
	cfg := testConfig(t)
	cfg.BinaryPath = bin
	cfg.CacheDir = t.TempDir()
	cfg.LogDir = t.TempDir()
	cfg.RTSPPort = port
	sup := NewSupervisor(cfg)
	if err := sup.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer sup.Stop()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		b, _ := os.ReadFile(marker)
		if len(b) >= 2 && CheckPortListening(port, 200*time.Millisecond) {
			if st := sup.Status(); st.Restarts < 1 {
				t.Fatalf("expected restart count, got %+v", st)
			}
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("mediamtx was not restarted; status=%+v", sup.Status())
}

// A custom -mediamtx-config must work even when the cache directory does not exist yet.
func TestSupervisor_CustomConfigWithFreshCacheDir(t *testing.T) {
	tmp := t.TempDir()
	src := filepath.Join(tmp, "ok.go")
	bin := filepath.Join(tmp, BinaryName())
	port := freePort(t)
	code := fmt.Sprintf(`package main
import ("net";"time")
func main(){ l,_ := net.Listen("tcp","127.0.0.1:%d"); defer l.Close(); time.Sleep(20*time.Second) }`, port)
	if err := os.WriteFile(src, []byte(code), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("go", "build", "-o", bin, src).CombinedOutput(); err != nil {
		t.Fatalf("build mock: %v %s", err, out)
	}
	custom := filepath.Join(tmp, "custom.yml")
	_ = os.WriteFile(custom, []byte("logLevel: info\n"), 0o644)
	cfg := testConfig(t)
	cfg.BinaryPath = bin
	cfg.ConfigPath = custom
	cfg.CacheDir = filepath.Join(tmp, "does", "not", "exist")
	cfg.LogDir = t.TempDir()
	cfg.RTSPPort = port
	sup := NewSupervisor(cfg)
	if err := sup.Start(context.Background()); err != nil {
		t.Fatalf("start with custom config: %v", err)
	}
	sup.Stop()
}

// settingLines keeps a config's settings: no comments, no blank lines.
func settingLines(cfg string) []string {
	var out []string
	for _, l := range strings.Split(cfg, "\n") {
		l = strings.TrimRight(l, " \r")
		if t := strings.TrimSpace(l); t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		out = append(out, l)
	}
	return out
}

// deploy/mediamtx.yml (Docker) must stay equivalent to what the Windows app
// generates with default settings.
func TestDeployConfigMatchesGenerated(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "deploy", "mediamtx.yml"))
	if err != nil {
		t.Fatal(err)
	}
	gen, dep := settingLines(GenerateConfig(DefaultConfig())), settingLines(string(b))
	if strings.Join(gen, "\n") != strings.Join(dep, "\n") {
		t.Fatalf("deploy/mediamtx.yml differs from the generated configuration.\ngenerated:\n%s\n\ndeploy:\n%s", strings.Join(gen, "\n"), strings.Join(dep, "\n"))
	}
}

// The stream password switch: other addresses need the user and password,
// this machine does not (the gateway's own reads), and the origins stay
// locked to the gateway.
// With a recording directory chosen, the generated configuration records every
// stream into that directory (one subdirectory per stream path) in fragmented
// MP4, one-second segments.
func TestGenerateConfig_Recording(t *testing.T) {
	cfg := DefaultConfig()
	dir := filepath.Join(t.TempDir(), "recordings")
	cfg.RecordDir = dir
	out := GenerateConfig(cfg)
	want := yamlSingleQuoted(filepath.Join(dir, "%path", "%Y-%m-%d_%H-%M-%S"))
	for _, sub := range []string{"  record: yes", "  recordPath: " + want, "  recordFormat: fmp4", "  recordSegmentDuration: 1s"} {
		if !strings.Contains(out, sub) {
			t.Errorf("recording config missing %q in:\n%s", sub, out)
		}
	}
	// The default (no directory) keeps the config free of any record keys, so
	// MediaMTX does not create recordings the user never asked for.
	plain := GenerateConfig(DefaultConfig())
	if strings.Contains(plain, "recordPath") || strings.Contains(plain, "recordFormat") {
		t.Errorf("default config must not record:\n%s", plain)
	}
}

func TestGenerateConfig_StreamPasswordAndOrigins(t *testing.T) {
	cfg := DefaultConfig()
	cfg.ReadUser, cfg.ReadPass = "bombecam", "it's-secret"
	cfg.GatewayOrigin = "http://127.0.0.1:9000/"
	out := GenerateConfig(cfg)
	for _, want := range []string{
		// sha256 of "it's-secret": the file on disk never holds the password
		"  - user: 'bombecam'\n    pass: 'sha256:" + hashedPass("it's-secret")[len("sha256:"):] + "'\n    ips: []\n    permissions:\n      - action: read\n",
		"apiAllowOrigin: 'http://127.0.0.1:9000'", "hlsAllowOrigin: 'http://127.0.0.1:9000'", "webrtcAllowOrigin: 'http://127.0.0.1:9000'",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "  - user: any\n    pass:\n    ips: []") {
		t.Error("anonymous reads from other devices must be gone")
	}
	if strings.Contains(out, "'*'") {
		t.Error("no origin may be a wildcard")
	}
	if strings.Contains(out, "secret") {
		t.Error("the stream password must not be written in plain text")
	}
}

func TestFriendlyBindError(t *testing.T) {
	raw := "2026/09/29 07:45:16 ERR listen udp :8000: bind: Only one usage of each socket address (protocol/network address/port) is normally permitted."
	if got := friendlyBindError(raw); !strings.HasPrefix(got, "port 8000 (UDP) is already used by another program") || !strings.HasSuffix(got, raw) {
		t.Fatalf("got %q", got)
	}
	if got := friendlyBindError("ERR: json: unknown field"); got != "ERR: json: unknown field" {
		t.Fatalf("unrelated errors must pass through: %q", got)
	}
}
