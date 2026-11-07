// bombecam-net applies "Block cloud video" to the listed cameras on a Linux router that
// the cameras route through, using nftables. (OpenWrt and GL.iNet routers use
// the router script instead: `bombecam-net router-script`.)
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
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

	"github.com/Fever-r/BombeCam/pkg/netstack"
	"github.com/Fever-r/BombeCam/pkg/policy"
	"github.com/Fever-r/BombeCam/pkg/renderer/openwrt"
)

// parseRegion validates -region/BOMBECAM_REGION, exiting on a bad value.
func parseRegion(s string) policy.Region {
	r, err := policy.ParseRegion(s)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[!] %v\n", err)
		os.Exit(1)
	}
	return r
}

// regionString reports the region in force: the saved one when the state has
// it, else the flag.
func regionString(state netstack.PersistedState, flag string) string {
	if state.Region != "" {
		if r, err := policy.ParseRegion(state.Region); err == nil {
			return string(r)
		}
	}
	return string(parseRegion(flag))
}

func getEnvOrDefault(envKey, defVal string) string {
	if val := os.Getenv(envKey); val != "" {
		return val
	}
	return defVal
}

func getEnvIntOrDefault(envKey string, defVal int) int {
	if val := os.Getenv(envKey); val != "" {
		if n, err := strconv.Atoi(val); err == nil {
			return n
		}
	}
	return defVal
}

type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

// cameraEntries returns --camera flags, or CAMERA_MAC (comma-separated
// "Name=MAC" or "MAC"). There is deliberately no default.
func cameraEntries(flags []string) []string {
	if len(flags) > 0 {
		return flags
	}
	var out []string
	for _, p := range strings.Split(os.Getenv("CAMERA_MAC"), ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func parseYesNo(s string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "yes", "y", "true", "on", "1":
		return true, nil
	case "no", "n", "false", "off", "0":
		return false, nil
	}
	return false, fmt.Errorf("expected yes or no, got %q", s)
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

// resolveNetworkParams fills in the optional NAT parameters from the host.
func resolveNetworkParams(cameraIf, cameraSubnet, gatewayIP, wanIf *string) {
	if *cameraIf == "" || *cameraSubnet == "" || *gatewayIP == "" {
		if ifaces, err := netstack.GetDefaultDiscovery().ListInterfaces(); err == nil {
			for _, ifc := range ifaces {
				if ifc.IsUp && !ifc.IsLoopback && len(ifc.Subnets) > 0 && len(ifc.IPs) > 0 {
					if *cameraIf == "" {
						*cameraIf = ifc.Name
					}
					if *cameraSubnet == "" {
						*cameraSubnet = ifc.Subnets[0].String()
					}
					if *gatewayIP == "" {
						*gatewayIP = ifc.IPs[0].String()
					}
					break
				}
			}
		}
	}
	if *wanIf == "" {
		natMgr := netstack.NewNATManager(*cameraIf, *cameraSubnet, "")
		if detected, err := natMgr.DetectWANInterface(); err == nil {
			*wanIf = detected
		}
	}
}

type options struct {
	stateDir     string
	cameras      multiFlag
	blockSetup   bool
	region       string
	masq         bool
	cameraIf     string
	cameraSubnet string
	gatewayIP    string
	wanIf        string
	ntpPort      int
	apiHost      string
	apiPort      int
	apiKey       string
}

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}
	subcmd := os.Args[1]

	var o options
	fs := flag.NewFlagSet(subcmd, flag.ExitOnError)
	fs.StringVar(&o.stateDir, "state-dir", getEnvOrDefault("BOMBECAM_STATE_DIR", netstack.DefaultStateDir), "state directory")
	fs.Var(&o.cameras, "camera", `camera as "Name=MAC" (repeatable; or CAMERA_MAC, comma-separated)`)
	fs.BoolVar(&o.blockSetup, "block-stream-setup", os.Getenv("BOMBECAM_BLOCK_STREAM_SETUP") == "1", "also block Osaio's stream-setup server (may stop BombeCam starting local streams)")
	fs.StringVar(&o.region, "region", getEnvOrDefault("BOMBECAM_REGION", string(policy.DefaultRegion)), `Osaio region: "us" or "eu"`)
	fs.BoolVar(&o.masq, "masquerade", os.Getenv("BOMBECAM_MASQUERADE") == "1", "also add return-path NAT (only if this box routes the camera subnet and has no NAT of its own)")
	fs.StringVar(&o.cameraIf, "camera-if", getEnvOrDefault("CAMERA_VLAN_IF", ""), "camera network interface (NAT only)")
	fs.StringVar(&o.cameraSubnet, "camera-subnet", getEnvOrDefault("CAMERA_SUBNET", ""), "camera subnet CIDR (NAT only)")
	fs.StringVar(&o.gatewayIP, "gateway-ip", getEnvOrDefault("CAMERA_GATEWAY_IP", ""), "router address on the camera network (NTP responder bind)")
	fs.StringVar(&o.wanIf, "wan-if", getEnvOrDefault("WAN_INTERFACE", ""), "WAN interface (NAT only)")
	fs.IntVar(&o.ntpPort, "ntp-port", 123, "local NTP responder port")
	fs.StringVar(&o.apiHost, "api-host", getEnvOrDefault("BOMBECAM_NET_API_HOST", "127.0.0.1"), "local REST API bind host")
	fs.IntVar(&o.apiPort, "api-port", getEnvIntOrDefault("BOMBECAM_NET_API_PORT", 8653), "local REST API port")
	fs.StringVar(&o.apiKey, "api-key", getEnvOrDefault("BOMBECAM_NET_API_KEY", ""), "bearer token for the POST route")

	switch subcmd {
	case "daemon":
		_ = fs.Parse(os.Args[2:])
		resolveNetworkParams(&o.cameraIf, &o.cameraSubnet, &o.gatewayIP, &o.wanIf)
		runDaemon(o)

	case "block-cloud-video":
		if len(os.Args) < 3 {
			fmt.Fprintln(os.Stderr, "Usage: bombecam-net block-cloud-video <yes|no> --camera \"Name=MAC\" [...]")
			os.Exit(1)
		}
		block, err := parseYesNo(os.Args[2])
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
		_ = fs.Parse(os.Args[3:])
		resolveNetworkParams(&o.cameraIf, &o.cameraSubnet, &o.gatewayIP, &o.wanIf)
		runSetBlock(o, block)

	case "status":
		_ = fs.Parse(os.Args[2:])
		resolveNetworkParams(&o.cameraIf, &o.cameraSubnet, &o.gatewayIP, &o.wanIf)
		runStatus(o)

	case "rules":
		if len(os.Args) < 3 || os.Args[2] != "render" {
			fmt.Fprintln(os.Stderr, "Usage: bombecam-net rules render [--block yes|no] --camera \"Name=MAC\" [...]")
			os.Exit(1)
		}
		blockFlag := fs.String("block", "yes", "Block cloud video? yes or no")
		_ = fs.Parse(os.Args[3:])
		block, err := parseYesNo(*blockFlag)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
		runRender(o, block)

	case "router-script":
		out := fs.String("o", "", "write the OpenWrt/GL.iNet router script to this file (default stdout)")
		_ = fs.Parse(os.Args[2:])
		if *out == "" {
			_, _ = os.Stdout.Write(openwrt.Script())
			return
		}
		if err := os.WriteFile(*out, openwrt.Script(), 0o755); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("[+] wrote %s\n", *out)

	case "nat":
		if len(os.Args) < 3 {
			fmt.Fprintln(os.Stderr, "Usage: bombecam-net nat <apply|remove|status|render> [flags]")
			os.Exit(1)
		}
		action := os.Args[2]
		_ = fs.Parse(os.Args[3:])
		resolveNetworkParams(&o.cameraIf, &o.cameraSubnet, &o.gatewayIP, &o.wanIf)
		runNAT(o.cameraIf, o.cameraSubnet, o.wanIf, action)

	case "ntp":
		listen := fs.String("listen", "", "address to bind the NTP responder to (default: the gateway IP)")
		_ = fs.Parse(os.Args[2:])
		resolveNetworkParams(&o.cameraIf, &o.cameraSubnet, &o.gatewayIP, &o.wanIf)
		addr := *listen
		if addr == "" {
			addr = o.gatewayIP
		}
		runNTP(fmt.Sprintf("%s:%d", addr, o.ntpPort))

	case "help", "-h", "--help":
		printUsage()

	default:
		fmt.Fprintf(os.Stderr, "Unknown subcommand: %q\n", subcmd)
		printUsage()
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Println(`BombeCam camera firewall for Linux routers (bombecam-net)

"Block cloud video" for the cameras you list with --camera
  yes: each listed camera can still reach the Osaio control server (app
       controls keep working) plus DNS, time sync and the short stream-setup
       handshake, all sharing a ~4 KB/s cap per camera. Everything else from
       the camera to the internet, including IPv6, is dropped. Cameras you
       don't list are not affected.
  no:  BombeCam's rules are removed; the cameras work normally.

` + policy.Headline + `

Usage:
  bombecam-net block-cloud-video <yes|no> --camera "Front Door=AA:BB:CC:DD:EE:FF"
  bombecam-net status
  bombecam-net rules render [--block yes|no] --camera ...
  bombecam-net daemon                 keep the setting applied, re-resolve hosts, serve the local API
  bombecam-net router-script [-o f]   print the OpenWrt / GL.iNet router script
  bombecam-net nat <apply|remove|status|render>
  bombecam-net ntp                    run a local NTP responder in the foreground

Flags:
  --camera              "Name=MAC" (repeatable) or CAMERA_MAC="Name=MAC,..."
  --block-stream-setup  also block Osaio's stream-setup server (narrower; BombeCam may not be able to start local streams)
  --region              Osaio region for the allowlist: "us" (default) or "eu" (also BOMBECAM_REGION)
  --state-dir           persistent state directory (default /var/lib/bombecam)
  --masquerade          add return-path NAT (only for a box that routes the camera subnet without NAT of its own)
  --api-host/--api-port local REST API (default 127.0.0.1:8653)`)
}

func managerConfig(o options) netstack.RulesetConfig {
	return netstack.RulesetConfig{
		Cameras:          cameraEntries(o.cameras),
		BlockStreamSetup: o.blockSetup,
		Region:           parseRegion(o.region),
		EnableMasq:       o.masq,
		WANInterface:     o.wanIf,
		CameraSubnet:     o.cameraSubnet,
	}
}

func newManager(o options) *netstack.RulesetManager {
	m, err := netstack.NewRulesetManager(managerConfig(o))
	if err != nil {
		fmt.Fprintf(os.Stderr, "[!] %v\n", err)
		os.Exit(1)
	}
	return m
}

func resolveNow(m *netstack.RulesetManager) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := m.Resolve(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "[!] %v\n", err)
	}
}

func printWarnings(m *netstack.RulesetManager) {
	for _, w := range m.Warnings() {
		fmt.Fprintf(os.Stderr, "[warning] %s\n", w)
	}
}

func runRender(o options, block bool) {
	m := newManager(o)
	if block {
		resolveNow(m)
	}
	out, err := m.Render(block)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	printWarnings(m)
	fmt.Print(out)
}

func runSetBlock(o options, block bool) {
	m := newManager(o)
	if block {
		resolveNow(m)
	}
	sm := netstack.NewStateManager(o.stateDir)
	if err := m.Apply(block); err != nil {
		fmt.Fprintf(os.Stderr, "[!] could not apply Block cloud video = %s: %v\n", yesNo(block), err)
		if block {
			_ = sm.Persist(netstack.StateOptions{
				Block:            true,
				Region:           m.Region(),
				Cameras:          m.CameraMACs(),
				BlockStreamSetup: m.BlockStreamSetupEnabled(),
			}, "", netstack.StatusDegradedFailClosed)
			fmt.Fprintln(os.Stderr, "    Nothing was changed in the kernel; the camera is NOT protected.")
		}
		os.Exit(1)
	}
	printWarnings(m)
	status := netstack.StatusRulesLoaded
	if !block {
		status = netstack.StatusRemoved
	}
	if err := sm.Persist(netstack.StateOptions{
		Block:            block,
		Region:           m.Region(),
		Cameras:          m.CameraMACs(),
		BlockStreamSetup: m.BlockStreamSetupEnabled(),
	}, m.LastRendered, status); err != nil {
		fmt.Fprintf(os.Stderr, "[!] rules applied but the setting could not be saved: %v\n", err)
		os.Exit(1)
	}
	if block {
		fmt.Println("[+] Block cloud video: YES. " + policy.Headline)
		fmt.Println("    The rules are loaded. That does not prove this router is on the camera's")
		fmt.Println("    path; run bombecam-verify or the verification checklist for that.")
	} else {
		fmt.Println("[+] Block cloud video: NO. BombeCam's rules are removed; the camera works normally.")
	}
	fmt.Printf("    Ruleset hash: %s\n", m.GetLastHash())
}

func runStatus(o options) {
	sm := netstack.NewStateManager(o.stateDir)
	block, state, err := sm.RestoreOnBoot()
	loaded := false
	if out, lerr := exec.Command("nft", "list", "table", "inet", "bombecam").CombinedOutput(); lerr == nil {
		loaded = strings.Contains(string(out), "chain camera_out")
	}
	// Report the saved configuration; before the first write, say what the
	// flags would use.
	cameras := state.Cameras
	if len(cameras) == 0 {
		for _, e := range cameraEntries(o.cameras) {
			mac := e
			if k := strings.LastIndex(e, "="); k >= 0 {
				mac = e[k+1:]
			}
			if c := policy.CanonicalMAC(mac); c != "" {
				cameras = append(cameras, c)
			}
		}
	}
	blockSetup := state.BlockStreamSetup
	if !state.ConfigSaved {
		blockSetup = o.blockSetup
	}
	out := map[string]any{
		"block_cloud_video":  block,
		"region":             regionString(state, o.region),
		"cameras":            cameras,
		"block_stream_setup": blockSetup,
		"rules_loaded":       loaded,
		"state_status":       state.Status,
		"ruleset_hash":       state.RulesetHash,
		"updated_at":         state.UpdatedAt,
		"notes":              state.Notes,
		"nat":                netstack.NewNATManager(o.cameraIf, o.cameraSubnet, o.wanIf).GetStatus(),
	}
	if err != nil {
		out["read_error"] = err.Error()
	}
	data, _ := json.MarshalIndent(out, "", "  ")
	fmt.Println(string(data))
}

// configValues is the daemon's changing configuration.
type configValues struct {
	Block            bool
	Cameras          []string
	Region           policy.Region
	BlockStreamSetup bool
}

// configRequest is the body of POST /api/v1/net/config. Omitted fields keep
// their current value; cameras replaces the whole list.
type configRequest struct {
	BlockCloudVideo  *bool     `json:"block_cloud_video"`
	Cameras          *[]string `json:"cameras"`
	Region           *string   `json:"region"`
	BlockStreamSetup *bool     `json:"block_stream_setup"`
}

// merge returns the configuration the request asks for. It rejects an empty
// camera list and an unknown region; camera MACs are validated by
// Reconfigure.
func (req configRequest) merge(cur configValues) (configValues, error) {
	out := cur
	if req.BlockCloudVideo != nil {
		out.Block = *req.BlockCloudVideo
	}
	if req.Cameras != nil {
		if len(*req.Cameras) == 0 {
			// Nothing to block: the list is irrelevant while blocking is off,
			// so keep the current one for when blocking comes back on.
			if out.Block {
				return configValues{}, fmt.Errorf("cameras must not be empty while block_cloud_video is on")
			}
			out.Cameras = cur.Cameras
		} else {
			out.Cameras = *req.Cameras
		}
	}
	if req.Region != nil {
		region, err := policy.ParseRegion(*req.Region)
		if err != nil {
			return configValues{}, err
		}
		out.Region = region
	}
	if req.BlockStreamSetup != nil {
		out.BlockStreamSetup = *req.BlockStreamSetup
	}
	return out, nil
}

// statusPayload is the JSON body of GET /api/v1/net/status and of a successful
// POST /api/v1/net/config.
func statusPayload(m *netstack.RulesetManager) map[string]any {
	on, applied := m.Active()
	resolved := map[string][]string{}
	for h, ips := range m.Resolved() {
		for _, ip := range ips {
			resolved[h] = append(resolved[h], ip.String())
		}
	}
	return map[string]any{
		"block_cloud_video":  on,
		"applied":            applied,
		"ruleset_hash":       m.GetLastHash(),
		"allowed_addresses":  resolved,
		"headline":           policy.Headline,
		"os":                 runtime.GOOS,
		"region":             string(m.Region()),
		"cameras":            m.CameraMACs(),
		"block_stream_setup": m.BlockStreamSetupEnabled(),
	}
}

func writeAPIError(w http.ResponseWriter, code int, kind, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": kind, "message": message})
}

// handleNetConfig serves POST /api/v1/net/config. It does not authenticate;
// the daemon wraps it with checkAuth. mu is held across reconfigure, apply
// and persist so two requests cannot interleave; applyLocked must not lock mu.
func handleNetConfig(w http.ResponseWriter, r *http.Request, m *netstack.RulesetManager, mu *sync.Mutex,
	applyLocked func(bool) error, status func() map[string]any) {
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	var req configRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	mu.Lock()
	cur := configValues{
		Cameras:          m.CameraMACs(),
		Region:           m.Region(),
		BlockStreamSetup: m.BlockStreamSetupEnabled(),
	}
	cur.Block, _ = m.Active()
	next, err := req.merge(cur)
	if err != nil {
		mu.Unlock()
		writeAPIError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	// Reconfigure only validates, so a bad MAC is a request error, not an
	// apply failure.
	if err := m.Reconfigure(next.Cameras, next.Region, next.BlockStreamSetup); err != nil {
		mu.Unlock()
		writeAPIError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if err := applyLocked(next.Block); err != nil {
		// The kernel still has the previous rules: put the manager back so
		// its reported configuration matches what is actually applied.
		_ = m.Reconfigure(cur.Cameras, cur.Region, cur.BlockStreamSetup)
		mu.Unlock()
		writeAPIError(w, http.StatusInternalServerError, "apply_failed", err.Error())
		return
	}
	mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(status())
}

// runDaemon keeps the saved setting applied: it restores it at boot (a missing
// or corrupt setting restores Yes), re-resolves the allowed host names every
// 10 minutes, and serves a small local API.
func runDaemon(o options) {
	// Restore before constructing the manager: the cameras, region and
	// stream-setup choice saved by an earlier run win over the flags.
	sm := netstack.NewStateManager(o.stateDir)
	block, state, err := sm.RestoreOnBoot()
	if err != nil {
		fmt.Printf("[-] %v\n", err)
	}
	cfg := managerConfig(o)
	if len(state.Cameras) > 0 {
		cfg.Cameras = state.Cameras
	}
	cfg.Region = parseRegion(regionString(state, o.region))
	// state.json from a version that persists the runtime configuration wins
	// over the flags; an older file (no config_saved) keeps the flag.
	if err == nil && state.ConfigSaved {
		cfg.BlockStreamSetup = state.BlockStreamSetup
	}
	m, merr := netstack.NewRulesetManager(cfg)
	if merr != nil {
		fmt.Fprintf(os.Stderr, "[!] %v\n", merr)
		os.Exit(1)
	}
	fmt.Printf("[+] BombeCam camera firewall daemon. Saved setting: Block cloud video = %s (%s)\n", yesNo(block), state.Status)
	if state.Notes != "" {
		fmt.Printf("    %s\n", state.Notes)
	}

	var mu sync.Mutex

	// persistLocked writes the setting, ruleset and the manager's current
	// configuration. Caller holds mu.
	persistLocked := func(b bool, status string) error {
		return sm.Persist(netstack.StateOptions{
			Block:            b,
			Region:           m.Region(),
			Cameras:          m.CameraMACs(),
			BlockStreamSetup: m.BlockStreamSetupEnabled(),
		}, m.LastRendered, status)
	}

	// applyLocked applies the setting and persists it. Caller holds mu.
	applyLocked := func(b bool) error {
		if b {
			resolveNow(m)
		}
		if err := m.Apply(b); err != nil {
			return err
		}
		printWarnings(m)
		status := netstack.StatusRulesLoaded
		if !b {
			status = netstack.StatusRemoved
		}
		return persistLocked(b, status)
	}

	apply := func(b bool) error {
		mu.Lock()
		defer mu.Unlock()
		return applyLocked(b)
	}

	if err := apply(block); err != nil {
		fmt.Fprintf(os.Stderr, "[!] FATAL: could not apply Block cloud video = %s: %v\n", yesNo(block), err)
		os.Exit(1)
	}
	fmt.Printf("[+] Applied. Ruleset hash %s\n", m.GetLastHash())

	go func() {
		for range time.Tick(10 * time.Minute) {
			mu.Lock()
			on, _ := m.Active()
			if on {
				ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
				changed, rerr := m.Resolve(ctx)
				cancel()
				if rerr != nil {
					fmt.Printf("[-] %v\n", rerr)
				}
				if changed {
					if err := m.Apply(true); err != nil {
						fmt.Printf("[-] re-apply after address change failed: %v\n", err)
					} else {
						_ = persistLocked(true, netstack.StatusRulesLoaded)
						fmt.Println("[+] allowlisted addresses changed; rules re-applied")
					}
				}
			}
			mu.Unlock()
		}
	}()

	checkAuth := func(w http.ResponseWriter, r *http.Request) bool {
		if o.apiKey == "" {
			return true
		}
		h := r.Header.Get("Authorization")
		if !strings.HasPrefix(h, "Bearer ") || strings.TrimSpace(strings.TrimPrefix(h, "Bearer ")) != o.apiKey {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": "unauthorized"})
			return false
		}
		return true
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/net/status", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(statusPayload(m))
	})
	mux.HandleFunc("/api/v1/net/config", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if !checkAuth(w, r) {
			return
		}
		handleNetConfig(w, r, m, &mu, applyLocked, func() map[string]any { return statusPayload(m) })
	})
	mux.HandleFunc("/api/v1/net/block-cloud-video", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if !checkAuth(w, r) {
			return
		}
		var req struct {
			BlockCloudVideo *bool `json:"block_cloud_video"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.BlockCloudVideo == nil {
			http.Error(w, `body must be {"block_cloud_video": true|false}`, http.StatusBadRequest)
			return
		}
		if err := apply(*req.BlockCloudVideo); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "applied", "block_cloud_video": *req.BlockCloudVideo})
	})
	mux.HandleFunc("/api/v1/net/verdict", func(w http.ResponseWriter, r *http.Request) {
		data, err := os.ReadFile(filepath.Join(o.stateDir, "verdict.json"))
		w.Header().Set("Content-Type", "application/json")
		if err != nil {
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": "verdict_not_found"})
			return
		}
		_, _ = w.Write(data)
	})

	apiAddr := net.JoinHostPort(o.apiHost, strconv.Itoa(o.apiPort))
	srv := &http.Server{Addr: apiAddr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		fmt.Printf("[+] Local API on http://%s\n", apiAddr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			fmt.Printf("[-] API server error: %v\n", err)
		}
	}()
	defer srv.Close()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	// Rules stay in the kernel on shutdown: stopping the daemon must not
	// silently unblock the cameras. Use `block-cloud-video no` for that.
	fmt.Println("\n[*] Shutting down (rules stay in place).")
}

func runNTP(bind string) {
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	srv := netstack.NewNTPServer(bind)
	if err := srv.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "[!] NTP responder failed to start on %s: %v\n", bind, err)
		os.Exit(1)
	}
	fmt.Printf("[+] NTP responder listening on %s\n", bind)
	<-stop
	_ = srv.Stop()
}

func runNAT(cameraIf, cameraSubnet, wanIf, action string) {
	natMgr := netstack.NewNATManager(cameraIf, cameraSubnet, wanIf)
	_, _ = natMgr.DetectWANInterface()
	switch action {
	case "apply":
		if err := natMgr.ApplyNAT(); err != nil {
			fmt.Fprintf(os.Stderr, "Error applying NAT: %v\n", err)
			os.Exit(1)
		}
		fmt.Println("[+] WAN NAT masquerade enabled")
	case "remove":
		if err := natMgr.RemoveNAT(); err != nil {
			fmt.Fprintf(os.Stderr, "Error removing NAT: %v\n", err)
			os.Exit(1)
		}
		fmt.Println("[+] WAN NAT masquerade removed")
	case "status":
		data, _ := json.MarshalIndent(natMgr.GetStatus(), "", "  ")
		fmt.Println(string(data))
	case "render":
		fmt.Print(natMgr.RenderNFTNATRules())
	default:
		fmt.Fprintf(os.Stderr, "Unknown NAT action: %q\n", action)
		os.Exit(1)
	}
}
