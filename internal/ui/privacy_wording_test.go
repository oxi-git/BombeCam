package ui

import (
	"io/fs"
	"regexp"
	"strings"
	"testing"

	"github.com/Fever-r/BombeCam/pkg/policy"
)

func readAsset(t *testing.T, name string) string {
	t.Helper()
	b, err := fs.ReadFile(FS(), name)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// The Firewall tab has the exact headline, a short hardware notice linking
// to the "other ways" page, one "Block all cameras" switch, a switch per camera
// (rendered by app.js), and "How it works" behind a disclosure.
func TestPrivacyUIStructure(t *testing.T) {
	html := readAsset(t, "index.html")
	js := readAsset(t, "app.js")
	if !strings.Contains(html, policy.Headline) {
		t.Error("index.html must show the exact headline")
	}
	for _, text := range []string{html, readAsset(t, "blocking-options.html"), policy.Headline} {
		if strings.Contains(text, "no video or images leave") || strings.Contains(text, "No video or images leave") || strings.Contains(text, "live video can't get through") {
			t.Fatal("unsupported image-content guarantee")
		}
	}
	for _, text := range []string{"8&nbsp;KB burst", "4-packet/s", "cannot determine whether its payload contains image data"} {
		if !strings.Contains(html, text) {
			t.Errorf("missing actual policy limit: %s", text)
		}
	}
	if n := strings.Count(html, `id="privacy-master"`); n != 1 || !strings.Contains(html, "Block all cameras") {
		t.Errorf("expected one Block all cameras switch, found %d", n)
	}
	if !regexp.MustCompile(`class="privacy-hardware">\s*<strong>Needs:</strong>\s*<span id="privacy-hardware-text">a GL\.iNet or OpenWrt router`).MatchString(html) ||
		!strings.Contains(html, `href="/blocking-options.html"`) {
		t.Error("the hardware notice and the link to the other-ways page must be on the tab")
	}
	for _, id := range []string{`id="privacy-camera-list"`, `id="btn-privacy-connect"`, `id="btn-privacy-disconnect"`} {
		if !strings.Contains(html, id) {
			t.Errorf("index.html lacks %s", id)
		}
	}
	for _, api := range []string{"/api/v1/privacy/camera", "/api/v1/privacy/block-all", "/api/v1/privacy/router/connect", "/api/v1/privacy/router/disconnect"} {
		if !strings.Contains(js, api) {
			t.Errorf("app.js does not use %s", api)
		}
	}
	if strings.Contains(html, `data-choice="`) {
		t.Error("the old Yes/No selector is gone")
	}
	if !regexp.MustCompile(`<details id="privacy-how" class="how-it-works">\s*<summary>How it works</summary>`).MatchString(html) {
		t.Error(`"How it works" must be a collapsed <details> panel`)
	}
	for _, section := range []string{"How the blocking works", "What it requires", "Setup"} {
		if !strings.Contains(html, "<h3>"+section+"</h3>") {
			t.Errorf("How it works is missing the %q section", section)
		}
	}
	for _, must := range []string{
		"What Osaio can still see", "your internet (IP) address", "which commands are sent",
		"mqtts02-us.osaio.net", "4&nbsp;KB/s per camera", "all IPv6", "Backblaze",
		"GL.iNet router on firmware 4.x", "Router mode", "Nothing is redirected or changed",
		"never saves it", "can only turn camera blocking on and off",
	} {
		if !strings.Contains(html, must) {
			t.Errorf("How it works should mention %q", must)
		}
	}
}

// The Firewall tab lets the user choose where the firewall runs: the router
// over SSH or the bombecam-net daemon on this computer.
func TestPrivacyBackendUI(t *testing.T) {
	html := readAsset(t, "index.html")
	js := readAsset(t, "app.js")
	for _, id := range []string{`id="privacy-backend"`, `id="privacy-backend-detail"`} {
		if !strings.Contains(html, id) {
			t.Errorf("index.html lacks %s", id)
		}
	}
	if !strings.Contains(html, `<option value="local">This computer (bombecam-net)</option>`) {
		t.Error("the backend selector must offer the local daemon")
	}
	for _, api := range []string{
		"/api/v1/privacy/backend", "/api/v1/privacy/region", "/api/v1/privacy/check",
		"/api/v1/privacy/camera", "/api/v1/privacy/block-all",
		"/api/v1/privacy/router/connect", "/api/v1/privacy/router/disconnect",
	} {
		if !strings.Contains(js, api) {
			t.Errorf("app.js does not use %s", api)
		}
	}
	if !strings.Contains(html, `id="privacy-region"`) {
		t.Error("index.html must expose the Osaio region next to the backend selector")
	}
	for _, id := range []string{`id="privacy-how-local"`, `id="privacy-hardware-text"`} {
		if !strings.Contains(html, id) {
			t.Errorf("index.html lacks %s", id)
		}
	}
}

// The separate information page compares the ways to block, with the costs of
// each, and is served with the UI.
func TestBlockingOptionsPage(t *testing.T) {
	page := readAsset(t, "blocking-options.html")
	for _, must := range []string{
		"<title>Ways to block camera traffic", `href="style.css"`, `href="/"`,
		"BombeCam router block", "Small travel router", "Full internet block", "Guest or IoT network",
		"Your own firewall rules", "Linux box", "DNS blocking", "Cloud features off in the Osaio app",
		"New streams can't start", "8.8.8.8", "Router mode", "information only",
		"Osaio still sees that the camera is online, your IP address, and which commands are sent",
	} {
		if !strings.Contains(page, must) {
			t.Errorf("blocking-options.html should mention %q", must)
		}
	}
	if p, c := strings.Count(page, "<h3>Pros</h3>"), strings.Count(page, "<h3>Cons</h3>"); p != 8 || c != 8 {
		t.Errorf("each of the 8 methods needs Pros and Cons (got %d / %d)", p, c)
	}
	// plain, non-repetitive wording: no alarm headings, and no restating
	// that a blocked camera loses its cloud features
	for _, gone := range []string{"Watch out", "No firmware updates, alerts or cloud clips", "cloud clips and firmware updates stop"} {
		if strings.Contains(page, gone) {
			t.Errorf("blocking-options.html should no longer say %q", gone)
		}
	}
	if strings.Contains(page, "<script") {
		t.Error("the information page needs no script")
	}
}

// Tabs for the viewer, cameras, Osaio logins, streams & ports and the
// firewall; ports and stream addresses live on their own tab, not in the
// viewer; Settings has Shut down; sign-in says BombeCam needs its own Osaio
// login.
func TestLayout(t *testing.T) {
	html := readAsset(t, "index.html")
	js := readAsset(t, "app.js")
	for _, must := range []string{
		`data-tab="tab-dashboard" id="nav-tab-viewer">📹 Viewer</button>`,
		`data-tab="tab-cameras" id="nav-tab-cameras">📷 Cameras</button>`,
		`data-tab="tab-logins" id="nav-tab-logins">🔑 Osaio logins`,
		`data-tab="tab-streams" id="nav-tab-streams">🔗 Streams &amp; ports</button>`,
		`data-tab="tab-safety" id="nav-tab-privacy">🛡️ Firewall</button>`,
		`id="btn-shutdown"`, "BombeCam needs its own Osaio login", "Use a login only BombeCam uses",
		"Firewall rules stay on your router",
	} {
		if !strings.Contains(html, must) {
			t.Errorf("index.html should contain %q", must)
		}
	}
	if strings.Contains(html, "Cloud video</button>") {
		t.Error("the tab is called Firewall now")
	}
	viewer := html[strings.Index(html, `<section id="tab-dashboard"`):]
	viewer = viewer[:strings.Index(viewer, "</section>")]
	for _, gone := range []string{"port-tag", "stream-rtsp-url", "stream-hls-url", "snippet-frigate", "rtsp://"} {
		if strings.Contains(viewer, gone) {
			t.Errorf("the viewer should no longer show %q (it belongs on Streams & ports)", gone)
		}
	}
	streams := html[strings.Index(html, `<section id="tab-streams"`):strings.Index(html, `<section id="tab-safety"`)]
	for _, must := range []string{`id="stream-list"`, `id="port-tag-rtsp"`, `id="port-tag-rtsp-udp"`, `id="port-tag-hls"`, `id="port-tag-webrtc"`, `id="snippet-frigate"`, `href="/integrations.html"`} {
		if !strings.Contains(streams, must) {
			t.Errorf("Streams & ports should contain %s", must)
		}
	}
	cameras := html[strings.Index(html, `<section id="tab-cameras"`):strings.Index(html, `<section id="tab-logins"`)]
	for _, must := range []string{`id="cameras-list"`, `id="cameras-filter"`} {
		if !strings.Contains(cameras, must) {
			t.Errorf("the Cameras tab should contain %s", must)
		}
	}
	settings := html[strings.Index(html, `<div id="modal-manage"`):]
	if strings.Contains(settings, "manage-cameras-list") || strings.Contains(html, "modal-credentials") {
		t.Error("cameras and passwords are managed on their own tabs, not in Settings")
	}
	for _, must := range []string{"/api/v1/gateway/shutdown", "rtsp_udp_ports", "Firewall: ", "/api/v1/accounts/signin", "/api/v1/accounts/remove"} {
		if !strings.Contains(js, must) {
			t.Errorf("app.js should use %q", must)
		}
	}
	if strings.Contains(js, "'Cloud video: ") {
		t.Error("the header badge says Firewall now")
	}
}

// Wording must stay accurate: no "zero exposure"-style claims, and none of
// the retired tier / Privacy Mode / Convenience Mode vocabulary.
func TestPrivacyUIWordingIsAccurate(t *testing.T) {
	for _, name := range []string{"index.html", "app.js", "blocking-options.html", "integrations.html", "integrations.js"} {
		body := strings.ToLower(readAsset(t, name))
		for _, banned := range []string{
			"zero exposure", "completely private", "100% private", "fully private", "no data leaves",
			"privacy mode", "convenience mode", "tier 0", "tier 1", "tier 2", "tier 3", "severance",
			"restore-internet", "/api/v1/firewall/",
		} {
			if strings.Contains(body, banned) {
				t.Errorf("%s contains %q", name, banned)
			}
		}
	}
}

// The Frigate / Home Assistant page: linked from the viewer, every setup
// step, copy buttons, and nothing that sends data anywhere but
// BombeCam's own API.
func TestIntegrationsPage(t *testing.T) {
	page := readAsset(t, "integrations.html")
	js := readAsset(t, "integrations.js")
	index := readAsset(t, "index.html")
	if !strings.Contains(index, `href="/integrations.html"`) {
		t.Error("the snippets card must link to the Frigate / Home Assistant page")
	}
	for _, must := range []string{
		"<title>Use with Frigate and Home Assistant", `href="style.css"`, `href="/"`, `name="viewport"`,
		// where they run
		"On another device", "In Docker Desktop on this PC", "Directly on this PC",
		// Frigate
		"Configuration editor", "go2rtc:", "each section may appear only once", "Starting Frigate from scratch",
		// Home Assistant
		"Generic Camera", "Use wallclock as timestamps", "Mosquitto broker",
		// checking and firewall
		"VLC", "ffprobe -rtsp_transport tcp", "mediamtx.exe", "Private", "Network profile type",
		// options
		"Stream password", "Snapshots for other devices", "Ports",
		`data-copy-from="integ-frigate"`, `src="integrations.js"`,
	} {
		if !strings.Contains(page, must) {
			t.Errorf("integrations.html should mention %q", must)
		}
	}
	for _, api := range []string{"/api/v1/integrations/settings", "/api/v1/integrations/frigate", "/api/v1/integrations/homeassistant", "/api/v1/auth/csrf", "X-CSRF-Token", "host.docker.internal",
		"Settings &rarr; Devices &amp; services", "Everything looks good.", "RTSP transport protocol", "More options"} {
		if !strings.Contains(js, api) {
			t.Errorf("integrations.js does not use %s", api)
		}
	}
	for _, bad := range []string{"http://", "https://"} {
		for _, l := range strings.Split(js, "\n") {
			if strings.Contains(l, bad+"'") || strings.Contains(l, "fetch('"+bad) {
				t.Errorf("integrations.js fetches an absolute URL: %s", l)
			}
		}
	}
	if strings.Contains(page, "<script src=\"http") {
		t.Error("no external scripts")
	}
}
