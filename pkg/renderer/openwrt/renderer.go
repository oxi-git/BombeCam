// Package openwrt ships BombeCam's router script for OpenWrt and GL.iNet
// routers and builds the exact command that applies a policy with it.
//
// The rules themselves are generated on the router by the script, because the
// two allowed Osaio host names are re-resolved there every 10 minutes (the
// router, not the PC, is what the camera's traffic passes through). The script
// supports both OpenWrt firewall generations, detected at run time:
//
//   - fw4 (nftables): OpenWrt 22.03+; e.g. GL.iNet 4.x builds on OpenWrt
//     22.03/23.05/24.10 (AR300M, MT300N-V2, E750, AXT1800, AX1800, BE3600,
//     the "op24" builds of MT3000/MT6000).
//   - fw3 (iptables): OpenWrt 21.02 and older; e.g. GL.iNet 4.x builds on the
//     "MTK 21.02" stream (MT3000, MT6000, X3000), A1300, B1300.
//
// For fw4 the nft table the script emits is byte-for-byte the table the Go
// nftables renderer emits for the same inputs (a test enforces that).
package openwrt

import (
	_ "embed"
	"fmt"
	"strings"

	"github.com/Fever-r/BombeCam/pkg/policy"
)

//go:embed bombecam-router.sh
var script []byte

// ScriptName is the file name used on the router and in downloads.
const ScriptName = "bombecam-router.sh"

// RemotePath is where the gateway uploads the script before running it.
const RemotePath = "/tmp/bombecam-router.sh"

// Script returns the router script (LF line endings, POSIX sh / busybox ash).
func Script() []byte {
	out := make([]byte, len(script))
	copy(out, script)
	return out
}

// Camera is one camera to protect on the router.
type Camera struct {
	Name string `json:"name"`
	MAC  string `json:"mac"`
}

// CamerasFromPolicy lists the document's subjects as router cameras.
func CamerasFromPolicy(doc *policy.Document) []Camera {
	out := make([]Camera, 0, len(doc.Subjects))
	for _, s := range doc.Subjects {
		name := s.Name
		if name == "" {
			name = s.ID
		}
		out = append(out, Camera{Name: policy.SanitizeName(name), MAC: policy.CanonicalMAC(s.MAC)})
	}
	return out
}

// ApplyArgs returns the script arguments that put doc in force on the router.
// Camera names are sanitised and MACs canonicalised, so every argument is
// made of shell- and nft-safe characters.
func ApplyArgs(doc *policy.Document, opts policy.Options) ([]string, error) {
	if err := doc.Validate(); err != nil {
		return nil, err
	}
	if !doc.BlockCloudVideo {
		return []string{"apply", "no"}, nil
	}
	args := []string{"apply", "yes"}
	for _, c := range CamerasFromPolicy(doc) {
		if c.MAC == "" {
			return nil, fmt.Errorf("camera %q has no valid MAC", c.Name)
		}
		args = append(args, "--camera", c.Name+"="+c.MAC)
	}
	if opts.BlockStreamSetup {
		args = append(args, "--block-stream-setup")
	} else {
		args = append(args, "--allow-stream-setup")
	}
	// Only send the region when it is not the default: a router still running
	// an older script rejects unknown options, and older scripts only know the
	// US allowlist.
	if region := doc.Region.OrDefault(); region != policy.RegionUS {
		args = append(args, "--region", string(region))
	}
	return args, nil
}

// Gate verbs: the only commands BombeCam's router key can run (the key's
// forced command is "bombecam-router gate", which reads the verb from
// SSH_ORIGINAL_COMMAND).
const (
	GateApply       = "apply" // stdin: ApplySpec
	GateStatus      = "status"
	GateConnections = "connections"
	GateVersion     = "version"
	GateUninstall   = "uninstall"
)

// ApplySpec is the stdin for the gate's "apply": the complete list of cameras
// to block (none means block no camera) and the stream-setup option. Names are
// sanitised and MACs canonicalised; cameras without a valid MAC are an error.
// The region line is emitted only when it is not the default, so routers still
// running an older script keep working.
func ApplySpec(cams []Camera, opts policy.Options, region policy.Region) ([]byte, error) {
	var b strings.Builder
	for _, c := range cams {
		mac := policy.CanonicalMAC(c.MAC)
		if mac == "" {
			return nil, fmt.Errorf("camera %q has no valid MAC", c.Name)
		}
		fmt.Fprintf(&b, "camera %s %s\n", mac, policy.SanitizeName(c.Name))
	}
	if opts.BlockStreamSetup {
		b.WriteString("block_stream_setup 1\n")
	} else {
		b.WriteString("block_stream_setup 0\n")
	}
	if r := region.OrDefault(); r != policy.RegionUS {
		fmt.Fprintf(&b, "region %s\n", r)
	}
	return []byte(b.String()), nil
}

// ConnectArgs are the script arguments that install BombeCam's key(s).
func ConnectArgs(authorizedKeys []string) []string {
	args := []string{"connect"}
	for _, k := range authorizedKeys {
		args = append(args, "--key", k)
	}
	return args
}

// ScriptVersion is the VERSION of the embedded script.
func ScriptVersion() string {
	for _, line := range strings.Split(string(script), "\n") {
		if v, ok := strings.CutPrefix(line, "VERSION="); ok {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// RemoteCommand is the single shell command the gateway runs over SSH. The
// script arrives on stdin, is saved to RemotePath, then executed with args.
func RemoteCommand(args []string) string {
	quoted := make([]string, len(args))
	for i, a := range args {
		quoted[i] = ShellQuote(a)
	}
	return fmt.Sprintf("umask 077 && cat > %s && sh %s %s", RemotePath, RemotePath, strings.Join(quoted, " "))
}

// ManualCommand is the command a user types in the router's SSH session after
// copying the script to /tmp (see the apply guide).
func ManualCommand(args []string) string {
	quoted := make([]string, len(args))
	for i, a := range args {
		quoted[i] = ShellQuote(a)
	}
	return "sh " + RemotePath + " " + strings.Join(quoted, " ")
}

// ShellQuote single-quotes s for POSIX sh.
func ShellQuote(s string) string {
	if s != "" && strings.Trim(s, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_.:/=") == "" {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// Result is the parsed BOMBECAM_RESULT line the script prints last.
type Result struct {
	Status   string            `json:"status"`
	Block    string            `json:"block"`
	Firewall string            `json:"firewall"`
	Fields   map[string]string `json:"fields"`
}

// ParseResult extracts the last BOMBECAM_RESULT line from script output.
func ParseResult(output string) (Result, bool) {
	var res Result
	found := false
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "BOMBECAM_RESULT ") {
			continue
		}
		found = true
		res = Result{Fields: map[string]string{}}
		for _, kv := range strings.Fields(strings.TrimPrefix(line, "BOMBECAM_RESULT ")) {
			k, v, ok := strings.Cut(kv, "=")
			if !ok {
				continue
			}
			res.Fields[k] = v
		}
		res.Status = res.Fields["status"]
		res.Block = res.Fields["block"]
		res.Firewall = res.Fields["firewall"]
	}
	return res, found
}
