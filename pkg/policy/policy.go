package policy

import (
	"errors"
	"fmt"
	"net"
	"regexp"
	"sort"
	"strings"
)

// Version of the policy format. Bumped whenever the allowlist semantics change.
const Version = "1.1.0"

// Region selects which Osaio cloud endpoints the allowlist trusts. A camera
// enrolled in the EU talks to different hosts than one enrolled in the US, so
// the allowlist has to match the account region or the camera loses its
// control channel (and BombeCam loses local stream setup).
type Region string

const (
	RegionUS Region = "us"
	RegionEU Region = "eu"

	// DefaultRegion keeps existing installs, and any document or setting that
	// carries no region, on the allowlist the feature was first built for.
	DefaultRegion Region = RegionUS
)

// Endpoints are the Osaio hosts a camera may still reach in one region.
type Endpoints struct {
	// ControlHosts are the MQTT/TLS control servers. A region may publish
	// more than one name and the camera picks one, so every name is allowed.
	ControlHosts    []string
	ControlPort     int
	StreamSetupHost string
	StreamSetupPort int
}

// regionEndpoints is the single source of truth for the allowlist per region.
// The values are checked against the router script by a test.
var regionEndpoints = map[Region]Endpoints{
	RegionUS: {
		ControlHosts:    []string{"mqtts02-us.osaio.net"},
		ControlPort:     8883,
		StreamSetupHost: "wss-us.osaio.net",
		StreamSetupPort: 443,
	},
	// Observed on a WS03 enrolled in the EU: mqtts-eu and mqtts01-eu are two
	// load-balanced control names, wss-eu is the stream-setup server.
	RegionEU: {
		ControlHosts:    []string{"mqtts-eu.osaio.net", "mqtts01-eu.osaio.net"},
		ControlPort:     8883,
		StreamSetupHost: "wss-eu.osaio.net",
		StreamSetupPort: 443,
	},
}

// ParseRegion accepts "", "us" or "eu" in any case. Empty means the default.
func ParseRegion(s string) (Region, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "":
		return DefaultRegion, nil
	case string(RegionUS):
		return RegionUS, nil
	case string(RegionEU):
		return RegionEU, nil
	default:
		return "", fmt.Errorf("unknown region %q (want %q or %q)", s, RegionUS, RegionEU)
	}
}

// OrDefault maps the empty region to DefaultRegion.
func (r Region) OrDefault() Region {
	if r == "" {
		return DefaultRegion
	}
	return r
}

// String returns the canonical region name.
func (r Region) String() string { return string(r.OrDefault()) }

// Endpoints returns the Osaio hosts trusted in the region.
func (r Region) Endpoints() (Endpoints, error) {
	key := r.OrDefault()
	eps, ok := regionEndpoints[key]
	if !ok {
		return Endpoints{}, fmt.Errorf("unknown region %q", key)
	}
	return eps, nil
}

// RegionEndpoints exposes the endpoint table (tests and CLIs).
func RegionEndpoints(r Region) (Endpoints, error) { return r.Endpoints() }

// Observed Osaio endpoints (WS03, US region), kept as aliases for the code and
// tests that predate the region table.
const (
	// ControlHost carries the camera's control channel: one long-lived TLS
	// (MQTT) connection, observed at roughly 19 KB over 77 seconds.
	ControlHost = "mqtts02-us.osaio.net"
	ControlPort = 8883

	// StreamSetupHost is Osaio's WebRTC signaling server. Each new live
	// session, including BombeCam's own local ones, is introduced through it.
	// It was observed carrying connection setup (a few KB). The rules
	// cannot inspect encrypted contents or guarantee that an allowed endpoint
	// never carries image data. Allowed by default so BombeCam can keep starting local streams;
	// Options.BlockStreamSetup removes it.
	StreamSetupHost = "wss-us.osaio.net"
	StreamSetupPort = 443
)

// The per-camera cap. Everything a camera may still send to the internet
// shares one bucket per camera. This reduces typical cloud streaming; it
// cannot rule out images, fragments or unusually low-rate video.
const (
	CapBytesPerSecond = 4096
	CapBurstBytes     = 8192

	// MaxCapBytesPerSecond is the invariant ceiling Validate enforces, so a
	// future edit cannot quietly raise the cap into video territory.
	MaxCapBytesPerSecond = 16384
)

// Headline is the one-sentence description shown next to the setting. It is
// deliberately limited to what the rules actually guarantee.
const Headline = "Camera internet access is limited by destination and rate; encrypted traffic may still carry image data."

// DefaultLocalCIDRs are private IPv4 ranges treated as "your network".
var DefaultLocalCIDRs = []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16"}

// Setting is the single user-facing choice.
type Setting struct {
	BlockCloudVideo bool `json:"block_cloud_video"`
	// Region picks the Osaio endpoints the allowlist trusts. Empty means
	// DefaultRegion (US).
	Region Region `json:"region,omitempty"`
}

// Options are advanced knobs that are not exposed as a second UI choice.
type Options struct {
	// BlockStreamSetup also blocks Osaio's WebRTC signaling server. This is
	// narrower, but it will likely stop BombeCam from starting new local
	// streams while cloud video is blocked. Intended for testing whether the
	// camera really needs it.
	BlockStreamSetup bool
}

// Compile builds the policy document for the given cameras and setting.
func Compile(subjects []Subject, s Setting, opts Options) (*Document, error) {
	if len(subjects) == 0 {
		return nil, errors.New("at least one camera must be specified")
	}
	subs := make([]Subject, 0, len(subjects))
	seen := map[string]string{}
	for _, sub := range subjects {
		if err := sub.Validate(); err != nil {
			return nil, fmt.Errorf("invalid camera %q: %w", sub.ID, err)
		}
		sub.MAC = CanonicalMAC(sub.MAC)
		if prev, dup := seen[sub.MAC]; dup {
			return nil, fmt.Errorf("cameras %q and %q have the same MAC %s", prev, sub.ID, sub.MAC)
		}
		seen[sub.MAC] = sub.ID
		subs = append(subs, sub)
	}
	sort.SliceStable(subs, func(i, j int) bool { return subs[i].MAC < subs[j].MAC })

	region := s.Region.OrDefault()
	eps, err := region.Endpoints()
	if err != nil {
		return nil, err
	}

	doc := &Document{
		Version:           Version,
		Region:            region,
		BlockCloudVideo:   s.BlockCloudVideo,
		Subjects:          subs,
		LocalCIDRs:        append([]string(nil), DefaultLocalCIDRs...),
		CapBytesPerSecond: CapBytesPerSecond,
		CapBurstBytes:     CapBurstBytes,
	}
	if !s.BlockCloudVideo {
		return doc, doc.Validate()
	}

	for i, host := range eps.ControlHosts {
		id := "control"
		purpose := "Osaio app controls (LED, pan/tilt, night vision) over the camera's encrypted control connection"
		if i > 0 {
			id = fmt.Sprintf("control_%d", i+1)
			purpose = "Osaio app controls (alternate control server for this region)"
		}
		doc.Allow = append(doc.Allow, AllowRule{
			ID:        id,
			Purpose:   purpose,
			Protocols: []Protocol{TCP},
			Port:      eps.ControlPort,
			Host:      host,
		})
	}
	if !opts.BlockStreamSetup {
		doc.Allow = append(doc.Allow, AllowRule{
			ID:        "stream_setup",
			Purpose:   "Stream-setup endpoint used when a local stream starts; encrypted payload content is not inspected",
			Protocols: []Protocol{TCP},
			Port:      eps.StreamSetupPort,
			Host:      eps.StreamSetupHost,
		})
	}
	doc.Allow = append(doc.Allow,
		AllowRule{
			ID:        "dns",
			Purpose:   "Name lookups (the camera uses public DNS such as 8.8.8.8 directly)",
			Protocols: []Protocol{UDP, TCP},
			Port:      53,
		},
		AllowRule{
			ID:        "time",
			Purpose:   "Clock sync (NTP)",
			Protocols: []Protocol{UDP},
			Port:      123,
		},
	)
	return doc, doc.Validate()
}

var hostRe = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z]{2,63}$`)

// Validate enforces the invariants every renderer relies on.
func (d *Document) Validate() error {
	if d == nil {
		return errors.New("nil policy document")
	}
	if len(d.Subjects) == 0 {
		return errors.New("policy must contain at least one camera")
	}
	for i, s := range d.Subjects {
		if err := s.Validate(); err != nil {
			return fmt.Errorf("camera[%d]: %w", i, err)
		}
	}
	for _, c := range d.LocalCIDRs {
		ip, n, err := net.ParseCIDR(c)
		if err != nil {
			return fmt.Errorf("invalid local CIDR %q: %w", c, err)
		}
		if ip.To4() == nil {
			return fmt.Errorf("local CIDR %q is IPv6: camera IPv6 is always blocked", c)
		}
		if !ip.IsPrivate() {
			return fmt.Errorf("local CIDR %q is not a private range", c)
		}
		if ones, _ := n.Mask.Size(); ones < 8 {
			return fmt.Errorf("local CIDR %q is too broad", c)
		}
	}
	if !d.BlockCloudVideo {
		if len(d.Allow) != 0 {
			return errors.New("allow rules present while cloud video is not blocked")
		}
		return nil
	}
	if d.CapBytesPerSecond <= 0 || d.CapBytesPerSecond > MaxCapBytesPerSecond {
		return fmt.Errorf("cap %d B/s outside (0, %d]: the cap must stay far below what video needs",
			d.CapBytesPerSecond, MaxCapBytesPerSecond)
	}
	if d.CapBurstBytes < d.CapBytesPerSecond || d.CapBurstBytes > 4*MaxCapBytesPerSecond {
		return fmt.Errorf("burst %d bytes out of range", d.CapBurstBytes)
	}
	eps, err := d.Region.Endpoints()
	if err != nil {
		return err
	}
	hasControl := map[string]bool{}
	ids := map[string]bool{}
	for _, r := range d.Allow {
		if r.ID == "" || ids[r.ID] {
			return fmt.Errorf("allow rule has empty or duplicate id %q", r.ID)
		}
		ids[r.ID] = true
		if r.Port <= 0 || r.Port > 65535 {
			return fmt.Errorf("allow rule %q: port %d out of range", r.ID, r.Port)
		}
		if len(r.Protocols) == 0 {
			return fmt.Errorf("allow rule %q has no protocol", r.ID)
		}
		for _, p := range r.Protocols {
			if p != TCP && p != UDP {
				return fmt.Errorf("allow rule %q: unsupported protocol %q", r.ID, p)
			}
		}
		if r.Host != "" && !hostRe.MatchString(r.Host) {
			return fmt.Errorf("allow rule %q: invalid host %q", r.ID, r.Host)
		}
		// A web/TLS port open to every destination is how the old Tier 1
		// quietly allowed cloud uploads. Only DNS and NTP may be host-less.
		if r.Host == "" && r.Port != 53 && r.Port != 123 {
			return fmt.Errorf("allow rule %q opens port %d to any destination; only DNS (53) and NTP (123) may", r.ID, r.Port)
		}
		if r.ID == "control" || strings.HasPrefix(r.ID, "control_") {
			if !containsString(eps.ControlHosts, r.Host) {
				return fmt.Errorf("allow rule %q: %q is not a %s control server (want %s)",
					r.ID, r.Host, d.Region.OrDefault(), strings.Join(eps.ControlHosts, ", "))
			}
			if r.Port != eps.ControlPort {
				return fmt.Errorf("allow rule %q: control port %d, want %d", r.ID, r.Port, eps.ControlPort)
			}
			hasControl[r.Host] = true
		}
		if r.ID == "stream_setup" && (r.Host != eps.StreamSetupHost || r.Port != eps.StreamSetupPort) {
			return fmt.Errorf("allow rule %q: must be %s:%d", r.ID, eps.StreamSetupHost, eps.StreamSetupPort)
		}
	}
	for _, h := range eps.ControlHosts {
		if !hasControl[h] {
			return fmt.Errorf("allowlist must include the control connection %s:%d", h, eps.ControlPort)
		}
	}
	return nil
}

// containsString reports whether s is in list.
func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// SanitizeName reduces a camera name to characters safe inside router rule
// comments and shell arguments. It never returns an empty string.
func SanitizeName(name string) string {
	var b strings.Builder
	for _, r := range strings.TrimSpace(name) {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == ' ', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
		if b.Len() >= 40 {
			break
		}
	}
	out := strings.TrimSpace(b.String())
	if out == "" {
		return "camera"
	}
	return out
}
