// Package prober implements verifier.ProbeExecutor against the real system.
//
// Every method here measures something or returns an error. Nothing returns a
// bare `true`. The whole point of this package is that the sentence
// "your camera is cut off" is only ever printed because something was observed.
//
// Requires Linux and CAP_NET_ADMIN/CAP_NET_RAW (in practice, root).
package prober

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Fever-r/BombeCam/pkg/verifier"
)

// Config describes the boundary this prober measures.
type Config struct {
	CameraInterface string // link the camera is attached to
	CameraSubnet    string // e.g. 10.0.0.0/24
	CanaryIP        string // an address on the camera segment we own and probe FROM
	TableName       string // nftables table holding the policy
	NATTableName    string // nftables table holding the return-path NAT
	LeaseFile       string // dnsmasq lease file (empty = skip the lease check)
	// CanaryNetns is the network namespace the canary lives in. Probes must
	// originate from a HOST ON THE CAMERA SEGMENT, not from the appliance:
	// locally-generated packets never traverse the forward hook, so a probe
	// sent from the appliance itself tests a path the camera never uses and
	// proves nothing about whether the camera's egress is blocked. Empty
	// means probe from the default namespace (for unit testing only;
	// New enforces CanaryNetns in production).
	CanaryNetns   string
	ProbeTimeout  time.Duration
	CaptureWindow time.Duration // how long to wait for an attributed frame
	// ApplyTemporaryControl installs and reads back a kernel-expiring canary-only lease.
	ApplyTemporaryControl func(context.Context, time.Duration) error
}

// DefaultConfig returns base default configuration for non-network-topology values.
func DefaultConfig() Config {
	return Config{
		TableName:     "bombecam",
		NATTableName:  "bombecam_nat",
		LeaseFile:     "/var/lib/misc/dnsmasq.leases",
		CanaryNetns:   "canary",
		ProbeTimeout:  1500 * time.Millisecond,
		CaptureWindow: 3 * time.Second,
	}
}

// Prober is a real, measuring implementation of verifier.ProbeExecutor.
type Prober struct {
	cfg Config

	mu           sync.Mutex
	modeApplied  int // -1 unknown, 0 off, 1 blocking
	counterBase  uint64
	counterKnown bool
	applyMode    func(ctx context.Context, block bool) error
}

// New builds a Prober. applyMode is the function that turns "Block cloud
// video" on or off (normally netstack.RulesetManager.Apply); it must not be
// nil, because a verifier that cannot switch it cannot run a differential.
func New(cfg Config, applyMode func(ctx context.Context, block bool) error) (*Prober, error) {
	if applyMode == nil {
		return nil, fmt.Errorf("applyMode must be supplied: without it there is no control leg, " +
			"and a single blocked probe carries no information")
	}
	if cfg.CameraInterface == "" {
		return nil, fmt.Errorf("CameraInterface is required")
	}
	if cfg.CameraSubnet == "" {
		return nil, fmt.Errorf("CameraSubnet is required")
	}
	if _, _, err := net.ParseCIDR(cfg.CameraSubnet); err != nil {
		return nil, fmt.Errorf("invalid camera subnet %q: %w", cfg.CameraSubnet, err)
	}
	if cfg.CanaryIP == "" {
		return nil, fmt.Errorf("CanaryIP is required")
	}
	if net.ParseIP(cfg.CanaryIP) == nil {
		return nil, fmt.Errorf("invalid canary IP %q", cfg.CanaryIP)
	}

	d := DefaultConfig()
	if cfg.TableName == "" {
		cfg.TableName = d.TableName
	}
	if cfg.NATTableName == "" {
		cfg.NATTableName = d.NATTableName
	}
	if cfg.LeaseFile == "" {
		cfg.LeaseFile = d.LeaseFile
	}
	if cfg.CanaryNetns == "" {
		cfg.CanaryNetns = d.CanaryNetns
	}
	if cfg.ProbeTimeout <= 0 {
		cfg.ProbeTimeout = d.ProbeTimeout
	}
	if cfg.CaptureWindow <= 0 {
		cfg.CaptureWindow = d.CaptureWindow
	}
	return &Prober{cfg: cfg, applyMode: applyMode, modeApplied: -1}, nil
}

var _ verifier.ProbeExecutor = (*Prober)(nil)

// ---------------------------------------------------------------- preconditions

// CheckPreconditions checks what a verification run needs. Each of
// the three checks queries the system; a failure names which one failed, so a
// broken network is never mistaken for a policy verdict.
func (p *Prober) CheckPreconditions(ctx context.Context, cameraIP, cameraMAC string) (bool, error) {
	if err := p.checkCameraAdjacent(ctx, cameraIP, cameraMAC); err != nil {
		return false, err
	}
	if err := p.checkCanaryPresent(); err != nil {
		return false, err
	}
	// Verify that a return path exists; without a return path, every
	// probe fails for a routing reason and blocking "passes" for the wrong one.
	if err := p.checkNATPresent(ctx); err != nil {
		return false, err
	}
	return true, nil
}

// checkCameraAdjacent confirms the camera is a live neighbour on our segment.
func (p *Prober) checkCameraAdjacent(ctx context.Context, cameraIP, cameraMAC string) error {
	out, err := runCmd(ctx, "ip", "-j", "neigh", "show", "dev", p.cfg.CameraInterface)
	if err != nil {
		return fmt.Errorf("cannot read the neighbour table for %s: %w", p.cfg.CameraInterface, err)
	}
	var entries []struct {
		Dst    string `json:"dst"`
		LLAddr string `json:"lladdr"`
		State  []string
	}
	if jerr := json.Unmarshal([]byte(out), &entries); jerr != nil {
		return fmt.Errorf("cannot parse the neighbour table: %w", jerr)
	}
	want := strings.ToLower(strings.TrimSpace(cameraMAC))
	for _, e := range entries {
		if strings.ToLower(e.LLAddr) != want {
			continue
		}
		for _, s := range e.State {
			switch strings.ToUpper(s) {
			case "REACHABLE", "STALE", "DELAY", "PROBE", "PERMANENT":
				return nil
			}
		}
		return fmt.Errorf("camera %s is in the neighbour table but not reachable (state %v)", cameraMAC, e.State)
	}
	return fmt.Errorf("camera %s (%s) is not a neighbour on %s: it is not on this segment, "+
		"so nothing measured here would be about that camera", cameraMAC, cameraIP, p.cfg.CameraInterface)
}

// checkCanaryPresent confirms the canary holds the address we probe from, and
// that the address is on the camera's segment. When the canary lives in its own
// namespace (the correct arrangement — see Config.CanaryNetns) the address is
// checked there.
func (p *Prober) checkCanaryPresent() error {
	_, subnet, err := net.ParseCIDR(p.cfg.CameraSubnet)
	if err != nil {
		return err
	}
	if !subnet.Contains(net.ParseIP(p.cfg.CanaryIP)) {
		return fmt.Errorf("canary %s is not on the camera segment %s: a probe from it would not "+
			"traverse the camera's path", p.cfg.CanaryIP, p.cfg.CameraSubnet)
	}
	if p.cfg.CanaryNetns != "" {
		out, err := runCmd(context.Background(), "ip", "-n", p.cfg.CanaryNetns, "-o", "addr", "show")
		if err != nil {
			return fmt.Errorf("canary namespace %q is not usable: %w", p.cfg.CanaryNetns, err)
		}
		if !strings.Contains(out, p.cfg.CanaryIP) {
			return fmt.Errorf("canary address %s is not assigned inside namespace %q",
				p.cfg.CanaryIP, p.cfg.CanaryNetns)
		}
		return nil
	}
	return p.checkCanaryOnInterface()
}

func (p *Prober) checkCanaryOnInterface() error {
	iface, err := net.InterfaceByName(p.cfg.CameraInterface)
	if err != nil {
		return fmt.Errorf("camera interface %s does not exist: %w", p.cfg.CameraInterface, err)
	}
	addrs, err := iface.Addrs()
	if err != nil {
		return fmt.Errorf("cannot list addresses on %s: %w", p.cfg.CameraInterface, err)
	}
	for _, a := range addrs {
		ip, _, perr := net.ParseCIDR(a.String())
		if perr == nil && ip.String() == p.cfg.CanaryIP {
			return nil
		}
	}
	return fmt.Errorf("canary address %s is not assigned to %s: the probe would not leave from the "+
		"camera's segment and would test a different path", p.cfg.CanaryIP, p.cfg.CameraInterface)
}

// checkNATPresent confirms a return path exists for the camera subnet.
func (p *Prober) checkNATPresent(ctx context.Context) error {
	out, err := runCmd(ctx, "nft", "list", "table", "ip", p.cfg.NATTableName)
	if err != nil {
		return fmt.Errorf("no return-path NAT table %q: without it the camera has no working route "+
			"out, every probe fails for a routing reason, and a blocking 'pass' would mean nothing",
			p.cfg.NATTableName)
	}
	if !strings.Contains(out, "masquerade") && !strings.Contains(out, "snat") {
		return fmt.Errorf("NAT table %q holds no masquerade/SNAT rule", p.cfg.NATTableName)
	}
	if !strings.Contains(out, p.cfg.CameraSubnet) {
		return fmt.Errorf("NAT table %q does not cover the camera subnet %s",
			p.cfg.NATTableName, p.cfg.CameraSubnet)
	}
	return nil
}

// ---------------------------------------------------------------- probes

// ExecuteProbe attempts to reach probe.TargetHost FROM the canary address, so
// the packet traverses the same forward path the camera's traffic would.
// Returns true when the destination was reached.
func (p *Prober) ExecuteProbe(ctx context.Context, probe verifier.NetworkProbe, fromCanary bool) bool {
	target := net.JoinHostPort(probe.TargetHost, strconv.Itoa(probe.Port))

	// Run the probe inside the canary's namespace by re-executing ourselves.
	// This keeps the probe on the camera's forward path with no extra runtime
	// dependency on the appliance.
	if fromCanary && p.cfg.CanaryNetns != "" {
		exe, err := os.Executable()
		if err != nil {
			return false
		}
		cmd := exec.CommandContext(ctx, "ip", "netns", "exec", p.cfg.CanaryNetns, exe,
			"-probe-once", fmt.Sprintf("%s/%s/%d/%s",
				strings.ToLower(probe.Protocol), probe.TargetHost, probe.Port,
				p.cfg.ProbeTimeout.String()))
		return cmd.Run() == nil
	}

	local := &net.TCPAddr{IP: net.ParseIP(p.cfg.CanaryIP)}
	dialer := &net.Dialer{Timeout: p.cfg.ProbeTimeout}
	if fromCanary {
		dialer.LocalAddr = local
	}

	switch strings.ToLower(probe.Protocol) {
	case "tcp":
		conn, err := dialer.DialContext(ctx, "tcp", target)
		if err != nil {
			return false
		}
		_ = conn.Close()
		return true

	case "udp":
		ua := &net.UDPAddr{IP: net.ParseIP(p.cfg.CanaryIP)}
		var d net.Dialer
		d.Timeout = p.cfg.ProbeTimeout
		if fromCanary {
			d.LocalAddr = ua
		}
		conn, err := d.DialContext(ctx, "udp", target)
		if err != nil {
			return false
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(p.cfg.ProbeTimeout))
		// A UDP socket connect() never fails on a dropped path, so we must
		// elicit a reply. For port 53 send a real query; otherwise a probe byte
		// and wait for anything (including an ICMP error surfaced as a read error).
		payload := []byte{0x01}
		if probe.Port == 53 {
			payload = dnsQuery("example.com")
		}
		if _, err := conn.Write(payload); err != nil {
			return false
		}
		buf := make([]byte, 1500)
		n, err := conn.Read(buf)
		return err == nil && n > 0

	default:
		return false
	}
}

// dnsQuery builds a minimal A-record query so a UDP/53 probe can be answered.
func dnsQuery(name string) []byte {
	var b []byte
	b = append(b, 0x13, 0x37) // id
	b = append(b, 0x01, 0x00) // standard query, recursion desired
	b = append(b, 0x00, 0x01) // qdcount
	b = append(b, 0, 0, 0, 0, 0, 0)
	for _, label := range strings.Split(name, ".") {
		b = append(b, byte(len(label)))
		b = append(b, []byte(label)...)
	}
	b = append(b, 0x00)
	b = append(b, 0x00, 0x01) // A
	b = append(b, 0x00, 0x01) // IN
	return b
}

// ---------------------------------------------------------------- counters

// ApplyMode switches blocking and snapshots the terminal deny counter, so a
// later CheckCounterIncrement compares like with like.
func (p *Prober) ApplyMode(ctx context.Context, block bool) error {
	if !block {
		return fmt.Errorf("unbounded blocking-off verification is prohibited")
	}
	if err := p.applyMode(ctx, block); err != nil {
		return err
	}
	mode := 0
	if block {
		mode = 1
	}
	p.mu.Lock()
	p.modeApplied = mode
	p.mu.Unlock()

	base, err := p.readDenyCounter(ctx)
	p.mu.Lock()
	p.counterBase, p.counterKnown = base, block && err == nil
	p.mu.Unlock()
	return nil
}

func (p *Prober) BeginControlLeg(ctx context.Context, ttl time.Duration) error {
	if ttl < time.Second || ttl > 30*time.Second || ttl%time.Second != 0 {
		return fmt.Errorf("invalid canary lease duration")
	}
	if p.cfg.ApplyTemporaryControl == nil {
		return fmt.Errorf("kernel-expiring canary lease is not configured")
	}
	if err := p.cfg.ApplyTemporaryControl(ctx, ttl); err != nil {
		return err
	}
	p.mu.Lock()
	p.modeApplied = 0
	p.counterKnown = false
	p.mu.Unlock()
	return nil
}

// CheckCounterIncrement reports whether the terminal deny rule counted more
// packets since blocking was switched on. A read failure is returned as an
// error, never as "the counter did not move".
func (p *Prober) CheckCounterIncrement(ctx context.Context) (bool, error) {
	p.mu.Lock()
	mode, base, known := p.modeApplied, p.counterBase, p.counterKnown
	p.mu.Unlock()

	if mode != 1 {
		return false, fmt.Errorf("blocking is not on, so there is no deny counter to compare")
	}
	if !known {
		return false, fmt.Errorf("the deny-rule counter could not be read when blocking was switched on")
	}
	now, err := p.readDenyCounter(ctx)
	if err != nil {
		return false, fmt.Errorf("cannot read the deny-rule counter: %w", err)
	}
	return now > base, nil
}

// readDenyCounter sums packet counters on drop rules in the camera_out chain.
func (p *Prober) readDenyCounter(ctx context.Context) (uint64, error) {
	chain := "camera_out"
	out, err := runCmd(ctx, "nft", "-j", "list", "chain", "inet", p.cfg.TableName, chain)
	if err != nil {
		return 0, fmt.Errorf("chain %s in table inet %s is not loaded: %w", chain, p.cfg.TableName, err)
	}
	var doc struct {
		Nftables []map[string]json.RawMessage `json:"nftables"`
	}
	if jerr := json.Unmarshal([]byte(out), &doc); jerr != nil {
		return 0, fmt.Errorf("cannot parse nft JSON: %w", jerr)
	}
	var total uint64
	var sawRule bool
	for _, node := range doc.Nftables {
		raw, ok := node["rule"]
		if !ok {
			continue
		}
		var rule struct {
			Expr []map[string]json.RawMessage `json:"expr"`
		}
		if json.Unmarshal(raw, &rule) != nil {
			continue
		}
		var packets uint64
		var isDrop bool
		for _, e := range rule.Expr {
			if c, ok := e["counter"]; ok {
				var ctr struct {
					Packets uint64 `json:"packets"`
				}
				if json.Unmarshal(c, &ctr) == nil {
					packets = ctr.Packets
				}
			}
			if _, ok := e["drop"]; ok {
				isDrop = true
			}
		}
		if isDrop {
			sawRule = true
			total += packets
		}
	}
	if !sawRule {
		return 0, fmt.Errorf("chain %s holds no counted drop rule to attest against", chain)
	}
	return total, nil
}

// -----------------// ---------------------------------------------------------------- L2 attribution
// AttributeL2 and LearnTTL are implemented in prober_linux.go (for Linux AF_PACKET)
// and prober_other.go (for non-Linux platforms).

var ErrRawCaptureUnsupported = errors.New("raw socket L2 capture is only supported on Linux")

// ---------------------------------------------------------------- helpers

func runCmd(ctx context.Context, name string, args ...string) (string, error) {
	if _, err := exec.LookPath(name); err != nil {
		return "", fmt.Errorf("%s is not installed: it is a hard dependency of enforcement verification", name)
	}
	cmd := exec.CommandContext(ctx, name, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("%s %s: %w (%s)", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

// ProbeOnce performs a single probe and is what the re-executed process runs
// inside the canary namespace. spec is "proto/host/port/timeout".
func ProbeOnce(spec string) error {
	parts := strings.Split(spec, "/")
	if len(parts) != 4 {
		return fmt.Errorf("probe spec must be proto/host/port/timeout, got %q", spec)
	}
	proto, host := strings.ToLower(parts[0]), parts[1]
	port, err := strconv.Atoi(parts[2])
	if err != nil {
		return err
	}
	to, err := time.ParseDuration(parts[3])
	if err != nil || to <= 0 {
		to = 3 * time.Second
	}
	target := net.JoinHostPort(host, strconv.Itoa(port))
	switch proto {
	case "tcp":
		c, err := net.DialTimeout("tcp", target, to)
		if err != nil {
			return err
		}
		return c.Close()
	case "udp":
		c, err := net.DialTimeout("udp", target, to)
		if err != nil {
			return err
		}
		defer c.Close()
		_ = c.SetDeadline(time.Now().Add(to))
		payload := []byte{0x13, 0x37, 'p', 'r', 'o', 'b', 'e'}
		if port == 53 {
			payload = dnsQuery("example.com")
		}
		if _, err := c.Write(payload); err != nil {
			return err
		}
		buf := make([]byte, 1500)
		n, err := c.Read(buf)
		if err != nil || n == 0 {
			return fmt.Errorf("no reply")
		}
		return nil
	}
	return fmt.Errorf("unsupported protocol %q", proto)
}
