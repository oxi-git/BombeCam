package policy

import (
	"strings"
	"testing"
)

var testCam = Subject{ID: "cam1", Name: "Test Camera", MAC: "AA:BB:CC:DD:EE:01"}

func TestCompileRejectsBadSubjects(t *testing.T) {
	cases := map[string][]Subject{
		"none":      nil,
		"no id":     {{ID: "", MAC: "aa:bb:cc:dd:ee:01"}},
		"no mac":    {{ID: "c"}},
		"bad mac":   {{ID: "c", MAC: "zz:bb:cc:dd:ee:01"}},
		"multicast": {{ID: "c", MAC: "01:00:5e:00:00:01"}},
		"zero":      {{ID: "c", MAC: "00:00:00:00:00:00"}},
		"duplicate": {{ID: "a", MAC: "aa:bb:cc:dd:ee:01"}, {ID: "b", MAC: "AA:BB:CC:DD:EE:01"}},
	}
	for name, subs := range cases {
		if _, err := Compile(subs, Setting{BlockCloudVideo: true}, Options{}); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestNoMeansNoRules(t *testing.T) {
	doc, err := Compile([]Subject{testCam}, Setting{BlockCloudVideo: false}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if doc.BlockCloudVideo || len(doc.Allow) != 0 {
		t.Fatalf("Block cloud video = No must produce no allowlist, got %+v", doc.Allow)
	}
}

func TestYesAllowlist(t *testing.T) {
	doc, err := Compile([]Subject{testCam}, Setting{BlockCloudVideo: true}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if doc.Subjects[0].MAC != "aa:bb:cc:dd:ee:01" {
		t.Errorf("MAC not canonicalised: %q", doc.Subjects[0].MAC)
	}
	ctl, ok := doc.Rule("control")
	if !ok || ctl.Host != "mqtts02-us.osaio.net" || ctl.Port != 8883 {
		t.Fatalf("control rule wrong: %+v", ctl)
	}
	// Regression: the old Tier 2C pointed control at AWS IoT.
	for _, r := range doc.Allow {
		if strings.Contains(r.Host, "amazonaws.com") {
			t.Errorf("rule %s still points at %s", r.ID, r.Host)
		}
	}
	if _, ok := doc.Rule("stream_setup"); !ok {
		t.Error("stream setup should be allowed by default")
	}
	for _, id := range []string{"dns", "time"} {
		if _, ok := doc.Rule(id); !ok {
			t.Errorf("missing %s rule", id)
		}
	}
	if doc.CapBytesPerSecond != 4096 {
		t.Errorf("cap = %d, want 4096", doc.CapBytesPerSecond)
	}
	got := strings.Join(doc.Hosts(), ",")
	if got != "mqtts02-us.osaio.net,wss-us.osaio.net" {
		t.Errorf("hosts = %s", got)
	}
}

func TestBlockStreamSetupOption(t *testing.T) {
	doc, err := Compile([]Subject{testCam}, Setting{BlockCloudVideo: true}, Options{BlockStreamSetup: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := doc.Rule("stream_setup"); ok {
		t.Fatal("stream setup rule present despite BlockStreamSetup")
	}
	if len(doc.Hosts()) != 1 {
		t.Fatalf("hosts = %v", doc.Hosts())
	}
}

// Regression for the old OpenWrt Tier 1, which allowed 443 and 8883 to any
// destination.
func TestValidateRejectsAnyDestinationWebPorts(t *testing.T) {
	doc, _ := Compile([]Subject{testCam}, Setting{BlockCloudVideo: true}, Options{})
	for _, port := range []int{443, 8883, 80} {
		bad := *doc
		bad.Allow = append(append([]AllowRule(nil), doc.Allow...),
			AllowRule{ID: "leak", Protocols: []Protocol{TCP}, Port: port})
		if err := bad.Validate(); err == nil {
			t.Errorf("port %d to any destination was accepted", port)
		}
	}
}

func TestValidateCapCeiling(t *testing.T) {
	doc, _ := Compile([]Subject{testCam}, Setting{BlockCloudVideo: true}, Options{})
	doc.CapBytesPerSecond = 64 * 1024
	if err := doc.Validate(); err == nil {
		t.Fatal("a 64 KB/s cap must be rejected")
	}
}

func TestValidateRequiresControl(t *testing.T) {
	doc, _ := Compile([]Subject{testCam}, Setting{BlockCloudVideo: true}, Options{})
	doc.Allow = doc.Allow[1:]
	if err := doc.Validate(); err == nil {
		t.Fatal("allowlist without the control connection must be rejected")
	}
}

func TestValidateLocalCIDRs(t *testing.T) {
	doc, _ := Compile([]Subject{testCam}, Setting{BlockCloudVideo: true}, Options{})
	for _, c := range []string{"8.8.8.0/24", "fd00::/8", "0.0.0.0/0"} {
		d := *doc
		d.LocalCIDRs = []string{c}
		if err := d.Validate(); err == nil {
			t.Errorf("local CIDR %s accepted", c)
		}
	}
}

func TestSanitizeName(t *testing.T) {
	cases := map[string]string{
		"Test Camera":         "Test Camera",
		`Mom's "cam"; rm -rf`: "Mom_s _cam__ rm -rf",
		"":                    "camera",
		"   ":                 "camera",
	}
	for in, want := range cases {
		if got := SanitizeName(in); got != want {
			t.Errorf("SanitizeName(%q) = %q, want %q", in, got, want)
		}
	}
	if got := SanitizeName(strings.Repeat("x", 100)); len(got) > 40 {
		t.Errorf("name not truncated: %d chars", len(got))
	}
}

func TestHeadlineWording(t *testing.T) {
	if !strings.Contains(Headline, "destination and rate") || !strings.Contains(Headline, "may still carry image data") {
		t.Fatal("headline must describe enforcement and residual encrypted payload risk")
	}
	if strings.Contains(strings.ToLower(Headline), "zero exposure") {
		t.Fatal("headline must not claim zero exposure")
	}
}

func TestRegionEndpoints(t *testing.T) {
	us, err := RegionEndpoints(RegionUS)
	if err != nil {
		t.Fatal(err)
	}
	if len(us.ControlHosts) != 1 || us.ControlHosts[0] != "mqtts02-us.osaio.net" {
		t.Fatalf("US control hosts = %v", us.ControlHosts)
	}
	eu, err := RegionEndpoints(RegionEU)
	if err != nil {
		t.Fatal(err)
	}
	want := "mqtts-eu.osaio.net,mqtts01-eu.osaio.net"
	if got := strings.Join(eu.ControlHosts, ","); got != want {
		t.Fatalf("EU control hosts = %s, want %s", got, want)
	}
	if eu.StreamSetupHost != "wss-eu.osaio.net" || eu.StreamSetupPort != 443 {
		t.Fatalf("EU stream setup = %s:%d", eu.StreamSetupHost, eu.StreamSetupPort)
	}
	if _, err := RegionEndpoints("cn"); err == nil {
		t.Fatal("unknown region accepted")
	}
	if r, err := ParseRegion(""); err != nil || r != DefaultRegion {
		t.Fatalf("empty region = %q, %v", r, err)
	}
	if r, err := ParseRegion("EU"); err != nil || r != RegionEU {
		t.Fatalf("EU region = %q, %v", r, err)
	}
}

func TestCompileEU(t *testing.T) {
	doc, err := Compile([]Subject{testCam}, Setting{BlockCloudVideo: true, Region: RegionEU}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if doc.Region != RegionEU {
		t.Fatalf("document region = %q", doc.Region)
	}
	ctl, ok := doc.Rule("control")
	if !ok || ctl.Host != "mqtts-eu.osaio.net" || ctl.Port != 8883 {
		t.Fatalf("control rule = %+v", ctl)
	}
	alt, ok := doc.Rule("control_2")
	if !ok || alt.Host != "mqtts01-eu.osaio.net" {
		t.Fatalf("alternate control rule = %+v", alt)
	}
	setup, ok := doc.Rule("stream_setup")
	if !ok || setup.Host != "wss-eu.osaio.net" {
		t.Fatalf("stream setup rule = %+v", setup)
	}
	if got, want := strings.Join(doc.Hosts(), ","), "mqtts-eu.osaio.net,mqtts01-eu.osaio.net,wss-eu.osaio.net"; got != want {
		t.Fatalf("hosts = %s, want %s", got, want)
	}
	// Default (empty) region keeps the US allowlist.
	def, err := Compile([]Subject{testCam}, Setting{BlockCloudVideo: true}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if def.Region != RegionUS {
		t.Fatalf("default region = %q", def.Region)
	}
	if _, ok := def.Rule("control_2"); ok {
		t.Fatal("US document has an alternate control rule")
	}
}

func TestUnknownRegionRejected(t *testing.T) {
	if _, err := Compile([]Subject{testCam}, Setting{BlockCloudVideo: true, Region: "cn"}, Options{}); err == nil {
		t.Fatal("unknown region compiled")
	}
}

func TestValidateRejectsForeignRegionHosts(t *testing.T) {
	doc, err := Compile([]Subject{testCam}, Setting{BlockCloudVideo: true}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	// A US document relabelled as EU no longer has the EU control servers.
	doc.Region = RegionEU
	if err := doc.Validate(); err == nil {
		t.Fatal("document validated with mismatched region hosts")
	}
}
