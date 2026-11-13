package mediamtx

import (
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const (
	// DefaultRTSPPort is the standard RTSP listening port for MediaMTX.
	DefaultRTSPPort = 8554

	// DefaultHTTPPort is the standard HLS listening port for MediaMTX.
	DefaultHTTPPort = 8888

	// DefaultAPIPort is the standard REST management port for MediaMTX.
	DefaultAPIPort = 9997

	// DefaultWebRTCPort is the standard WebRTC / WHEP listening port for MediaMTX.
	DefaultWebRTCPort = 8889

	// DefaultWebRTCICEPort is the UDP port MediaMTX uses for WebRTC media.
	DefaultWebRTCICEPort = 8189

	// DefaultRTPPort / DefaultRTCPPort carry RTSP over UDP (MediaMTX's
	// defaults). Clients learn them during RTSP setup, so when another
	// program holds them the supervisor simply picks the next free pair.
	DefaultRTPPort  = 8000
	DefaultRTCPPort = 8001

	// DefaultConfigPath is empty: BombeCam generates a known-good configuration
	// in CacheDir on every launch instead of depending on the working directory.
	DefaultConfigPath = ""

	// DefaultStartupTimeout is the maximum duration to wait for MediaMTX to bind its ports.
	DefaultStartupTimeout = 5 * time.Second

	// DefaultProbeTimeout is the network dial timeout when checking port listeners.
	DefaultProbeTimeout = 300 * time.Millisecond

	// MediaMTXVersion specifies the targeted MediaMTX release tag.
	MediaMTXVersion = "v1.9.3"
)

// Config holds runtime configuration options for the MediaMTX supervisor.
type Config struct {
	// RTSPPort is the TCP port on which MediaMTX serves RTSP (default: 8554).
	RTSPPort int

	// HTTPPort is the TCP port on which MediaMTX serves HLS (default: 8888).
	HTTPPort int

	// WebRTCPort is the TCP port on which MediaMTX serves WebRTC/WHEP (default: 8889).
	WebRTCPort int

	// APIPort is the TCP port on which MediaMTX serves REST API (default: 9997).
	APIPort int

	// WebRTCICEPort is the UDP port MediaMTX uses for WebRTC media (default: 8189).
	WebRTCICEPort int

	// RTPPort is the UDP port for RTSP over UDP; RTCP uses RTPPort+1
	// (default: 8000). Start moves it when another program holds it.
	RTPPort int

	// ConfigPath is an optional user-supplied mediamtx.yml. Empty (the default)
	// means BombeCam writes GenerateConfig() into CacheDir and uses that.
	ConfigPath string

	// LogDir is where MediaMTX output is written (default: CacheDir/../logs).
	LogDir string

	// BinaryPath is an optional explicit path override for the mediamtx binary.
	BinaryPath string

	// CacheDir is the directory where downloaded/cached mediamtx binaries are stored.
	// Defaults to %LOCALAPPDATA%\bombecam\bin on Windows, ~/.local/share/bombecam/bin on Linux.
	CacheDir string

	// StartupTimeout is the maximum duration to wait for MediaMTX to start listening on RTSPPort.
	StartupTimeout time.Duration

	// ProbeTimeout is the timeout when checking if RTSPPort is already listening.
	ProbeTimeout time.Duration

	// AutoDownload specifies whether to attempt downloading MediaMTX if missing.
	AutoDownload bool

	// DownloadURL specifies an optional explicit download URL for the archive.
	DownloadURL string

	// GatewayOrigin is the web origin of BombeCam's UI; MediaMTX's API, HLS
	// and WebRTC servers answer cross-origin requests from it only
	// (default http://127.0.0.1:8654).
	GatewayOrigin string

	// ReadUser/ReadPass, when set, are required for reading streams from any
	// address other than this machine (the "stream password" switch).
	ReadUser string
	ReadPass string

	// RecordDir, when non-empty, records each published stream to this
	// directory (one subdirectory per stream path). Recording only captures
	// while a stream is published, so a camera records while its live view is
	// active. Empty (the default) disables recording and keeps the generated
	// configuration byte-identical to deploy/mediamtx.yml.
	RecordDir string
}

// DefaultGatewayOrigin is the origin of the web UI on its default address.
const DefaultGatewayOrigin = "http://127.0.0.1:8654"

// hashedPass returns MediaMTX's "sha256:" form of a password, so the
// generated file on disk never holds the stream password itself.
func hashedPass(pass string) string {
	sum := sha256.Sum256([]byte(pass))
	return "sha256:" + base64.StdEncoding.EncodeToString(sum[:])
}

// yamlSingleQuoted quotes s for YAML (” escapes a quote).
func yamlSingleQuoted(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// DefaultCacheDir returns the platform-specific cache directory for bombecam binaries.
func DefaultCacheDir() string {
	if runtime.GOOS == "windows" {
		localApp := os.Getenv("LOCALAPPDATA")
		if localApp == "" {
			home := os.Getenv("USERPROFILE")
			if home != "" {
				localApp = filepath.Join(home, "AppData", "Local")
			} else {
				localApp = os.TempDir()
			}
		}
		return filepath.Join(localApp, "bombecam", "bin")
	}

	// Linux / Darwin / UNIX
	dataHome := os.Getenv("XDG_DATA_HOME")
	if dataHome != "" {
		return filepath.Join(dataHome, "bombecam", "bin")
	}
	home := os.Getenv("HOME")
	if home != "" {
		return filepath.Join(home, ".local", "share", "bombecam", "bin")
	}
	return filepath.Join(os.TempDir(), "bombecam", "bin")
}

// mediamtxArchives are the official MediaMTX release archives BombeCam may
// download, with their SHA-256 checksums. A downloaded archive must match; any
// other archive is refused.
var mediamtxArchives = map[string]string{
	"mediamtx_v1.9.3_windows_amd64.zip":    "af2ce0dce3201e10c39dae4e6d8e52c983d6940b1fa0fdd7f3d16c87ac764626",
	"mediamtx_v1.9.3_linux_amd64.tar.gz":   "0b885dbfa4ef9c14cd00191c57d90d804255ff50403a28b85ceee7988c535b60",
	"mediamtx_v1.9.3_linux_arm64v8.tar.gz": "f2f02109dd3d88773d7de5ae84385d41041bc9d60d0459eb6c61462cb2da0d1e",
	"mediamtx_v1.9.3_linux_armv6.tar.gz":   "7b795b6b7d942937ec48afb89e9c1f0529e5094dfeb2cd87b69fdf9e33abc8b8",
	"mediamtx_v1.9.3_darwin_amd64.tar.gz":  "935ed174245004b425a93f6b7c1eac52310d833d4ebbca041d0f19195291eaf6",
	"mediamtx_v1.9.3_darwin_arm64.tar.gz":  "562b7f8fe24d9005510efe1d15589119a2bf4e77e6a4d3fe35b1d3f55c75263c",
}

// releaseArchive names the MediaMTX release archive for a platform, or "" if
// MediaMTX publishes none. (Its 64-bit ARM Linux build is "arm64v8"; the
// armv6 build also runs on armv7.)
func releaseArchive(goos, goarch string) string {
	platform := map[string]string{
		"windows/amd64": "windows_amd64.zip",
		"linux/amd64":   "linux_amd64.tar.gz",
		"linux/arm64":   "linux_arm64v8.tar.gz",
		"linux/arm":     "linux_armv6.tar.gz",
		"darwin/amd64":  "darwin_amd64.tar.gz",
		"darwin/arm64":  "darwin_arm64.tar.gz",
	}[goos+"/"+goarch]
	if platform == "" {
		return ""
	}
	return fmt.Sprintf("mediamtx_%s_%s", MediaMTXVersion, platform)
}

// DefaultDownloadURL returns the official GitHub release URL of MediaMTX for
// the current OS and architecture, or "" if there is none.
func DefaultDownloadURL() string {
	archive := releaseArchive(runtime.GOOS, runtime.GOARCH)
	if archive == "" {
		return ""
	}
	return fmt.Sprintf("https://github.com/bluenviron/mediamtx/releases/download/%s/%s", MediaMTXVersion, archive)
}

// PinnedSHA256 returns the checksum a download from downloadURL must have.
// ok is false for any archive BombeCam does not know.
func PinnedSHA256(downloadURL string) (sum string, ok bool) {
	i := strings.LastIndex(downloadURL, "/")
	sum, ok = mediamtxArchives[downloadURL[i+1:]]
	return sum, ok
}

// DefaultConfig returns a Config populated with standard defaults.
func DefaultConfig() Config {
	return Config{
		RTSPPort:       DefaultRTSPPort,
		HTTPPort:       DefaultHTTPPort,
		WebRTCPort:     DefaultWebRTCPort,
		APIPort:        DefaultAPIPort,
		WebRTCICEPort:  DefaultWebRTCICEPort,
		RTPPort:        DefaultRTPPort,
		ConfigPath:     DefaultConfigPath,
		CacheDir:       DefaultCacheDir(),
		StartupTimeout: DefaultStartupTimeout,
		ProbeTimeout:   DefaultProbeTimeout,
		AutoDownload:   true,
		DownloadURL:    DefaultDownloadURL(),
	}
}

// GeneratedConfigName is the file name of the configuration BombeCam writes into
// its cache directory on every launch. It deliberately differs from the
// "mediamtx.yml" that ships inside the MediaMTX release archive, which is
// extracted into the same directory.
const GeneratedConfigName = "bombecam-mediamtx.yml"

// GenerateConfig returns a complete MediaMTX v1.9.x configuration for the
// given ports. Every key in here must exist in the pinned MediaMTX release:
// MediaMTX refuses to start on an unknown key.
// TestGenerateConfig_LoadsInPinnedMediaMTX guards this when a binary is present.
// With default settings the result matches deploy/mediamtx.yml
// (TestDeployConfigMatchesGenerated).
func GenerateConfig(cfg Config) string {
	rtsp, hls, webrtc, api, ice := cfg.RTSPPort, cfg.HTTPPort, cfg.WebRTCPort, cfg.APIPort, cfg.WebRTCICEPort
	rtp := cfg.RTPPort
	if rtp <= 0 {
		rtp = DefaultRTPPort
	}
	if rtsp <= 0 {
		rtsp = DefaultRTSPPort
	}
	if hls <= 0 {
		hls = DefaultHTTPPort
	}
	if webrtc <= 0 {
		webrtc = DefaultWebRTCPort
	}
	if api <= 0 {
		api = DefaultAPIPort
	}
	if ice <= 0 {
		ice = DefaultWebRTCICEPort
	}
	origin := strings.TrimRight(strings.TrimSpace(cfg.GatewayOrigin), "/")
	if origin == "" {
		origin = DefaultGatewayOrigin
	}

	readers := `  - user: any
    pass:
    ips: []
    permissions:
      - action: read
      - action: playback
`
	readNote := "# Anyone on the network may read (RTSP/HLS/WebRTC) so NVRs like Frigate can connect."
	if cfg.ReadUser != "" {
		readers = fmt.Sprintf(`  - user: %s
    pass: %s
    ips: []
    permissions:
      - action: read
      - action: playback
`, yamlSingleQuoted(cfg.ReadUser), yamlSingleQuoted(hashedPass(cfg.ReadPass)))
		readNote = "# Other devices need the stream user name and password to read (the \"stream\n# password\" switch in BombeCam)."
	}

	// Optional recording. "all_others" is the catch-all path, so a record block
	// there turns recording on for every stream. The block is emitted only when
	// a recording directory is chosen, so the default output stays
	// byte-identical to deploy/mediamtx.yml.
	record := ""
	if strings.TrimSpace(cfg.RecordDir) != "" {
		record = "\n  record: yes\n  recordPath: " + yamlSingleQuoted(filepath.Join(cfg.RecordDir, "%path/%Y-%m-%d_%H-%M-%S")) + "\n  recordFormat: fmp4\n  recordSegmentDuration: 1s"
	}
	const base = `# Generated by BombeCam on every launch - edits here are overwritten.
# To use your own file instead, start BombeCam with -mediamtx-config <path>.
logLevel: info
logDestinations: [stdout]

# Only the gateway on this machine may publish or use the management API; it
# reads without a password too (snapshots, friendly stream names).
%s
authMethod: internal
authInternalUsers:
  - user: any
    pass:
    ips: ['127.0.0.1', '::1']
    permissions:
      - action: publish
      - action: read
      - action: playback
      - action: api
      - action: metrics
      - action: pprof
%s
api: yes
apiAddress: 127.0.0.1:%d
# MediaMTX answers cross-origin requests from any web page unless told
# otherwise. BombeCam's viewer reaches the API, HLS and WebRTC through the
# gateway, so only the gateway's own origin is allowed.
apiAllowOrigin: %s

rtsp: yes
rtspAddress: :%d
protocols: [tcp, udp]
# RTSP over UDP. If another program holds these, BombeCam picks the next free
# pair; players learn the ports when they connect, so nothing else changes.
rtpAddress: :%d
rtcpAddress: :%d

rtmp: no
srt: no

hls: yes
hlsAddress: :%d
hlsAllowOrigin: %s
hlsAlwaysRemux: yes
hlsVariant: mpegts
hlsSegmentCount: 7
hlsSegmentDuration: 1s

webrtc: yes
webrtcAddress: :%d
webrtcAllowOrigin: %s
webrtcLocalUDPAddress: :%d

paths:
  all_others:%s
`
	rendered := fmt.Sprintf(base,
		readNote, readers, api, yamlSingleQuoted(origin),
		rtsp, rtp, rtp+1, hls, yamlSingleQuoted(origin),
		webrtc, yamlSingleQuoted(origin), ice, record)

	return rendered
}

// MinimalMediaMTXConfig generates the default configuration with the given ports.
func MinimalMediaMTXConfig(rtspPort, hlsPort, apiPort int) string {
	cfg := DefaultConfig()
	cfg.RTSPPort, cfg.HTTPPort, cfg.APIPort = rtspPort, hlsPort, apiPort
	return GenerateConfig(cfg)
}
