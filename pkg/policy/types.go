// Package policy is the single source of truth for BombeCam's camera firewall.
//
// The user-facing choice is "Block cloud video", made per camera (the policy
// document lists the cameras that are blocked):
//
//   - Not blocked: BombeCam installs no rules for it. The camera works normally.
//   - Blocked: the router the camera connects to applies a plain outbound
//     allowlist. The camera may still reach the Osaio control server (so the
//     Osaio app can move it, switch the LED, change night vision) and the small
//     basics it needs (DNS, time sync, and the short stream-setup handshake
//     BombeCam's own local streams rely on). Every one of those shares a
//     per-camera rate cap. Allowed encrypted traffic can still carry image
//     data; this is not content inspection. Everything else from
//     the camera to the internet — cloud clip/snapshot uploads, relay servers,
//     direct peer-to-peer video, and all IPv6 — is dropped.
//
// The policy uses only allow, deny and rate-limit decisions. It never
// redirects, rewrites or answers traffic on the camera's behalf, and it never
// touches the camera's firmware.
package policy

import (
	"errors"
	"fmt"
	"net"
	"strings"
)

// Protocol is an L4 protocol an allow rule applies to.
type Protocol string

const (
	TCP Protocol = "tcp"
	UDP Protocol = "udp"
)

// Subject is one camera, identified by its MAC address. The router matches the
// camera by MAC on any of its LAN-side interfaces, so an IP address is never
// needed (cameras get theirs from DHCP and it can change).
type Subject struct {
	ID        string `json:"id"`
	Name      string `json:"name,omitempty"`      // display name, e.g. "Front Door"
	MAC       string `json:"mac"`                 // canonical "aa:bb:cc:dd:ee:ff"
	Interface string `json:"interface,omitempty"` // optional ingress interface restriction
}

// Validate checks that the subject has an ID and a real unicast MAC.
func (s Subject) Validate() error {
	if strings.TrimSpace(s.ID) == "" {
		return errors.New("subject ID must not be empty")
	}
	mac := strings.TrimSpace(s.MAC)
	if mac == "" {
		return errors.New("subject MAC must not be empty (cameras are identified by MAC address)")
	}
	hw, err := net.ParseMAC(mac)
	if err != nil || len(hw) != 6 {
		return fmt.Errorf("invalid subject MAC %q", mac)
	}
	if hw[0]&1 == 1 {
		return fmt.Errorf("subject MAC %q is a multicast/broadcast address", mac)
	}
	allZero := true
	for _, b := range hw {
		if b != 0 {
			allZero = false
		}
	}
	if allZero {
		return fmt.Errorf("subject MAC %q is all zeros", mac)
	}
	return nil
}

// CanonicalMAC returns the MAC in lower-case colon form, or "" if invalid.
func CanonicalMAC(mac string) string {
	hw, err := net.ParseMAC(strings.TrimSpace(mac))
	if err != nil || len(hw) != 6 {
		return ""
	}
	return strings.ToLower(hw.String())
}

// AllowRule is one thing a camera may still send to the internet while cloud
// video is blocked. Every allow rule is subject to the per-camera cap.
type AllowRule struct {
	ID        string     `json:"id"`      // stable identifier, e.g. "control"
	Purpose   string     `json:"purpose"` // plain-language reason
	Protocols []Protocol `json:"protocols"`
	Port      int        `json:"port"`
	// Host is the DNS name the destination must resolve to. Empty means any
	// public IPv4 destination on this port (used for DNS and time sync, whose
	// servers the camera chooses itself). IPv6 is never allowed.
	Host string `json:"host,omitempty"`
}

// Document is the compiled policy for a set of cameras.
type Document struct {
	Version string `json:"version"`
	// Region names the Osaio cloud the allowlist is built for. Empty means
	// the default region (US), so documents written before regions existed
	// keep their meaning.
	Region          Region    `json:"region,omitempty"`
	BlockCloudVideo bool      `json:"block_cloud_video"`
	Subjects        []Subject `json:"subjects"`
	// Allow is the ordered allowlist. Empty when BlockCloudVideo is false.
	Allow []AllowRule `json:"allow"`
	// LocalCIDRs are destinations inside the home network. Traffic to them is
	// left alone so the camera can still reach BombeCam and other local
	// devices even if they sit on a different subnet of the same router.
	LocalCIDRs []string `json:"local_cidrs"`
	// CapBytesPerSecond and CapBurstBytes bound everything a camera may send
	// to the internet (all allow rules share one bucket per camera).
	CapBytesPerSecond int `json:"cap_bytes_per_second"`
	CapBurstBytes     int `json:"cap_burst_bytes"`
}

// Hosts returns the distinct DNS names the allowlist depends on, in rule order.
func (d *Document) Hosts() []string {
	seen := map[string]bool{}
	var out []string
	for _, r := range d.Allow {
		if r.Host != "" && !seen[r.Host] {
			seen[r.Host] = true
			out = append(out, r.Host)
		}
	}
	return out
}

// Rule returns the allow rule with the given ID.
func (d *Document) Rule(id string) (AllowRule, bool) {
	for _, r := range d.Allow {
		if r.ID == id {
			return r, true
		}
	}
	return AllowRule{}, false
}
