package netstack

import (
	"fmt"
	"net"
	"strings"
	"sync"
)

// NetworkInterface represents an inspected host network interface.
type NetworkInterface struct {
	Name         string       `json:"name"`
	HardwareAddr string       `json:"hardware_addr"`
	IPs          []net.IP     `json:"ips"`
	Subnets      []*net.IPNet `json:"subnets"`
	IsUp         bool         `json:"is_up"`
	IsLoopback   bool         `json:"is_loopback"`
}

// NetworkDiscovery provides dynamic host network inspection.
type NetworkDiscovery interface {
	// ListInterfaces returns all active non-loopback network interfaces with their subnets.
	ListInterfaces() ([]NetworkInterface, error)

	// DetectHostSubnets returns all IPv4 subnets assigned to active host interfaces.
	DetectHostSubnets() ([]*net.IPNet, error)

	// FindInterfaceForIP determines which local interface a destination/source IP belongs to.
	FindInterfaceForIP(ip net.IP) (*NetworkInterface, *net.IPNet, error)

	// IsIPInHostSubnets checks if an IP belongs to any locally configured host subnet.
	IsIPInHostSubnets(ip net.IP) bool
}

// HostDiscovery is the default implementation of NetworkDiscovery using the host OS network stack.
type HostDiscovery struct{}

// NewHostDiscovery creates a new HostDiscovery instance.
func NewHostDiscovery() *HostDiscovery {
	return &HostDiscovery{}
}

// ListInterfaces inspects host network interfaces and returns non-loopback interfaces.
func (d *HostDiscovery) ListInterfaces() ([]NetworkInterface, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, fmt.Errorf("failed to list network interfaces: %w", err)
	}

	var result []NetworkInterface
	for _, iface := range ifaces {
		isUp := (iface.Flags & net.FlagUp) != 0
		isLoopback := (iface.Flags & net.FlagLoopback) != 0

		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}

		var ips []net.IP
		var subnets []*net.IPNet

		for _, addr := range addrs {
			var ip net.IP
			var ipNet *net.IPNet

			switch v := addr.(type) {
			case *net.IPNet:
				ip = v.IP
				ipNet = v
			case *net.IPAddr:
				ip = v.IP
			}

			if ip != nil {
				// Only focus on IPv4 for camera subnets
				if v4 := ip.To4(); v4 != nil {
					ips = append(ips, v4)
					if ipNet != nil {
						subnets = append(subnets, ipNet)
					}
				}
			}
		}

		result = append(result, NetworkInterface{
			Name:         iface.Name,
			HardwareAddr: iface.HardwareAddr.String(),
			IPs:          ips,
			Subnets:      subnets,
			IsUp:         isUp,
			IsLoopback:   isLoopback,
		})
	}

	return result, nil
}

// DetectHostSubnets returns all active, non-loopback IPv4 subnets assigned to the host.
func (d *HostDiscovery) DetectHostSubnets() ([]*net.IPNet, error) {
	ifaces, err := d.ListInterfaces()
	if err != nil {
		return nil, err
	}

	var subnets []*net.IPNet
	seen := make(map[string]bool)

	for _, iface := range ifaces {
		if !iface.IsUp || iface.IsLoopback {
			continue
		}
		for _, subnet := range iface.Subnets {
			cidr := subnet.String()
			if !seen[cidr] {
				seen[cidr] = true
				subnets = append(subnets, subnet)
			}
		}
	}

	return subnets, nil
}

// FindInterfaceForIP finds which host interface and subnet covers the given IP.
func (d *HostDiscovery) FindInterfaceForIP(ip net.IP) (*NetworkInterface, *net.IPNet, error) {
	if ip == nil {
		return nil, nil, fmt.Errorf("ip cannot be nil")
	}
	v4 := ip.To4()
	if v4 == nil {
		return nil, nil, fmt.Errorf("only IPv4 addresses supported: %s", ip)
	}

	ifaces, err := d.ListInterfaces()
	if err != nil {
		return nil, nil, err
	}

	for _, iface := range ifaces {
		if !iface.IsUp {
			continue
		}
		for _, subnet := range iface.Subnets {
			if subnet.Contains(v4) {
				return &iface, subnet, nil
			}
		}
	}

	return nil, nil, fmt.Errorf("no host interface found containing IP %s", ip)
}

// IsIPInHostSubnets returns true if the IP is contained in any active host subnet.
func (d *HostDiscovery) IsIPInHostSubnets(ip net.IP) bool {
	if ip == nil {
		return false
	}
	v4 := ip.To4()
	if v4 == nil {
		return false
	}

	subnets, err := d.DetectHostSubnets()
	if err != nil {
		return false
	}

	for _, subnet := range subnets {
		if subnet.Contains(v4) {
			return true
		}
	}
	return false
}

// MockDiscovery provides an in-memory NetworkDiscovery implementation for tests and simulations.
type MockDiscovery struct {
	mu         sync.RWMutex
	Interfaces []NetworkInterface
}

// NewMockDiscovery creates a new MockDiscovery with predefined interfaces.
func NewMockDiscovery(ifaces []NetworkInterface) *MockDiscovery {
	return &MockDiscovery{
		Interfaces: ifaces,
	}
}

// ListInterfaces returns mock interfaces.
func (m *MockDiscovery) ListInterfaces() ([]NetworkInterface, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	res := make([]NetworkInterface, len(m.Interfaces))
	copy(res, m.Interfaces)
	return res, nil
}

// DetectHostSubnets returns subnets from active non-loopback mock interfaces.
func (m *MockDiscovery) DetectHostSubnets() ([]*net.IPNet, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var subnets []*net.IPNet
	seen := make(map[string]bool)

	for _, iface := range m.Interfaces {
		if !iface.IsUp || iface.IsLoopback {
			continue
		}
		for _, subnet := range iface.Subnets {
			cidr := subnet.String()
			if !seen[cidr] {
				seen[cidr] = true
				subnets = append(subnets, subnet)
			}
		}
	}
	return subnets, nil
}

// FindInterfaceForIP finds a mock interface containing ip.
func (m *MockDiscovery) FindInterfaceForIP(ip net.IP) (*NetworkInterface, *net.IPNet, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	v4 := ip.To4()
	if v4 == nil {
		return nil, nil, fmt.Errorf("invalid IPv4: %s", ip)
	}

	for _, iface := range m.Interfaces {
		if !iface.IsUp {
			continue
		}
		for _, subnet := range iface.Subnets {
			if subnet.Contains(v4) {
				cp := iface
				return &cp, subnet, nil
			}
		}
	}
	return nil, nil, fmt.Errorf("no mock interface for IP %s", ip)
}

func (m *MockDiscovery) IsIPInHostSubnets(ip net.IP) bool {
	subnets, err := m.DetectHostSubnets()
	if err != nil {
		return false
	}
	v4 := ip.To4()
	if v4 == nil {
		return false
	}
	for _, subnet := range subnets {
		if subnet.Contains(v4) {
			return true
		}
	}
	return false
}

// Global default discovery
var (
	defaultDiscoveryMu sync.RWMutex
	defaultDiscovery   NetworkDiscovery = NewHostDiscovery()
)

// SetDefaultDiscovery overrides the default discovery provider (useful for tests).
func SetDefaultDiscovery(d NetworkDiscovery) {
	defaultDiscoveryMu.Lock()
	defer defaultDiscoveryMu.Unlock()
	defaultDiscovery = d
}

// GetDefaultDiscovery returns the current default discovery provider.
func GetDefaultDiscovery() NetworkDiscovery {
	defaultDiscoveryMu.RLock()
	defer defaultDiscoveryMu.RUnlock()
	return defaultDiscovery
}

// DetectHostSubnets returns active IPv4 subnets using the default discovery provider.
func DetectHostSubnets() ([]*net.IPNet, error) {
	return GetDefaultDiscovery().DetectHostSubnets()
}

// IsIPInHostSubnets checks if ip belongs to any active host subnet using the default discovery provider.
func IsIPInHostSubnets(ip net.IP) bool {
	return GetDefaultDiscovery().IsIPInHostSubnets(ip)
}

// FindInterfaceForIP finds the host interface for an IP using the default discovery provider.
func FindInterfaceForIP(ip net.IP) (*NetworkInterface, *net.IPNet, error) {
	return GetDefaultDiscovery().FindInterfaceForIP(ip)
}

// ParseCIDRs parses a slice of CIDR string notations into *net.IPNet slice.
func ParseCIDRs(cidrs []string) ([]*net.IPNet, error) {
	var result []*net.IPNet
	for _, c := range cidrs {
		c = strings.TrimSpace(c)
		if c == "" {
			continue
		}
		_, ipNet, err := net.ParseCIDR(c)
		if err != nil {
			return nil, fmt.Errorf("invalid CIDR %q: %w", c, err)
		}
		result = append(result, ipNet)
	}
	return result, nil
}
