package openwrt

import (
	"bytes"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Fever-r/BombeCam/pkg/policy"
	"github.com/Fever-r/BombeCam/pkg/renderer/nftables"
)

var cams = []policy.Subject{
	{ID: "c1", Name: "Test Camera", MAC: "AA:BB:CC:DD:EE:01"},
	{ID: "c2", Name: "Porch", MAC: "aa:bb:cc:dd:ee:02"},
}

func TestScriptPolicyConstantsMatchGo(t *testing.T) {
	s := string(Script())
	us, err := policy.RegionEndpoints(policy.RegionUS)
	if err != nil {
		t.Fatal(err)
	}
	eu, err := policy.RegionEndpoints(policy.RegionEU)
	if err != nil {
		t.Fatal(err)
	}
	if us.ControlPort != eu.ControlPort || us.StreamSetupPort != eu.StreamSetupPort {
		t.Fatalf("the script has one port pair; regions disagree: %+v vs %+v", us, eu)
	}
	want := map[string]string{
		"REGION":           string(policy.DefaultRegion),
		"CONTROL_PORT":     fmt.Sprint(us.ControlPort),
		"SETUP_PORT":       fmt.Sprint(us.StreamSetupPort),
		"US_CONTROL_HOSTS": `"` + strings.Join(us.ControlHosts, " ") + `"`,
		"US_SETUP_HOST":    us.StreamSetupHost,
		"EU_CONTROL_HOSTS": `"` + strings.Join(eu.ControlHosts, " ") + `"`,
		"EU_SETUP_HOST":    eu.StreamSetupHost,
		"CAP_BPS":          fmt.Sprint(policy.CapBytesPerSecond),
		"CAP_BURST":        fmt.Sprint(policy.CapBurstBytes),
		"LOCAL_NETS":       `"` + strings.Join(policy.DefaultLocalCIDRs, " ") + `"`,
		"VERSION":          policy.Version,
	}
	for k, v := range want {
		re := regexp.MustCompile(`(?m)^` + k + `=(.*)$`)
		m := re.FindStringSubmatch(s)
		if m == nil || m[1] != v {
			t.Errorf("script %s = %v, Go policy says %s", k, m, v)
		}
	}
	if !strings.Contains(s, policy.Headline) {
		t.Error("script success message must use the exact headline wording")
	}
	if bytes.Contains(script, []byte("\r")) {
		t.Error("script must use LF line endings (busybox sh fails on CRLF)")
	}
	for _, bad := range []string{"DNAT", "SNAT", "REDIRECT", "dnat to", "redirect to", "-j MARK", "TPROXY"} {
		if strings.Contains(s, bad) {
			t.Errorf("script must only allow/deny/rate-limit, found %q", bad)
		}
	}
}

func TestApplyArgs(t *testing.T) {
	doc, _ := policy.Compile(cams, policy.Setting{BlockCloudVideo: true}, policy.Options{})
	args, err := ApplyArgs(doc, policy.Options{})
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(args, "|")
	want := "apply|yes|--camera|Test Camera=aa:bb:cc:dd:ee:01|--camera|Porch=aa:bb:cc:dd:ee:02|--allow-stream-setup"
	if got != want {
		t.Fatalf("args = %s\nwant   %s", got, want)
	}
	cmd := RemoteCommand(args)
	if !strings.Contains(cmd, "'Test Camera=aa:bb:cc:dd:ee:01'") || !strings.HasPrefix(cmd, "umask 077 && cat > /tmp/bombecam-router.sh && sh /tmp/bombecam-router.sh apply yes") {
		t.Fatalf("remote command = %s", cmd)
	}

	off, _ := policy.Compile(cams, policy.Setting{BlockCloudVideo: false}, policy.Options{})
	args, _ = ApplyArgs(off, policy.Options{})
	if strings.Join(args, " ") != "apply no" {
		t.Fatalf("No args = %v", args)
	}
}

func TestApplyArgsEU(t *testing.T) {
	doc, err := policy.Compile(cams, policy.Setting{BlockCloudVideo: true, Region: policy.RegionEU}, policy.Options{})
	if err != nil {
		t.Fatal(err)
	}
	args, err := ApplyArgs(doc, policy.Options{})
	if err != nil {
		t.Fatal(err)
	}
	want := "apply|yes|--camera|Test Camera=aa:bb:cc:dd:ee:01|--camera|Porch=aa:bb:cc:dd:ee:02|--allow-stream-setup|--region|eu"
	if got := strings.Join(args, "|"); got != want {
		t.Fatalf("args = %s\nwant   %s", got, want)
	}
	// The gate spec carries the region for EU only; old scripts reject the
	// line, so the default (US) must stay silent.
	spec, err := ApplySpec(CamerasFromPolicy(doc), policy.Options{}, policy.RegionEU)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(spec), "region eu\n") {
		t.Fatalf("EU gate spec has no region line:\n%s", spec)
	}
	us, _ := policy.Compile(cams, policy.Setting{BlockCloudVideo: true}, policy.Options{})
	usSpec, err := ApplySpec(CamerasFromPolicy(us), policy.Options{}, policy.RegionUS)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(usSpec), "region ") {
		t.Fatalf("US gate spec must not carry a region line:\n%s", usSpec)
	}
}

func TestShellQuote(t *testing.T) {
	cases := map[string]string{
		"yes":           "yes",
		"a b":           "'a b'",
		"it's":          `'it'\''s'`,
		"$(reboot)":     "'$(reboot)'",
		"":              "''",
		"x=aa:bb:cc:dd": "x=aa:bb:cc:dd",
	}
	for in, want := range cases {
		if got := ShellQuote(in); got != want {
			t.Errorf("ShellQuote(%q) = %s, want %s", in, got, want)
		}
	}
}

func TestParseResult(t *testing.T) {
	out := "noise\nBOMBECAM_RESULT status=ok block=yes firewall=fw4 cameras=1 cap=bytes\n"
	r, ok := ParseResult(out)
	if !ok || r.Status != "ok" || r.Block != "yes" || r.Firewall != "fw4" || r.Fields["cameras"] != "1" {
		t.Fatalf("got %+v %v", r, ok)
	}
	if _, ok := ParseResult("nothing"); ok {
		t.Fatal("found a result in output without one")
	}
}

// shellEnv prepares a fake router root with a cached address list so the
// script can render without touching the network.
func shellEnv(t *testing.T, fw string) (sh string, env []string, root string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("needs a POSIX shell")
	}
	sh = "busybox"
	if _, err := exec.LookPath("busybox"); err != nil {
		sh = "sh"
		if _, err := exec.LookPath("sh"); err != nil {
			t.Skip("no shell")
		}
	}
	if s := os.Getenv("BC_TEST_SHELL"); s != "" { // e.g. dash or bash, to check portability
		sh = s
	}
	root = t.TempDir()
	must(t, os.MkdirAll(filepath.Join(root, "etc/bombecam"), 0o755))
	must(t, os.WriteFile(filepath.Join(root, "etc/bombecam/ips"), []byte(
		"mqtts02-us.osaio.net 3.1.2.3 20000\nmqtts02-us.osaio.net 3.1.2.4 20000\nwss-us.osaio.net 52.9.9.9 20000\n"), 0o644))
	scriptPath := filepath.Join(root, ScriptName)
	must(t, os.WriteFile(scriptPath, Script(), 0o755))
	env = append(os.Environ(), "BC_ROOT="+root, "BC_DRYRUN=1", "BC_FW="+fw, "BC_CAP_MODE=bytes", "BC_IPT_LOG=1")
	if sh == "busybox" {
		// Use busybox's awk, sed, grep... too, as on OpenWrt.
		bb, _ := exec.LookPath("busybox")
		bbin := filepath.Join(root, "busybox-bin")
		must(t, os.MkdirAll(bbin, 0o755))
		for _, applet := range strings.Fields("awk sed grep cut sort tr wc cat cp mv rm mkdir mktemp head tail " +
			"uniq date id chmod dirname basename touch tee ln readlink timeout nslookup") {
			must(t, os.Symlink(bb, filepath.Join(bbin, applet)))
		}
		env = prependPath(env, bbin)
	}
	return sh, env, root
}

// shellForFakeApplets returns the shell for a test that puts a fake of a
// busybox applet (such as ip) on PATH. Debian's busybox-static, the busybox on
// Ubuntu servers and GitHub's runners, runs its own applets before anything on
// PATH and would ignore the fake; the system sh, with busybox's tools still
// first on PATH, runs it.
func shellForFakeApplets(t *testing.T, sh string) string {
	t.Helper()
	if sh != "busybox" {
		return sh
	}
	dir := t.TempDir()
	fake := filepath.Join(dir, "ip")
	must(t, os.WriteFile(fake, []byte("#!/bin/sh\n"), 0o755))
	cmd := exec.Command("busybox", "sh", "-c", "command -v ip")
	cmd.Env = prependPath(os.Environ(), dir)
	if out, err := cmd.Output(); err == nil && strings.TrimSpace(string(out)) == fake {
		return sh
	}
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("busybox sh ignores PATH for its applets and there is no other shell")
	}
	return "sh"
}

func prependPath(env []string, dir string) []string {
	out := make([]string, 0, len(env)+1)
	path := os.Getenv("PATH")
	for _, e := range env {
		if strings.HasPrefix(e, "PATH=") {
			path = strings.TrimPrefix(e, "PATH=")
			continue
		}
		out = append(out, e)
	}
	return append(out, "PATH="+dir+string(os.PathListSeparator)+path)
}

func runScript(t *testing.T, sh string, env []string, root string, args ...string) (string, error) {
	t.Helper()
	var argv []string
	if sh == "busybox" {
		argv = append(argv, "sh")
	}
	argv = append(argv, filepath.Join(root, ScriptName))
	argv = append(argv, args...)
	cmd := exec.Command(sh, argv...)
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func normalizeNft(s string) string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		l := strings.TrimSpace(line)
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		out = append(out, l)
	}
	return strings.Join(out, "\n")
}

// The script's fw4 output and the Go renderer must be the same ruleset.
func TestScriptFw4MatchesGoRenderer(t *testing.T) {
	sh, env, root := shellEnv(t, "fw4")
	out, err := runScript(t, sh, env, root, "render", "yes",
		"--camera", "Test Camera=AA:BB:CC:DD:EE:01", "--camera", "Porch=aa-bb-cc-dd-ee-02")
	if err != nil {
		t.Fatalf("render failed: %v\n%s", err, out)
	}
	doc, _ := policy.Compile(cams, policy.Setting{BlockCloudVideo: true}, policy.Options{})
	r, _ := nftables.NewRenderer(nftables.DefaultConfig())
	goOut, err := r.Compile(doc, map[string][]net.IP{
		policy.ControlHost:     {net.ParseIP("3.1.2.4"), net.ParseIP("3.1.2.3")},
		policy.StreamSetupHost: {net.ParseIP("52.9.9.9")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if a, b := normalizeNft(out), normalizeNft(goOut); a != b {
		t.Fatalf("script and Go renderer disagree\n--- script\n%s\n--- go\n%s", a, b)
	}
}

func TestScriptRejectsBadCameras(t *testing.T) {
	sh, env, root := shellEnv(t, "fw4")
	for _, spec := range []string{
		"T=XX:XX:XX:XX:XX:XX", "T=01:00:5e:00:00:01", "T=00:00:00:00:00:00", "T=aa:bb:cc", "no-equals-sign",
	} {
		if out, err := runScript(t, sh, env, root, "render", "yes", "--camera", spec); err == nil {
			t.Errorf("%q accepted:\n%s", spec, out)
		}
	}
	out, err := runScript(t, sh, env, root, "render", "yes", "--camera", `Mom"s cam; rm -rf /=aa:bb:cc:dd:ee:09`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `comment "cap: Mom_s cam_ rm -rf _"`) {
		t.Fatalf("name not sanitised:\n%s", out)
	}
}

func TestScriptApplyAndRemoveDryRun(t *testing.T) {
	sh, env, root := shellEnv(t, "fw4")
	out, err := runScript(t, sh, env, root, "apply", "yes", "--camera", "Test Camera=AA:BB:CC:DD:EE:01")
	if err != nil {
		t.Fatalf("apply failed: %v\n%s", err, out)
	}
	res, ok := ParseResult(out)
	if !ok || res.Status != "ok" || res.Block != "yes" || res.Fields["cameras"] != "1" {
		t.Fatalf("bad result %+v\n%s", res, out)
	}
	// The router knows no address for the camera: nothing to clear.
	if res.Fields["cleared"] != "none" {
		t.Errorf("cleared = %q, want none\n%s", res.Fields["cleared"], out)
	}
	cron, _ := os.ReadFile(filepath.Join(root, "etc/crontabs/root"))
	if !strings.Contains(string(cron), "bombecam-router refresh") {
		t.Error("cron refresh not installed")
	}
	if _, err := os.Stat(filepath.Join(root, "etc/init.d/bombecam")); err != nil {
		t.Error("boot script not installed")
	}
	if _, err := os.Stat(filepath.Join(root, "usr/sbin/bombecam-router")); err != nil {
		t.Error("script not installed to /usr/sbin")
	}
	camsFile, _ := os.ReadFile(filepath.Join(root, "etc/bombecam/cameras"))
	if string(camsFile) != "aa:bb:cc:dd:ee:01\tTest Camera\n" {
		t.Errorf("cameras file = %q", camsFile)
	}

	// Re-apply with no --camera keeps the saved list; --block-stream-setup sticks.
	out, err = runScript(t, sh, env, root, "apply", "yes", "--block-stream-setup")
	if err != nil || strings.Contains(out, "dport 443") || !strings.Contains(out, "stream_setup=blocked") {
		t.Fatalf("block-stream-setup re-apply wrong (%v):\n%s", err, out)
	}

	out, err = runScript(t, sh, env, root, "apply", "no")
	if err != nil {
		t.Fatalf("apply no failed: %v\n%s", err, out)
	}
	if res, _ := ParseResult(out); res.Block != "no" {
		t.Fatalf("apply no result: %+v", res)
	}
	if _, err := os.Stat(filepath.Join(root, "etc/bombecam")); !os.IsNotExist(err) {
		t.Error("config dir left behind")
	}
	cron, _ = os.ReadFile(filepath.Join(root, "etc/crontabs/root"))
	if strings.Contains(string(cron), "bombecam") {
		t.Error("cron entry left behind")
	}
	if _, err := os.Stat(filepath.Join(root, "etc/init.d/bombecam")); !os.IsNotExist(err) {
		t.Error("boot script left behind")
	}
}

func TestScriptFw3Render(t *testing.T) {
	sh, env, root := shellEnv(t, "fw3")
	out, err := runScript(t, sh, env, root, "render", "yes", "--camera", "Test Camera=AA:BB:CC:DD:EE:01")
	if err != nil {
		t.Fatalf("render failed: %v\n%s", err, out)
	}
	for _, s := range []string{
		"*filter", ":bombecam_out - [0:0]",
		"-A bombecam_out -d 192.168.0.0/16 -j RETURN",
		"-A bombecam_out -d 3.1.2.3/32 -p tcp -m tcp --dport 8883 -j bombecam_cap",
		"-A bombecam_out -d 52.9.9.9/32 -p tcp -m tcp --dport 443 -j bombecam_cap",
		"-A bombecam_out -j DROP",
		"-m mac --mac-source aa:bb:cc:dd:ee:01 -m hashlimit --hashlimit-above 4096b/s --hashlimit-burst 8192b --hashlimit-name bc_aabbccddee01 -j DROP",
		"-A bombecam_cap -j ACCEPT", "COMMIT",
	} {
		if !strings.Contains(out, s) {
			t.Errorf("fw3 rules missing %q\n%s", s, out)
		}
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "--dport 443") && !strings.Contains(line, "-d ") {
			t.Errorf("443 open to any destination: %s", line)
		}
	}
	// Syntax check against a real kernel when possible.
	if os.Geteuid() == 0 && runtime.GOOS == "linux" {
		if _, err := exec.LookPath("iptables-legacy-restore"); err == nil {
			cmd := exec.Command("unshare", "-n", "iptables-legacy-restore", "--test", "-n")
			cmd.Stdin = strings.NewReader(out)
			if b, err := cmd.CombinedOutput(); err != nil && !strings.Contains(string(b), "not permitted") {
				t.Fatalf("iptables-restore --test rejected the rules: %v\n%s", err, b)
			}
		}
	}
}

// "connections" sorts each camera connection in the router's conntrack table
// into allowed categories and flags everything else.
// Blocking clears the tracked connections of the blocked cameras' own
// addresses (DHCP lease and IPv6 neighbours), and nobody else's.
func TestScriptApplyClearsCameraConnections(t *testing.T) {
	for _, fw := range []string{"fw4", "fw3"} {
		t.Run(fw, func(t *testing.T) {
			sh, env, root := shellEnv(t, fw)
			sh = shellForFakeApplets(t, sh)
			bin := filepath.Join(root, "bin")
			must(t, os.MkdirAll(bin, 0o755))
			must(t, os.WriteFile(filepath.Join(bin, "ip"), []byte(`#!/bin/sh
case "$*" in
"-6 neigh") printf 'fe80::1 dev br-lan lladdr aa:bb:cc:dd:ee:01 STALE\nfe80::50 dev br-lan lladdr 11:22:33:44:55:66 STALE\n' ;;
esac
`), 0o755))
			must(t, os.MkdirAll(filepath.Join(root, "tmp"), 0o755))
			must(t, os.WriteFile(filepath.Join(root, "tmp/dhcp.leases"), []byte(
				"1900000000 aa:bb:cc:dd:ee:01 192.168.8.123 TestCamera *\n1900000000 11:22:33:44:55:66 192.168.8.50 PC *\n"), 0o644))
			env = prependPath(env, bin)
			out, err := runScript(t, sh, env, root, "apply", "yes", "--camera", "Test Camera=AA:BB:CC:DD:EE:01")
			if err != nil {
				t.Fatalf("apply failed: %v\n%s", err, out)
			}
			var cleared []string
			for _, line := range strings.Split(out, "\n") {
				if a, ok := strings.CutPrefix(line, "[dry-run] clear the router's tracked connections of "); ok {
					cleared = append(cleared, a)
				}
			}
			if strings.Join(cleared, " ") != "192.168.8.123 fe80::1" {
				t.Errorf("cleared %q, want only the camera's addresses\n%s", cleared, out)
			}
			if res, _ := ParseResult(out); res.Status != "ok" || res.Fields["cleared"] != "yes" {
				t.Errorf("result %+v\n%s", res, out)
			}
		})
	}
}

// clear_addr writes only a well-formed address to OpenWrt's conntrack file:
// anything else written there (such as "f") clears every device's connections.
func TestScriptClearAddrWritesOnlyAnAddress(t *testing.T) {
	sh, env, root := shellEnv(t, "fw4")
	ct := filepath.Join(root, "nf_conntrack")
	env = append(env, "BC_DRYRUN=0", "BC_CONNTRACK="+ct)
	clear := func(addr string) (string, error) {
		must(t, os.WriteFile(ct, nil, 0o600))
		var argv []string
		if sh == "busybox" {
			argv = append(argv, "sh")
		}
		// Source the script with a harmless command, then call the function.
		argv = append(argv, "-c", `a=$1; set -- version; . "$0" >/dev/null; clear_addr "$a"`, filepath.Join(root, ScriptName), addr)
		cmd := exec.Command(sh, argv...)
		cmd.Env = env
		if out, err := cmd.CombinedOutput(); err != nil {
			return "", fmt.Errorf("%v: %s", err, out)
		}
		b, err := os.ReadFile(ct)
		return string(b), err
	}
	for _, addr := range []string{"192.168.8.123", "fe80::1", "2001:db8::123"} {
		if got, err := clear(addr); err != nil || got != addr+"\n" {
			t.Errorf("clear_addr %s wrote %q (%v)", addr, got, err)
		}
	}
	for _, addr := range []string{"f", "", "192.168.8", "192.168.8.123 f", "fe80::1;f", "camera", "1.2.3.4\nf"} {
		if got, err := clear(addr); err == nil || got != "" {
			t.Errorf("clear_addr %q was accepted and wrote %q", addr, got)
		}
	}
}

func TestScriptConnections(t *testing.T) {
	sh, env, root := shellEnv(t, "fw4")
	sh = shellForFakeApplets(t, sh)
	if out, err := runScript(t, sh, env, root, "apply", "yes", "--camera", "Test Camera=AA:BB:CC:DD:EE:01"); err != nil {
		t.Fatalf("apply failed: %v\n%s", err, out)
	}
	// apply re-resolved the names; pin the cached addresses (dated today, so
	// they are not aged out) to the fixture's.
	day := time.Now().Unix() / 86400
	must(t, os.WriteFile(filepath.Join(root, "etc/bombecam/ips"), []byte(fmt.Sprintf(
		"mqtts02-us.osaio.net 3.1.2.3 %d\nwss-us.osaio.net 52.9.9.9 %d\n", day, day)), 0o644))
	// A fake ip(8): the camera's IPv6 neighbours and the router's own addresses.
	bin := filepath.Join(root, "bin")
	must(t, os.MkdirAll(bin, 0o755))
	must(t, os.WriteFile(filepath.Join(bin, "ip"), []byte(`#!/bin/sh
case "$*" in
"-6 neigh") printf 'fe80::1 dev br-lan lladdr aa:bb:cc:dd:ee:01 STALE\n2001:db8::123 dev br-lan lladdr AA:BB:CC:DD:EE:01 REACHABLE\n' ;;
"addr") printf '    inet 192.168.8.1/24 brd 192.168.8.255 scope global br-lan\n    inet6 2001:db8::1/64 scope global\n' ;;
esac
`), 0o755))
	must(t, os.MkdirAll(filepath.Join(root, "tmp"), 0o755))
	must(t, os.WriteFile(filepath.Join(root, "tmp/dhcp.leases"), []byte(
		"1900000000 aa:bb:cc:dd:ee:01 192.168.8.123 TestCamera *\n"), 0o644))
	ct := filepath.Join(root, "nf_conntrack")
	must(t, os.WriteFile(ct, []byte(strings.Join([]string{
		// allowed
		"ipv4     2 tcp      6 7431 ESTABLISHED src=192.168.8.123 dst=3.1.2.3 sport=40000 dport=8883 src=3.1.2.3 dst=203.0.113.2 sport=8883 dport=40000 [ASSURED] mark=0 zone=0 use=2",
		"ipv4     2 tcp      6 7431 ESTABLISHED src=192.168.8.123 dst=52.9.9.9 sport=40001 dport=443 src=52.9.9.9 dst=203.0.113.2 sport=443 dport=40001 [ASSURED] mark=0 zone=0 use=2",
		"ipv4     2 udp      17 25 src=192.168.8.123 dst=8.8.8.8 sport=5353 dport=53 src=8.8.8.8 dst=203.0.113.2 sport=53 dport=5353 mark=0 zone=0 use=2",
		"ipv4     2 udp      17 25 src=192.168.8.123 dst=162.159.200.1 sport=123 dport=123 src=162.159.200.1 dst=203.0.113.2 sport=123 dport=123 mark=0 zone=0 use=2",
		// local
		"ipv4     2 tcp      6 100 ESTABLISHED src=192.168.8.123 dst=192.168.8.50 sport=40002 dport=8554 src=192.168.8.50 dst=192.168.8.123 sport=8554 dport=40002 [ASSURED] mark=0 zone=0 use=2",
		"ipv4     2 udp      17 29 src=192.168.8.123 dst=224.0.0.251 sport=5353 dport=5353 [UNREPLIED] src=224.0.0.251 dst=192.168.8.123 sport=5353 dport=5353 mark=0 zone=0 use=2",
		"ipv6     10 udp      17 29 src=fe80::1 dst=ff02::fb sport=5353 dport=5353 [UNREPLIED] src=ff02::fb dst=fe80::1 sport=5353 dport=5353 mark=0 zone=0 use=2",
		"ipv6     10 udp      17 29 src=2001:db8::123 dst=2001:db8::1 sport=5000 dport=53 src=2001:db8::1 dst=2001:db8::123 sport=53 dport=5000 mark=0 zone=0 use=2",
		// not allowed
		"ipv4     2 tcp      6 60 SYN_SENT src=192.168.8.123 dst=104.16.0.1 sport=40003 dport=443 [UNREPLIED] src=104.16.0.1 dst=203.0.113.2 sport=443 dport=40003 mark=0 zone=0 use=2",
		"ipv4     2 udp      17 29 src=192.168.8.123 dst=1.2.3.4 sport=50000 dport=3478 src=1.2.3.4 dst=203.0.113.2 sport=3478 dport=50000 mark=0 zone=0 use=2",
		"ipv4     2 tcp      6 7431 ESTABLISHED src=192.168.8.123 dst=3.1.2.3 sport=40004 dport=443 src=3.1.2.3 dst=203.0.113.2 sport=443 dport=40004 [ASSURED] mark=0 zone=0 use=2",
		"ipv4     2 icmp     1 29 src=192.168.8.123 dst=8.8.8.8 type=8 code=0 id=1 src=8.8.8.8 dst=203.0.113.2 type=0 code=0 id=1 mark=0 zone=0 use=2",
		"ipv6     10 tcp      6 7431 ESTABLISHED src=2001:db8::123 dst=2600:1f18::1 sport=40005 dport=443 src=2600:1f18::1 dst=2001:db8::123 sport=443 dport=40005 [ASSURED] mark=0 zone=0 use=2",
		// another device: ignored
		"ipv4     2 tcp      6 7431 ESTABLISHED src=192.168.8.50 dst=104.16.0.1 sport=40006 dport=443 src=104.16.0.1 dst=203.0.113.2 sport=443 dport=40006 [ASSURED] mark=0 zone=0 use=2",
		"",
	}, "\n")), 0o644))
	env = prependPath(append(env, "BC_CONNTRACK="+ct), bin)

	out, err := runScript(t, sh, env, root, "connections")
	if err != nil {
		t.Fatalf("connections failed: %v\n%s", err, out)
	}
	for _, want := range []string{
		"Test Camera  aa:bb:cc:dd:ee:01  192.168.8.123 fe80::1 2001:db8::123",
		"control        1", "stream_setup   1", "dns            1", "time           1",
		"your network   3", "this router    1", "not allowed    5",
		"tcp 104.16.0.1:443 SYN_SENT (no reply)", "udp 1.2.3.4:3478", "tcp 3.1.2.3:443 ESTABLISHED",
		"icmp 8.8.8.8", "tcp [2600:1f18::1]:443 ESTABLISHED",
		"reboot the router",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q", want)
		}
	}
	res, ok := ParseResult(out)
	if !ok || res.Fields["connections"] != "13" || res.Fields["not_allowed"] != "5" {
		t.Errorf("result = %+v", res)
	}
	if t.Failed() {
		t.Log(out)
	}

	// With stream setup blocked, the wss-us connection is no longer allowed.
	if out, err := runScript(t, sh, env, root, "apply", "yes", "--block-stream-setup"); err != nil {
		t.Fatalf("re-apply failed: %v\n%s", err, out)
	}
	out, _ = runScript(t, sh, env, root, "connections")
	if !strings.Contains(out, "tcp 52.9.9.9:443 ESTABLISHED") || !strings.Contains(out, "not_allowed=6") {
		t.Errorf("stream setup still allowed after --block-stream-setup:\n%s", out)
	}
}

// router-setup/ ships a copy of the script for manual installs; it must be
// byte-for-byte the embedded one, with LF line endings.
func TestRouterSetupCopyMatchesEmbeddedScript(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "..", "router-setup", ScriptName))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(b, Script()) {
		t.Fatal("router-setup/bombecam-router.sh differs from pkg/renderer/openwrt/bombecam-router.sh; copy it over")
	}
	if bytes.Contains(b, []byte("\r")) {
		t.Error("bombecam-router.sh must use LF line endings")
	}
}

// Flow offloading lets established connections skip the forward chain (and
// the cap) on fw4 and fw3 alike, so apply turns it off and "no" restores it.
func TestScriptOffloadingOffOnBothFirewalls(t *testing.T) {
	for _, fw := range []string{"fw4", "fw3"} {
		t.Run(fw, func(t *testing.T) {
			sh, env, root := shellEnv(t, fw)
			bin := filepath.Join(root, "bin")
			must(t, os.MkdirAll(bin, 0o755))
			must(t, os.WriteFile(filepath.Join(bin, "uci"), []byte(`#!/bin/sh
[ "$1" = -q ] && shift
case "$1 $2" in
"get firewall.@defaults[0].flow_offloading") echo 1 ;;
"get firewall.@defaults[0].flow_offloading_hw") echo 0 ;;
*) exit 1 ;;
esac
`), 0o755))
			env = prependPath(env, bin)
			out, err := runScript(t, sh, env, root, "apply", "yes", "--camera", "Test Camera=AA:BB:CC:DD:EE:01")
			if err != nil || !strings.Contains(out, "[dry-run] uci set firewall.@defaults[0].flow_offloading=0") ||
				!strings.Contains(out, "[dry-run] uci commit firewall") {
				t.Fatalf("offloading not turned off (%v):\n%s", err, out)
			}
			out, err = runScript(t, sh, env, root, "apply", "no")
			if err != nil || !strings.Contains(out, "[dry-run] uci set firewall.@defaults[0].flow_offloading=1") {
				t.Fatalf("offloading not restored (%v):\n%s", err, out)
			}
		})
	}
}

// connect installs BombeCam's key restricted to the gate; the gate runs only
// its verbs; "apply no" keeps BombeCam connected; uninstall removes it all.
func TestScriptConnectGateUninstall(t *testing.T) {
	sh, env, root := shellEnv(t, "fw4")
	authKeys := filepath.Join(root, "etc/dropbear/authorized_keys")
	must(t, os.MkdirAll(filepath.Dir(authKeys), 0o700))
	must(t, os.WriteFile(authKeys, []byte("ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOTHERKEYOTHERKEYOTHERKEYOTHERKEYOTHERKEY user@laptop\n"), 0o600))
	env = append(env, "BC_AUTH_KEYS="+authKeys)
	key := "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIGb0mbeCamGatewayTestKeyTestKeyTestKeyTest1"

	for _, bad := range []string{"ssh-dss AAAAB3NzaC1kc3MAAACBAP", "ssh-ed25519 AAAA\"; rm -rf /", "command=\"x\" " + key,
		"garbage\n" + key, key + "\nssh-rsa AAAAB3NzaC1yc2EAAAADAQABAAABAQDxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"} {
		if out, err := runScript(t, sh, env, root, "connect", "--key", bad); err == nil {
			t.Errorf("bad key %q accepted:\n%s", bad, out)
		}
	}
	out, err := runScript(t, sh, env, root, "connect", "--key", key+" some comment")
	if res, ok := ParseResult(out); err != nil || !ok || res.Fields["connected"] != "yes" || res.Fields["version"] != ScriptVersion() {
		t.Fatalf("connect failed (%v):\n%s", err, out)
	}
	ak, _ := os.ReadFile(authKeys)
	lines := strings.Split(strings.TrimSpace(string(ak)), "\n")
	if len(lines) != 2 || !strings.HasSuffix(lines[0], "user@laptop") ||
		!regexp.MustCompile(`^command="[^"]*bombecam-router gate",no-port-forwarding,no-agent-forwarding,no-X11-forwarding,no-pty ssh-ed25519 \S+ bombecam-gateway$`).MatchString(lines[1]) {
		t.Fatalf("authorized_keys:\n%s", ak)
	}
	// Connecting again replaces BombeCam's line rather than adding another.
	if _, err := runScript(t, sh, env, root, "connect", "--key", key); err != nil {
		t.Fatal(err)
	}
	if ak2, _ := os.ReadFile(authKeys); strings.Count(string(ak2), "bombecam-router gate") != 1 {
		t.Fatalf("duplicate key lines:\n%s", ak2)
	}

	forced := regexp.MustCompile(`command="([^"]*)"`).FindStringSubmatch(lines[1])[1]
	gate := func(verb, stdin string) (string, error) {
		cmd := exec.Command("sh", "-c", forced)
		cmd.Env = append(env, "SSH_ORIGINAL_COMMAND="+verb)
		cmd.Stdin = strings.NewReader(stdin)
		b, err := cmd.CombinedOutput()
		return string(b), err
	}
	spec, _ := ApplySpec([]Camera{{Name: "Test Camera", MAC: "AA:BB:CC:DD:EE:01"}, {Name: "Porch", MAC: "aa:bb:cc:dd:ee:02"}}, policy.Options{}, policy.RegionUS)
	out, err = gate(GateApply, string(spec))
	if res, _ := ParseResult(out); err != nil || res.Block != "yes" || res.Fields["cameras"] != "2" || res.Fields["connected"] != "yes" {
		t.Fatalf("gate apply failed (%v):\n%s", err, out)
	}
	for _, bad := range []struct{ verb, stdin string }{
		{"id", ""}, {"sh -c id", ""}, {"apply;id", ""}, {"", ""},
		{GateApply, "camera zz:zz Bad\n"}, {GateApply, "rm -rf /\n"}, {GateApply, "camera aa:bb:cc:dd:ee:01 X\n$(id)\n"},
	} {
		if out, err := gate(bad.verb, bad.stdin); err == nil || strings.Contains(out, "uid=") {
			t.Errorf("gate accepted %q / %q:\n%s", bad.verb, bad.stdin, out)
		}
	}
	// A failed request leaves the previous rules in place.
	if cams, _ := os.ReadFile(filepath.Join(root, "etc/bombecam/cameras")); strings.Count(string(cams), "\n") != 2 {
		t.Fatalf("cameras file changed by a refused request: %q", cams)
	}
	// An empty list turns blocking off but keeps BombeCam connected.
	out, err = gate(GateApply, "block_stream_setup 0\n")
	if res, _ := ParseResult(out); err != nil || res.Block != "no" || res.Fields["connected"] != "yes" {
		t.Fatalf("empty apply (%v):\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(root, "usr/sbin/bombecam-router")); err != nil {
		t.Fatal("program removed although BombeCam is still connected")
	}
	if out, err = gate(GateVersion, ""); err != nil || !strings.Contains(out, "connected=yes") {
		t.Fatalf("version (%v):\n%s", err, out)
	}
	out, err = gate(GateUninstall, "")
	if res, _ := ParseResult(out); err != nil || res.Fields["connected"] != "no" {
		t.Fatalf("uninstall (%v):\n%s", err, out)
	}
	if ak3, _ := os.ReadFile(authKeys); strings.Contains(string(ak3), "bombecam") || !strings.Contains(string(ak3), "user@laptop") {
		t.Fatalf("authorized_keys after uninstall:\n%s", ak3)
	}
	if _, err := os.Stat(filepath.Join(root, "etc/bombecam")); !os.IsNotExist(err) {
		t.Fatal("config left behind after uninstall")
	}
}
