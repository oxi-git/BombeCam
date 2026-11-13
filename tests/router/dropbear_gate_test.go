package router

// A real-SSH-server test of BombeCam's router key: dropbear (the SSH server
// OpenWrt and GL.iNet ship) honouring the forced command and restrictions the
// router script writes into authorized_keys. Run it with
// tests/router/dropbear_gate_test.sh, which starts dropbear and sets the
// BOMBECAM_DROPBEAR_* variables; without them the test is skipped.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/Fever-r/BombeCam/pkg/policy"
	"github.com/Fever-r/BombeCam/pkg/renderer/openwrt"
	"github.com/Fever-r/BombeCam/pkg/routerpush"
)

type dropbearEnv struct {
	addr, user, pass, root, authKeys string
}

func dropbearFromEnv(t *testing.T) dropbearEnv {
	t.Helper()
	e := dropbearEnv{
		addr:     os.Getenv("BOMBECAM_DROPBEAR_ADDR"),
		user:     os.Getenv("BOMBECAM_DROPBEAR_USER"),
		pass:     os.Getenv("BOMBECAM_DROPBEAR_PASS"),
		root:     os.Getenv("BOMBECAM_DROPBEAR_ROOT"),
		authKeys: os.Getenv("BOMBECAM_DROPBEAR_AUTHKEYS"),
	}
	if e.addr == "" {
		t.Skip("set up by tests/router/dropbear_gate_test.sh")
	}
	return e
}

func TestDropbearGate(t *testing.T) {
	e := dropbearFromEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	ks, err := routerpush.NewKeySet()
	if err != nil {
		t.Fatal(err)
	}
	env := fmt.Sprintf("BC_ROOT=%s BC_DRYRUN=1 BC_FW=fw4 BC_AUTH_KEYS=%s", e.root, e.authKeys)
	uploaded := filepath.Join(e.root, "uploaded.sh")

	// 1. Connect over the password: install the script and the keys.
	quoted := []string{}
	for _, a := range openwrt.ConnectArgs(ks.AuthorizedKeys()) {
		quoted = append(quoted, openwrt.ShellQuote(a))
	}
	cmd := fmt.Sprintf("umask 077 && cat > %s && %s sh %s %s", uploaded, env, uploaded, strings.Join(quoted, " "))
	res, err := routerpush.Run(ctx, routerpush.Target{Address: e.addr, User: e.user, Password: e.pass}, cmd, openwrt.Script())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if r, ok := openwrt.ParseResult(res.Output); !ok || r.Fields["connected"] != "yes" {
		t.Fatalf("connect failed:\n%s", res.Output)
	}
	hostKey, hostKeyType := res.HostKey, res.HostKeyType
	ak, _ := os.ReadFile(e.authKeys)
	if !strings.Contains(string(ak), "user@laptop") {
		t.Fatalf("the user's own key was removed:\n%s", ak)
	}
	if n := strings.Count(string(ak), "bombecam-router gate"); n != 2 {
		t.Fatalf("expected 2 restricted BombeCam keys, found %d:\n%s", n, ak)
	}

	keyTarget := routerpush.Target{Address: e.addr, User: e.user, Signers: ks.Signers(), PinnedHostKey: hostKey, PinnedHostKeyType: hostKeyType}

	// 2. Apply over the key (no password).
	spec, _ := openwrt.ApplySpec([]openwrt.Camera{{Name: "Test Camera", MAC: "AA:BB:CC:DD:EE:01"}}, policy.Options{}, policy.RegionUS)
	res, err = routerpush.Run(ctx, keyTarget, openwrt.GateApply, spec)
	if err != nil {
		t.Fatalf("apply over key: %v", err)
	}
	if r, ok := openwrt.ParseResult(res.Output); !ok || r.Status != "ok" || r.Block != "yes" || r.Fields["connected"] != "yes" {
		t.Fatalf("apply over key failed:\n%s", res.Output)
	}
	cams, _ := os.ReadFile(filepath.Join(e.root, "etc/bombecam/cameras"))
	if string(cams) != "aa:bb:cc:dd:ee:01\tTest Camera\n" {
		t.Fatalf("cameras file = %q", cams)
	}

	// 3. The RSA key alone works too (routers whose dropbear lacks Ed25519).
	rsaOnly := keyTarget
	rsaOnly.Signers = ks.Signers()[1:]
	if res, err = routerpush.Run(ctx, rsaOnly, openwrt.GateVersion, nil); err != nil || !strings.Contains(res.Output, "version="+openwrt.ScriptVersion()) {
		t.Fatalf("rsa key: %v\n%s", err, res.Output)
	}

	// 4. Nothing else runs over the key.
	for _, bad := range []string{"id", "sh", "cat /etc/passwd", "apply; id", "status && id", "uninstall --now", ""} {
		res, err = routerpush.Run(ctx, keyTarget, bad, nil)
		if err != nil {
			t.Fatalf("%q: %v", bad, err)
		}
		if res.ExitCode == 0 || strings.Contains(res.Output, "uid=") || strings.Contains(res.Output, "root:") ||
			!strings.Contains(res.Output, "can only run") {
			t.Errorf("%q was not refused (exit %d):\n%s", bad, res.ExitCode, res.Output)
		}
	}

	// 5. No shell, no pty, no port forwarding.
	client, err := ssh.Dial("tcp", e.addr, &ssh.ClientConfig{
		User: e.user, Auth: []ssh.AuthMethod{ssh.PublicKeys(ks.Signers()...)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), Timeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if c, err := client.Dial("tcp", "127.0.0.1:22"); err == nil {
		c.Close()
		t.Error("port forwarding allowed over BombeCam's key")
	}
	sess, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.RequestPty("xterm", 24, 80, ssh.TerminalModes{}); err == nil {
		t.Error("pty allowed over BombeCam's key")
	}
	sess.Close()
	shell, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	shell.Stdout, shell.Stderr = &out, &out
	if err := shell.Shell(); err != nil {
		t.Fatal(err)
	}
	_ = shell.Wait() // the shell request runs the gate with no verb, which refuses
	if !strings.Contains(out.String(), "can only run") {
		t.Errorf("shell request not refused:\n%s", out.String())
	}

	// 6. Uninstall over the key removes the rules and the key itself.
	res, err = routerpush.Run(ctx, keyTarget, openwrt.GateUninstall, nil)
	if err != nil {
		t.Fatalf("uninstall: %v", err)
	}
	if r, ok := openwrt.ParseResult(res.Output); !ok || r.Fields["connected"] != "no" {
		t.Fatalf("uninstall failed:\n%s", res.Output)
	}
	ak, _ = os.ReadFile(e.authKeys)
	if strings.Contains(string(ak), "bombecam") || !strings.Contains(string(ak), "user@laptop") {
		t.Fatalf("authorized_keys after uninstall:\n%s", ak)
	}
	if _, err := routerpush.Run(ctx, keyTarget, openwrt.GateVersion, nil); !errors.Is(err, routerpush.ErrAuth) {
		t.Fatalf("key still accepted after uninstall: %v", err)
	}
}

// A dropbear with only an Ed25519 host key must still be reachable (Go's
// default host key order makes dropbear 2022.83 abort the handshake).
func TestDropbearEd25519OnlyHostKey(t *testing.T) {
	e := dropbearFromEnv(t)
	addr := os.Getenv("BOMBECAM_DROPBEAR_ED25519_ADDR")
	if addr == "" {
		t.Skip("no Ed25519-only dropbear")
	}
	res, err := routerpush.Run(context.Background(), routerpush.Target{Address: addr, User: e.user, Password: e.pass}, "echo reached", nil)
	if err != nil || !strings.Contains(res.Output, "reached") || res.HostKeyType != "ssh-ed25519" {
		t.Fatalf("ed25519-only router: %v %+v", err, res)
	}
}
