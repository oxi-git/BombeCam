// Package netstack provides low-level network foundation services, NAT masquerade,
// safe host firewall rules, local DNS sinkholing, and local NTP time delivery
// for BombeCam v4.
package netstack

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
)

// WAN Return-Path NAT/Masquerade Configuration.
// Configures netfilter masquerade on outbound WAN interface
// for the camera subnet so that return traffic reaches the camera, and enables
// kernel ip_forward.

// NATManager manages WAN return-path NAT masquerade and IP forwarding.
type NATManager struct {
	mu              sync.RWMutex
	CameraInterface string
	CameraSubnet    string
	WANInterface    string
	Enabled         bool
}

// NewNATManager creates a new NATManager instance.
func NewNATManager(cameraIf, cameraSubnet, wanIf string) *NATManager {
	if cameraIf == "" || cameraSubnet == "" {
		if ifaces, err := GetDefaultDiscovery().ListInterfaces(); err == nil {
			for _, ifc := range ifaces {
				if ifc.IsUp && !ifc.IsLoopback && len(ifc.Subnets) > 0 {
					if cameraIf == "" {
						cameraIf = ifc.Name
					}
					if cameraSubnet == "" {
						cameraSubnet = ifc.Subnets[0].String()
					}
					break
				}
			}
		}
	}
	mgr := &NATManager{
		CameraInterface: cameraIf,
		CameraSubnet:    cameraSubnet,
		WANInterface:    wanIf,
		Enabled:         false,
	}
	if wanIf == "" {
		_, _ = mgr.DetectWANInterface()
	}
	return mgr
}

// DetectWANInterface dynamically detects the default outbound WAN interface.
func (m *NATManager) DetectWANInterface() (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// 1. Try reading Linux /proc/net/route
	if runtime.GOOS == "linux" {
		f, err := os.Open("/proc/net/route")
		if err == nil {
			defer f.Close()
			scanner := bufio.NewScanner(f)
			// Skip header
			if scanner.Scan() {
				for scanner.Scan() {
					fields := strings.Fields(scanner.Text())
					// Format: Iface Destination Gateway Flags ...
					if len(fields) >= 3 && fields[1] == "00000000" { // default route
						iface := fields[0]
						if iface != m.CameraInterface {
							m.WANInterface = iface
							return iface, nil
						}
					}
				}
			}
		}

		// 2. Try 'ip route show default'
		cmd := exec.Command("ip", "route", "show", "default")
		out, err := cmd.Output()
		if err == nil {
			parts := strings.Fields(string(out))
			for i, p := range parts {
				if p == "dev" && i+1 < len(parts) {
					iface := parts[i+1]
					if iface != m.CameraInterface {
						m.WANInterface = iface
						return iface, nil
					}
				}
			}
		}
	}

	// Fallback to configured WAN interface
	if m.WANInterface != "" {
		return m.WANInterface, nil
	}

	// Fallback to dynamic host interface discovery
	if ifaces, err := GetDefaultDiscovery().ListInterfaces(); err == nil {
		for _, ifc := range ifaces {
			if ifc.IsUp && !ifc.IsLoopback && ifc.Name != m.CameraInterface {
				m.WANInterface = ifc.Name
				return ifc.Name, nil
			}
		}
	}

	return "", fmt.Errorf("no default WAN interface detected")
}

// RenderNFTNATRules renders the nftables NAT masquerade table.
// WAN return-path NAT masquerade for camera subnet.
func (m *NATManager) RenderNFTNATRules() string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.renderLocked()
}

// renderLocked renders the NAT ruleset. Caller must hold m.mu.
func (m *NATManager) renderLocked() string {
	return fmt.Sprintf(`# WAN return-path NAT masquerade for the camera subnet.
#
# The match is on the OUTPUT interface. At the postrouting hook the input
# interface is NULL, so an iifname match here could never fire.
#
# Declare-then-delete makes this file a replacement, not an append.
table ip bombecam_nat
delete table ip bombecam_nat
table ip bombecam_nat {
    chain postrouting {
        type nat hook postrouting priority srcnat; policy accept;
        oifname "%s" ip saddr %s counter masquerade
    }
}
`, m.WANInterface, m.CameraSubnet)
}

// RenderIPTablesNATRules renders equivalent legacy iptables NAT rules for diagnostics.
func (m *NATManager) RenderIPTablesNATRules() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()

	return []string{
		fmt.Sprintf("iptables -t nat -A POSTROUTING -s %s -o %s -j MASQUERADE", m.CameraSubnet, m.WANInterface),
	}
}

// EnableIPForwarding enables kernel IPv4 forwarding.
func (m *NATManager) EnableIPForwarding() error {
	if runtime.GOOS == "linux" {
		// Attempt direct sysctl /proc write
		err := os.WriteFile("/proc/sys/net/ipv4/ip_forward", []byte("1\n"), 0644)
		if err != nil {
			// Fallback to sysctl command
			cmd := exec.Command("sysctl", "-w", "net.ipv4.ip_forward=1")
			if out, err2 := cmd.CombinedOutput(); err2 != nil {
				return fmt.Errorf("failed to enable ip_forward: %w (%s)", err2, strings.TrimSpace(string(out)))
			}
		}
	}
	return nil
}

// IsIPForwardingEnabled checks if kernel IPv4 forwarding is active.
func (m *NATManager) IsIPForwardingEnabled() (bool, error) {
	if runtime.GOOS == "linux" {
		data, err := os.ReadFile("/proc/sys/net/ipv4/ip_forward")
		if err != nil {
			return false, err
		}
		val := strings.TrimSpace(string(data))
		return val == "1", nil
	}
	// Simulated true on non-linux systems where kernel /proc is not available
	return true, nil
}

// ApplyNAT renders the NAT rules and applies them to the kernel.
func (m *NATManager) ApplyNAT() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if err := m.EnableIPForwarding(); err != nil {
		return fmt.Errorf("ip_forward failed: %w", err)
	}

	rules := m.renderLocked()

	if runtime.GOOS != "linux" {
		return fmt.Errorf("camera NAT is Linux-only: refusing to report NAT active on %s", runtime.GOOS)
	}
	// A missing nft is a hard failure: reporting success with no rules loaded
	// would hide a missing return path.
	if _, err := exec.LookPath("nft"); err != nil {
		return fmt.Errorf("nftables not installed: cannot establish the camera return path. " +
			"Install nftables; this is not an optional dependency")
	}
	cmd := exec.Command("nft", "-f", "-")
	cmd.Stdin = strings.NewReader(rules)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("nft apply nat failed: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	// Read the rule back. Applying is not evidence; the kernel holding it is.
	out, err := exec.Command("nft", "list", "table", "ip", "bombecam_nat").CombinedOutput()
	if err != nil {
		return fmt.Errorf("NAT readback failed after apply: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	if !strings.Contains(string(out), "masquerade") ||
		!strings.Contains(string(out), m.CameraSubnet) ||
		!strings.Contains(string(out), m.WANInterface) {
		return fmt.Errorf("NAT readback did not show the expected masquerade rule for %s via %s; "+
			"refusing to report the return path as active:\n%s", m.CameraSubnet, m.WANInterface, out)
	}

	m.Enabled = true
	return nil
}

// RemoveNAT removes the NAT table from the kernel.
func (m *NATManager) RemoveNAT() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if runtime.GOOS == "linux" {
		if _, err := exec.LookPath("nft"); err == nil {
			cmd := exec.Command("nft", "delete", "table", "ip", "bombecam_nat")
			_ = cmd.Run() // Ignore if table does not exist
		}
	}

	m.Enabled = false
	return nil
}

// Status returns current NAT configuration status.
type NATStatus struct {
	Enabled            bool   `json:"enabled"`
	CameraInterface    string `json:"camera_interface"`
	CameraSubnet       string `json:"camera_subnet"`
	WANInterface       string `json:"wan_interface"`
	IPForwardingActive bool   `json:"ip_forwarding_active"`
}

// GetStatus returns the structured status of the NAT manager.
func (m *NATManager) GetStatus() NATStatus {
	m.mu.RLock()
	defer m.mu.RUnlock()

	ipf, _ := m.IsIPForwardingEnabled()
	return NATStatus{
		Enabled:            m.Enabled,
		CameraInterface:    m.CameraInterface,
		CameraSubnet:       m.CameraSubnet,
		WANInterface:       m.WANInterface,
		IPForwardingActive: ipf,
	}
}

func ValidateCameraSubnet(cidr string) error {
	_, _, err := net.ParseCIDR(cidr)
	if err != nil {
		return fmt.Errorf("invalid camera subnet CIDR %q: %w", cidr, err)
	}
	return nil
}
