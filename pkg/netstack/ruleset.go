package netstack

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"os/exec"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Fever-r/BombeCam/pkg/policy"
	"github.com/Fever-r/BombeCam/pkg/renderer/nftables"
)

// RulesetManager applies the "Block cloud video" policy on a Linux router the
// cameras route through (bombecam-net). It does not generate rules itself:
// rendering lives in pkg/renderer/nftables. OpenWrt routers use the router
// script in pkg/renderer/openwrt instead.
type RulesetConfig struct {
	// Cameras as "Name=MAC" or bare "MAC" entries. Subjects are identified by
	// MAC; there is deliberately no default.
	Cameras          []string
	BlockStreamSetup bool
	// Region selects the Osaio endpoints the allowlist trusts ("us" or
	// "eu"). Empty means the default (US).
	Region policy.Region
	// Optional return-path NAT when this box routes the camera subnet itself.
	EnableMasq   bool
	WANInterface string
	CameraSubnet string
	// Resolver looks up the allowlisted host names. Defaults to DefaultResolver.
	Resolver func(ctx context.Context, host string) ([]net.IP, error)
}

// ParseCameras turns "Name=MAC" / "MAC" entries into policy subjects.
func ParseCameras(entries []string) ([]policy.Subject, error) {
	var subs []policy.Subject
	for i, e := range entries {
		e = strings.TrimSpace(e)
		if e == "" {
			continue
		}
		name, mac := "", e
		if k := strings.LastIndex(e, "="); k >= 0 {
			name, mac = strings.TrimSpace(e[:k]), strings.TrimSpace(e[k+1:])
		}
		if name == "" {
			name = fmt.Sprintf("camera_%d", i+1)
		}
		subs = append(subs, policy.Subject{ID: fmt.Sprintf("camera_%d", i+1), Name: name, MAC: mac})
	}
	if len(subs) == 0 {
		return nil, fmt.Errorf("no camera MAC configured: cameras are identified by MAC address (set CAMERA_MAC or --camera)")
	}
	return subs, nil
}

type resolvedAddr struct {
	ip       net.IP
	lastSeen time.Time
}

// RulesetManager holds the applied state.
type RulesetManager struct {
	mu           sync.RWMutex
	cfg          RulesetConfig
	subjects     []policy.Subject
	renderer     *nftables.Renderer
	addrs        map[string]map[string]resolvedAddr // host -> ip string -> addr
	active       bool                               // BlockCloudVideo currently applied
	applied      bool                               // something has been applied this run
	LastRendered string
	LastHash     string
}

// KeepResolvedFor is how long an address stays allowed after DNS last returned
// it, so a load-balancer change does not cut an open control connection.
const KeepResolvedFor = 7 * 24 * time.Hour

// NewRulesetManager validates the config.
func NewRulesetManager(cfg RulesetConfig) (*RulesetManager, error) {
	subs, err := ParseCameras(cfg.Cameras)
	if err != nil {
		return nil, err
	}
	// Validate subjects now so a bad MAC fails at startup, not at apply time.
	if _, err := policy.Compile(subs, policy.Setting{BlockCloudVideo: true, Region: cfg.Region}, policy.Options{}); err != nil {
		return nil, err
	}
	rcfg := nftables.DefaultConfig()
	rcfg.EnableMasq = cfg.EnableMasq
	rcfg.WANInterface = cfg.WANInterface
	rcfg.CameraSubnet = cfg.CameraSubnet
	r, err := nftables.NewRenderer(rcfg)
	if err != nil {
		return nil, err
	}
	if cfg.Resolver == nil {
		cfg.Resolver = DefaultResolver
	}
	return &RulesetManager{cfg: cfg, subjects: subs, renderer: r, addrs: map[string]map[string]resolvedAddr{}}, nil
}

// Document compiles the policy for the given setting.
func (m *RulesetManager) Document(block bool) (*policy.Document, error) {
	return policy.Compile(m.subjects, policy.Setting{BlockCloudVideo: block, Region: m.cfg.Region},
		policy.Options{BlockStreamSetup: m.cfg.BlockStreamSetup})
}

// Region returns the Osaio region the allowlist is built for.
func (m *RulesetManager) Region() policy.Region { return m.cfg.Region.OrDefault() }

// Resolved returns the currently allowed addresses per host.
func (m *RulesetManager) Resolved() map[string][]net.IP {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.resolvedLocked()
}

func (m *RulesetManager) resolvedLocked() map[string][]net.IP {
	out := map[string][]net.IP{}
	for h, set := range m.addrs {
		for _, a := range set {
			out[h] = append(out[h], a.ip)
		}
		sort.Slice(out[h], func(i, j int) bool { return out[h][i].String() < out[h][j].String() })
	}
	return out
}

// Resolve refreshes the allowlisted host addresses. It reports whether the
// address set changed (the rules then need re-applying). Lookup failures keep
// the previous addresses.
func (m *RulesetManager) Resolve(ctx context.Context) (bool, error) {
	doc, err := m.Document(true)
	if err != nil {
		return false, err
	}
	now := time.Now()
	fresh := map[string][]net.IP{}
	var errs []string
	for _, h := range doc.Hosts() {
		ips, err := m.cfg.Resolver(ctx, h)
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", h, err))
		}
		fresh[h] = ips
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	before := fmt.Sprint(m.resolvedLocked())
	for h, ips := range fresh {
		if m.addrs[h] == nil {
			m.addrs[h] = map[string]resolvedAddr{}
		}
		for _, ip := range ips {
			if v4 := ip.To4(); v4 != nil {
				m.addrs[h][v4.String()] = resolvedAddr{ip: v4, lastSeen: now}
			}
		}
	}
	for h, set := range m.addrs {
		for k, a := range set {
			if now.Sub(a.lastSeen) > KeepResolvedFor {
				delete(set, k)
			}
		}
		if len(set) == 0 {
			delete(m.addrs, h)
		}
	}
	changed := before != fmt.Sprint(m.resolvedLocked())
	if len(errs) > 0 {
		return changed, fmt.Errorf("lookup failed: %s", strings.Join(errs, "; "))
	}
	return changed, nil
}

// Render compiles the nftables ruleset for the setting.
func (m *RulesetManager) Render(block bool) (string, error) {
	doc, err := m.Document(block)
	if err != nil {
		return "", err
	}
	m.mu.RLock()
	resolved := m.resolvedLocked()
	m.mu.RUnlock()
	return m.renderer.Compile(doc, resolved)
}

// Warnings returns non-fatal problems from the last render.
func (m *RulesetManager) Warnings() []string { return m.renderer.Warnings() }

// ComputeHash returns the SHA-256 of a ruleset.
func ComputeHash(ruleset string) string {
	h := sha256.Sum256([]byte(ruleset))
	return hex.EncodeToString(h[:])
}

// Apply loads the ruleset for the setting in one nft transaction and checks
// the kernel holds what was compiled. Every failure is an error.
func (m *RulesetManager) Apply(block bool) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return m.ApplyContext(ctx, block)
}

func (m *RulesetManager) ApplyContext(ctx context.Context, block bool) error {
	ruleset, err := m.Render(block)
	if err != nil {
		return fmt.Errorf("render ruleset failed: %w", err)
	}
	return m.applyRendered(ctx, ruleset, block)
}

func (m *RulesetManager) applyRendered(ctx context.Context, ruleset string, block bool) error {
	if runtime.GOOS != "linux" {
		return fmt.Errorf("the camera firewall runs on Linux routers; refusing to report it active on %s", runtime.GOOS)
	}
	if _, err := exec.LookPath("nft"); err != nil {
		return fmt.Errorf("nftables not installed: cannot apply the camera firewall")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	cmd := exec.CommandContext(ctx, "nft", "-f", "-")
	cmd.Stdin = strings.NewReader(ruleset)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("nft transaction failed: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	out, lerr := exec.CommandContext(ctx, "nft", "list", "table", "inet", "bombecam").CombinedOutput()
	present := lerr == nil && strings.Contains(string(out), "chain camera_out")
	if block && !present {
		return fmt.Errorf("readback did not find the BombeCam table after applying: the kernel does not hold the compiled rules (%s)",
			strings.TrimSpace(string(out)))
	}
	if !block && lerr == nil {
		return fmt.Errorf("BombeCam table still present after removing it")
	}
	m.active = block
	m.applied = true
	m.LastRendered = ruleset
	m.LastHash = ComputeHash(ruleset)
	return nil
}

// Active reports the setting last applied, and whether anything was applied.
func (m *RulesetManager) Active() (block bool, applied bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.active, m.applied
}

// GetLastHash returns the hash of the last applied ruleset.
func (m *RulesetManager) GetLastHash() string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.LastHash
}

// ValidateSyntax checks the rendered ruleset with `nft -c` without loading it.
func (m *RulesetManager) ValidateSyntax(block bool) error {
	ruleset, err := m.Render(block)
	if err != nil {
		return err
	}
	if _, err := exec.LookPath("nft"); err != nil {
		return fmt.Errorf("nftables not installed: cannot validate ruleset")
	}
	cmd := exec.Command("nft", "-c", "-f", "-")
	cmd.Stdin = strings.NewReader(ruleset)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("nft syntax check failed: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// PublicResolvers are asked in addition to the system resolver, because the
// cameras themselves query 8.8.8.8 directly.
var PublicResolvers = []string{"8.8.8.8:53", "8.8.4.4:53"}

// DefaultResolver returns the union of IPv4 answers from the system resolver
// and the public resolvers the cameras use.
func DefaultResolver(ctx context.Context, host string) ([]net.IP, error) {
	seen := map[string]bool{}
	var out []net.IP
	var lastErr error
	add := func(r *net.Resolver) {
		c, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		ips, err := r.LookupIP(c, "ip4", host)
		if err != nil {
			lastErr = err
			return
		}
		for _, ip := range ips {
			if !seen[ip.String()] {
				seen[ip.String()] = true
				out = append(out, ip)
			}
		}
	}
	add(net.DefaultResolver)
	for _, srv := range PublicResolvers {
		srv := srv
		add(&net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, network, srv)
		}})
	}
	if len(out) == 0 && lastErr != nil {
		return nil, lastErr
	}
	return out, nil
}
