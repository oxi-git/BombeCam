package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/Fever-r/BombeCam/internal/ui"
)

// The page is embedded in the binary, so a broken script ships silently: the
// browser shows the static HTML and no request ever leaves it. A syntax error
// in app.js once made every LAN visitor see the "Add your cameras" form, as if
// the setup had been lost. Check the script parses; skip when node is absent.
func TestWebScriptParses(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed: cannot check app.js syntax")
	}
	b, err := ui.FS().(interface {
		ReadFile(string) ([]byte, error)
	}).ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	f := filepath.Join(t.TempDir(), "app.js")
	if err := os.WriteFile(f, b, 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(node, "--check", f).CombinedOutput(); err != nil {
		t.Fatalf("app.js does not parse: %v\n%s", err, out)
	}
}
