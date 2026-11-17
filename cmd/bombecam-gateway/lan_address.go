package main

import (
	"net"
	"net/http"
	"sort"
	"strings"
)

// LANAddress is one IPv4 address of this PC that other devices may use to
// reach its streams.
type LANAddress struct {
	IP        string `json:"ip"`
	Interface string `json:"interface"`
	// Default marks the address of the default route (the one Windows uses
	// for the internet), which BombeCam picks when the user hasn't chosen.
	Default bool `json:"default"`
}

// Seams for tests: the addresses of this machine and its default-route address.
var (
	lanAddressesFunc = listLANAddresses
	primaryIPFunc    = primaryLANIPv4
)

var cgnat = func() *net.IPNet { _, n, _ := net.ParseCIDR("100.64.0.0/10"); return n }()

// usableLANIP reports whether ip is an address other devices on a home or
// VPN network can use: private IPv4 (10/8, 172.16/12, 192.168/16) or the
// 100.64/10 range that VPNs such as Tailscale use.
func usableLANIP(ip net.IP) bool {
	ip4 := ip.To4()
	if ip4 == nil || ip4.IsLoopback() || ip4.IsLinkLocalUnicast() || ip4.IsUnspecified() {
		return false
	}
	return ip4.IsPrivate() || cgnat.Contains(ip4)
}

// listLANAddresses lists this machine's usable IPv4 addresses, the
// default-route one first.
func listLANAddresses() []LANAddress {
	primary := primaryIPFunc()
	var out []LANAddress
	seen := map[string]bool{}
	ifaces, _ := net.Interfaces()
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, _ := ifc.Addrs()
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok || ipn.IP.To4() == nil {
				continue
			}
			// private and VPN addresses, plus the default-route address
			// whatever its range (a PC wired straight to a modem)
			if !usableLANIP(ipn.IP) && (ipn.IP.To4().String() != primary || ipn.IP.IsLoopback()) {
				continue
			}
			ip := ipn.IP.To4().String()
			if seen[ip] {
				continue
			}
			seen[ip] = true
			out = append(out, LANAddress{IP: ip, Interface: ifc.Name, Default: ip == primary})
		}
	}
	if primary != "" && !seen[primary] && usableLANIP(net.ParseIP(primary)) {
		out = append(out, LANAddress{IP: primary, Default: true})
	}
	sortLANAddresses(out)
	return out
}

func sortLANAddresses(a []LANAddress) {
	sort.SliceStable(a, func(i, j int) bool {
		if a[i].Default != a[j].Default {
			return a[i].Default
		}
		return a[i].IP < a[j].IP
	})
}

// addressSource says where an advertised stream address came from.
const (
	addrFromChoice   = "chosen"    // picked on the Frigate / Home Assistant page
	addrFromPage     = "page"      // the address this page was opened with
	addrFromDefault  = "automatic" // the default-route address
	addrFromLoopback = "loopback"  // no network address found: this PC only
)

// isLocalOnlyHost reports whether a Host header names this PC only
// (loopback, localhost, or Go's httptest placeholder).
func isLocalOnlyHost(host string) bool {
	h := strings.ToLower(strings.TrimSpace(host))
	if stripped, _, err := net.SplitHostPort(h); err == nil {
		h = stripped
	}
	h = strings.Trim(h, "[]")
	if h == "" || h == "localhost" || h == "example.com" {
		return true
	}
	if ip := net.ParseIP(h); ip != nil {
		return ip.IsLoopback() || ip.IsUnspecified()
	}
	return false
}

// resolveAdvertisedHost picks the host name or address that other devices
// should use for this PC's streams: the user's choice when it is still one of
// this PC's addresses; else the address the page was opened with, if that
// is not a loopback name; else the default-route address; else 127.0.0.1.
func resolveAdvertisedHost(r *http.Request, chosen string) (host, source string, chosenMissing bool) {
	if chosen = strings.TrimSpace(chosen); chosen != "" {
		for _, a := range lanAddressesFunc() {
			if a.IP == chosen {
				return chosen, addrFromChoice, false
			}
		}
		chosenMissing = true
	}
	if r != nil && !isLocalOnlyHost(r.Host) {
		h := r.Host
		if stripped, _, err := net.SplitHostPort(h); err == nil {
			h = stripped
		}
		return strings.Trim(h, "[]"), addrFromPage, chosenMissing
	}
	if ip := primaryIPFunc(); ip != "" {
		return ip, addrFromDefault, chosenMissing
	}
	for _, a := range lanAddressesFunc() {
		return a.IP, addrFromDefault, chosenMissing
	}
	return "127.0.0.1", addrFromLoopback, chosenMissing
}

// hostForURL brackets IPv6 literals.
func hostForURL(h string) string {
	if strings.Contains(h, ":") && !strings.HasPrefix(h, "[") {
		return "[" + h + "]"
	}
	return h
}
