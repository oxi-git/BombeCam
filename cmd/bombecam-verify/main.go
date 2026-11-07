// bombecam-verify answers one question for a Linux router running
// bombecam-net: is this router actually on the camera's path, and does
// "Block cloud video" actually stop everything but the capped allowlist?
//
// It answers by measuring. Every input to the verdict comes from the system:
// the neighbour table, the interface addresses, the NAT table, real sockets
// probed from the camera segment, nftables counters, and a raw-socket capture
// on the camera link. Nothing is assumed, and any input that cannot be read
// downgrades the verdict rather than being skipped.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/Fever-r/BombeCam/pkg/netstack"
	"github.com/Fever-r/BombeCam/pkg/policy"
	"github.com/Fever-r/BombeCam/pkg/prober"
	"github.com/Fever-r/BombeCam/pkg/verifier"
)

func main() {
	// Re-exec hook: the prober runs each probe inside the canary's network
	// namespace by re-executing this binary there. Handled before flag parsing
	// so it stays out of the CLI surface.
	if len(os.Args) == 3 && os.Args[1] == "-probe-once" {
		if err := prober.ProbeOnce(os.Args[2]); err != nil {
			os.Exit(1)
		}
		os.Exit(0)
	}

	regionFlag := flag.String("region", getEnvOrDefault("BOMBECAM_REGION", string(policy.DefaultRegion)), `Osaio region: "us" or "eu"`)
	cameraID := flag.String("camera", getEnvOrDefault("CAMERA_ID", "default_camera"), "camera identifier")
	cameraMAC := flag.String("mac", getEnvOrDefault("CAMERA_MAC", ""), "camera MAC address (required)")
	cameraIP := flag.String("ip", getEnvOrDefault("CAMERA_IP", ""), "camera IP address (required)")
	cameraIf := flag.String("iface", getEnvOrDefault("CAMERA_VLAN_IF", ""), "camera interface")
	cameraSubnet := flag.String("subnet", getEnvOrDefault("CAMERA_SUBNET", ""), "camera subnet")
	gatewayIP := flag.String("gateway", getEnvOrDefault("CAMERA_GATEWAY_IP", ""), "appliance address on the camera segment")
	canaryIP := flag.String("canary", getEnvOrDefault("CANARY_IP", ""), "address on the camera segment to probe from")
	canaryMAC := flag.String("canary-mac", getEnvOrDefault("CANARY_MAC", ""), "canary MAC, enrolled as a subject so the policy treats it exactly like the camera")
	canaryNetns := flag.String("canary-netns", getEnvOrDefault("CANARY_NETNS", "canary"), "network namespace the canary lives in (required: probes must originate on the camera segment)")
	wanIf := flag.String("wan", getEnvOrDefault("WAN_INTERFACE", ""), "WAN interface")
	checkRouter := flag.Bool("consumer-router", false, "verify a third-party router device-block by canary alone")
	verdictPath := flag.String("verdict-file", verifier.DefaultVerdictPath, "where to persist the verdict")
	serveMetrics := flag.Int("metrics-port", 0, "serve Prometheus metrics on this port (0 = off)")
	probeMatrix := flag.String("probe-matrix", "", "JSON probe matrix (default: generated from the compiled policy)")
	watch := flag.Duration("watch", 0, "re-verify on this interval instead of exiting (0 = run once)")
	timeout := flag.Duration("timeout", 90*time.Second, "budget for one verification run")
	flag.Parse()

	// Dynamic network discovery fallback for camera interface, subnet, gateway, WAN
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

	if *cameraMAC == "" || *cameraIP == "" {
		fmt.Fprintln(os.Stderr, "bombecam-verify: -mac and -ip are required.")
		fmt.Fprintln(os.Stderr, "  A camera is identified by MAC + interface. There is no default, because a")
		fmt.Fprintln(os.Stderr, "  verdict about the wrong camera is worse than no verdict.")
		os.Exit(2)
	}

	if *cameraIf == "" || *cameraSubnet == "" || *gatewayIP == "" {
		fmt.Fprintln(os.Stderr, "bombecam-verify: -iface, -subnet, and -gateway are required (or set via CAMERA_VLAN_IF, CAMERA_SUBNET, CAMERA_GATEWAY_IP or host discovery).")
		os.Exit(2)
	}

	if *canaryIP == "" {
		fmt.Fprintln(os.Stderr, "bombecam-verify: -canary is required (or set via CANARY_IP).")
		os.Exit(2)
	}
	if !*checkRouter {
		if *canaryMAC == "" {
			fmt.Fprintln(os.Stderr, "bombecam-verify: -canary-mac is required for a bounded canary-only control leg.")
			os.Exit(2)
		}
		canaryHW, err := net.ParseMAC(*canaryMAC)
		if err != nil || len(canaryHW) != 6 || canaryHW[0]&1 != 0 {
			fmt.Fprintln(os.Stderr, "bombecam-verify: invalid canary MAC.")
			os.Exit(2)
		}
		for _, mac := range strings.Split(*cameraMAC, ",") {
			cameraHW, _ := net.ParseMAC(strings.TrimSpace(mac))
			if bytes.Equal(cameraHW, canaryHW) {
				fmt.Fprintln(os.Stderr, "bombecam-verify: the canary MAC must differ from every real camera MAC.")
				os.Exit(2)
			}
		}
	}

	region, rerr := policy.ParseRegion(*regionFlag)
	if rerr != nil {
		fmt.Fprintf(os.Stderr, "bombecam-verify: -region: %v\n", rerr)
		os.Exit(2)
	}
	rules, err := netstack.NewRulesetManager(netstack.RulesetConfig{
		Cameras:      subjectMACs(*cameraMAC, *canaryMAC),
		Region:       region,
		EnableMasq:   *wanIf != "",
		WANInterface: *wanIf,
		CameraSubnet: *cameraSubnet,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "bombecam-verify: cannot build the policy: %v\n", err)
		os.Exit(2)
	}
	if _, rerr := rules.Resolve(context.Background()); rerr != nil {
		fmt.Fprintf(os.Stderr, "[!] %v (allowlisted names without an address stay blocked)\n", rerr)
	}

	pcfg := prober.DefaultConfig()
	pcfg.CameraInterface = *cameraIf
	pcfg.CameraSubnet = *cameraSubnet
	pcfg.CanaryIP = *canaryIP
	pcfg.CanaryNetns = *canaryNetns
	pcfg.ApplyTemporaryControl = func(ctx context.Context, ttl time.Duration) error {
		return rules.ApplyTemporaryControl(ctx, *canaryMAC, *cameraIf, ttl)
	}
	p, err := prober.New(pcfg, func(ctx context.Context, block bool) error {
		return rules.ApplyContext(ctx, block)
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "bombecam-verify: %v\n", err)
		os.Exit(2)
	}

	cfg := verifier.DefaultConfig()
	cfg.Region = region
	cfg.CameraID = *cameraID
	cfg.CameraMAC = *cameraMAC
	cfg.CameraIP = *cameraIP
	cfg.GatewayIP = *gatewayIP
	if *checkRouter {
		cfg.EnforcementPoint = "consumer_router"
	}
	v := verifier.NewVerifier(cfg, p, verifier.DefaultMetrics)

	// The matrix is generated from the compiled policy, so the predictions the
	// differential is checked against cannot drift from the rules in force.
	if *probeMatrix != "" {
		m, merr := verifier.LoadProbeMatrix(*probeMatrix)
		if merr != nil {
			fmt.Fprintf(os.Stderr, "bombecam-verify: cannot load probe matrix: %v\n", merr)
			os.Exit(2)
		}
		if err := v.SetProbeMatrix(m); err != nil {
			fmt.Fprintf(os.Stderr, "bombecam-verify: %v\n", err)
			os.Exit(2)
		}
	} else if doc, derr := rules.Document(true); derr == nil {
		m, merr := verifier.MatrixFromPolicy(doc, verifier.DefaultProbeTargets())
		if merr != nil {
			fmt.Fprintf(os.Stderr, "bombecam-verify: cannot generate a probe matrix from the policy: %v\n", merr)
			os.Exit(2)
		}
		if err := v.SetProbeMatrix(m); err != nil {
			fmt.Fprintf(os.Stderr, "bombecam-verify: %v\n", err)
			os.Exit(2)
		}
	}

	runOnce := func() *verifier.Verdict {
		ctx, cancel := context.WithTimeout(context.Background(), *timeout)
		defer cancel()

		var verdict *verifier.Verdict
		var rerr error
		if *checkRouter {
			verdict, rerr = v.VerifyConsumerRouterBlock(ctx)
		} else {
			verdict, rerr = v.RunVerification(ctx)
		}
		if rerr != nil {
			// A run that could not be performed is not a verdict about the
			// firewall. Record that, do not invent one.
			if verdict == nil {
				verdict = &verifier.Verdict{
					CameraID: *cameraID, EnforcementPoint: cfg.EnforcementPoint,
					Status: verifier.StatusUnknown, BlockCloudVideo: false,
					Details:    verifier.VerdictDetails{FailureReason: rerr.Error()},
					VerifiedAt: time.Now().UTC(), ExpiresAt: time.Now().UTC(),
				}
			}
			fmt.Fprintf(os.Stderr, "[!] verification could not be performed: %v\n", rerr)
		}
		if err := verifier.SaveVerdict(*verdictPath, verdict); err != nil {
			fmt.Fprintf(os.Stderr, "[!] could not persist the verdict to %s: %v\n", *verdictPath, err)
		}
		return verdict
	}

	verdict := runOnce()

	if *serveMetrics > 0 {
		http.HandleFunc("/metrics", func(w http.ResponseWriter, _ *http.Request) {
			cur := verifier.LoadVerdict(*verdictPath)
			fmt.Fprint(w, verifier.DefaultMetrics.Render(cur))
		})
		go func() {
			_ = http.ListenAndServe(fmt.Sprintf(":%d", *serveMetrics), nil)
		}()
	}

	if *watch > 0 {
		fmt.Printf("[*] re-verifying every %s; verdicts persist to %s\n", *watch, *verdictPath)
		for range time.Tick(*watch) {
			verdict = runOnce()
			printVerdict(verdict, false)
		}
	}

	printVerdict(verdict, true)
	switch verdict.EffectiveStatus() {
	case verifier.StatusEnforcedProven, verifier.StatusEnforcedObserved:
		os.Exit(0)
	default:
		os.Exit(1)
	}
}

// subjectMACs returns every device the policy must treat as a subject. The
// canary is included deliberately: the differential only carries information
// if the canary is subject to exactly the same rules as the camera.
func subjectMACs(cameraMAC, canaryMAC string) []string {
	out := []string{}
	for _, m := range append(strings.Split(cameraMAC, ","), strings.Split(canaryMAC, ",")...) {
		if m = strings.TrimSpace(m); m != "" {
			out = append(out, m)
		}
	}
	return out
}

func printVerdict(verdict *verifier.Verdict, withJSON bool) {
	if withJSON {
		if b, err := json.MarshalIndent(verdict, "", "  "); err == nil {
			fmt.Println(string(b))
		}
	}
	fmt.Printf("\n[VERDICT]: %s\n", verdict.EffectiveStatus())
	fmt.Printf("[MEANING]: %s\n", verdict.EffectiveStatus().HumanReadable())
	if r := verdict.Details.FailureReason; r != "" {
		fmt.Printf("[BECAUSE]: %s\n", r)
	}
}

func getEnvOrDefault(key, fallback string) string {
	if val := os.Getenv(key); val != "" {
		return val
	}
	return fallback
}
