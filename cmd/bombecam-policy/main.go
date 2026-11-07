// bombecam-policy compiles the "Block cloud video" policy for a set of
// cameras and prints it in one of several forms.
//
//	bombecam-policy -block yes -camera "Front Door=AA:BB:CC:DD:EE:FF" -target json
//	bombecam-policy -block yes -camera "Front Door=AA:BB:CC:DD:EE:FF" -target nftables -resolve
//	bombecam-policy -target openwrt-script -o bombecam-router.sh
//	bombecam-policy -block yes -camera "Front Door=AA:BB:CC:DD:EE:FF" -target openwrt-command
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"strings"

	"github.com/Fever-r/BombeCam/pkg/netstack"
	"github.com/Fever-r/BombeCam/pkg/policy"
	"github.com/Fever-r/BombeCam/pkg/renderer/nftables"
	"github.com/Fever-r/BombeCam/pkg/renderer/openwrt"
)

type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

func getEnvOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// parseYesNo accepts yes/no/true/false/on/off/1/0.
func parseYesNo(s string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "yes", "y", "true", "on", "1":
		return true, nil
	case "no", "n", "false", "off", "0":
		return false, nil
	}
	return false, fmt.Errorf("expected yes or no, got %q", s)
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("bombecam-policy", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var cameras multiFlag
	fs.Var(&cameras, "camera", `camera as "Name=MAC" (repeatable); CAMERA_MAC may list several, comma-separated`)
	block := fs.String("block", "yes", "Block cloud video? yes or no")
	target := fs.String("target", "json", "json | nftables | openwrt-script | openwrt-command")
	blockSetup := fs.Bool("block-stream-setup", false, "also block Osaio's stream-setup server (may stop BombeCam starting local streams)")
	regionFlag := fs.String("region", getEnvOrDefault("BOMBECAM_REGION", string(policy.DefaultRegion)), `Osaio region: "us" or "eu"`)
	resolve := fs.Bool("resolve", false, "nftables: resolve the allowlisted host names now (otherwise their sets are empty)")
	outputFile := fs.String("o", "", "write to this file instead of stdout")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	out := stdout
	if *outputFile != "" {
		f, err := os.Create(*outputFile)
		if err != nil {
			fmt.Fprintf(stderr, "Error: %v\n", err)
			return 1
		}
		defer f.Close()
		out = f
	}

	if *target == "openwrt-script" {
		_, _ = out.Write(openwrt.Script())
		return 0
	}

	on, err := parseYesNo(*block)
	if err != nil {
		fmt.Fprintf(stderr, "Error: -block: %v\n", err)
		return 2
	}
	if len(cameras) == 0 {
		if env := getEnvOrDefault("CAMERA_MAC", ""); env != "" {
			cameras = strings.Split(env, ",")
		}
	}
	subs, err := netstack.ParseCameras(cameras)
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return 2
	}
	region, err := policy.ParseRegion(*regionFlag)
	if err != nil {
		fmt.Fprintf(stderr, "Error: -region: %v\n", err)
		return 2
	}
	opts := policy.Options{BlockStreamSetup: *blockSetup}
	doc, err := policy.Compile(subs, policy.Setting{BlockCloudVideo: on, Region: region}, opts)
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return 1
	}

	switch strings.ToLower(*target) {
	case "json":
		b, _ := json.MarshalIndent(doc, "", "  ")
		fmt.Fprintln(out, string(b))
	case "nftables", "nft":
		r, _ := nftables.NewRenderer(nftables.DefaultConfig())
		resolved := map[string][]net.IP{}
		if *resolve {
			for _, h := range doc.Hosts() {
				ips, rerr := netstack.DefaultResolver(context.Background(), h)
				if rerr != nil {
					fmt.Fprintf(stderr, "[!] %s: %v\n", h, rerr)
				}
				resolved[h] = ips
			}
		}
		compiled, cerr := r.Compile(doc, resolved)
		if cerr != nil {
			fmt.Fprintf(stderr, "Compilation failed: %v\n", cerr)
			return 1
		}
		for _, w := range r.Warnings() {
			fmt.Fprintf(stderr, "[warning] %s\n", w)
		}
		fmt.Fprint(out, compiled)
	case "openwrt-command":
		a, aerr := openwrt.ApplyArgs(doc, opts)
		if aerr != nil {
			fmt.Fprintf(stderr, "Error: %v\n", aerr)
			return 1
		}
		fmt.Fprintln(out, openwrt.ManualCommand(a))
	default:
		fmt.Fprintf(stderr, "Error: unknown target %q (json, nftables, openwrt-script, openwrt-command)\n", *target)
		return 2
	}
	return 0
}
