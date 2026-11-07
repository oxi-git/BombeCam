package netstack

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func fakeResolver(answers map[string][]string) func(context.Context, string) ([]net.IP, error) {
	return func(_ context.Context, host string) ([]net.IP, error) {
		a, ok := answers[host]
		if !ok {
			return nil, errors.New("no such host")
		}
		var out []net.IP
		for _, s := range a {
			out = append(out, net.ParseIP(s))
		}
		return out, nil
	}
}

func TestParseCameras(t *testing.T) {
	subs, err := ParseCameras([]string{"Test Camera=AA:BB:CC:DD:EE:01", " aa:bb:cc:dd:ee:02 "})
	if err != nil {
		t.Fatal(err)
	}
	if subs[0].Name != "Test Camera" || subs[0].MAC != "AA:BB:CC:DD:EE:01" || subs[1].Name != "camera_2" {
		t.Fatalf("got %+v", subs)
	}
	if _, err := ParseCameras(nil); err == nil {
		t.Fatal("no cameras must be an error")
	}
	if _, err := NewRulesetManager(RulesetConfig{Cameras: []string{"x=not-a-mac"}}); err == nil {
		t.Fatal("bad MAC accepted")
	}
}

func TestRulesetManagerResolveAndRender(t *testing.T) {
	answers := map[string][]string{
		"mqtts02-us.osaio.net": {"3.1.2.3"},
		"wss-us.osaio.net":     {"52.9.9.9", "2600::1"},
	}
	m, err := NewRulesetManager(RulesetConfig{
		Cameras:  []string{"Test Camera=aa:bb:cc:dd:ee:01"},
		Resolver: fakeResolver(answers),
	})
	if err != nil {
		t.Fatal(err)
	}
	changed, err := m.Resolve(context.Background())
	if err != nil || !changed {
		t.Fatalf("first resolve: changed=%v err=%v", changed, err)
	}
	if changed, _ := m.Resolve(context.Background()); changed {
		t.Fatal("unchanged answers reported as a change")
	}
	// A new address is added; the old one is kept (it may still carry an open
	// control connection).
	answers["mqtts02-us.osaio.net"] = []string{"3.1.2.9"}
	if changed, _ := m.Resolve(context.Background()); !changed {
		t.Fatal("new address not reported")
	}
	out, err := m.Render(true)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "elements = { 3.1.2.3, 3.1.2.9 }") || strings.Contains(out, "2600::1") {
		t.Fatalf("unexpected sets:\n%s", out)
	}
	off, _ := m.Render(false)
	if strings.Contains(off, "chain") {
		t.Fatal("No must not render chains")
	}
	// Lookup failures keep the previous addresses.
	delete(answers, "wss-us.osaio.net")
	if _, err := m.Resolve(context.Background()); err == nil {
		t.Fatal("expected lookup error")
	}
	if got := m.Resolved()["wss-us.osaio.net"]; len(got) != 1 {
		t.Fatalf("addresses lost on lookup failure: %v", got)
	}
}

func TestStateFailClosed(t *testing.T) {
	dir := t.TempDir()
	sm := NewStateManager(dir)
	block, st, err := sm.RestoreOnBoot()
	if err != nil || !block || st.Status != StatusDegradedFailClosed {
		t.Fatalf("missing state: block=%v status=%s err=%v", block, st.Status, err)
	}
	if err := sm.Persist(false, "", "", StatusRemoved); err != nil {
		t.Fatal(err)
	}
	if block, st, _ = sm.RestoreOnBoot(); block || st.Status != StatusRemoved {
		t.Fatalf("persisted No restored as block=%v status=%s", block, st.Status)
	}
	if err := sm.Persist(true, "", "table inet bombecam {}", ""); err != nil {
		t.Fatal(err)
	}
	if block, st, _ = sm.RestoreOnBoot(); !block || st.RulesetHash == "" {
		t.Fatalf("persisted Yes restored as block=%v hash=%q", block, st.RulesetHash)
	}
	must(t, os.WriteFile(filepath.Join(dir, "block_cloud_video"), []byte("maybe"), 0o600))
	if block, st, _ = sm.RestoreOnBoot(); !block || st.Status != StatusDegradedFailClosed {
		t.Fatal("corrupt setting must fail closed")
	}
	must(t, os.WriteFile(filepath.Join(dir, "block_cloud_video"), []byte("no\n"), 0o600))
	must(t, os.WriteFile(filepath.Join(dir, "state.json"), []byte("{truncated"), 0o600))
	if block, _, err = sm.RestoreOnBoot(); !block || err == nil {
		t.Fatal("unparseable state.json must fail closed with an error")
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
