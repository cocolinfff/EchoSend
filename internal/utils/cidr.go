// Package utils provides shared utility helpers for the EchoSend daemon.
package utils

import (
	"encoding/binary"
	"fmt"
	"net"
	"strings"
)

// ParseTarget accepts either a single IPv4 address ("192.168.1.100") or a
// CIDR block ("10.0.0.0/24") and returns the list of usable host addresses.
//
//   - For a bare IP the returned slice contains exactly that one address.
//   - For a CIDR block the network address and broadcast address are excluded,
//     so a /24 returns 254 addresses, a /30 returns 2, and a /32 returns 1.
//   - IPv6 addresses are accepted but returned as-is (no expansion).
func ParseTarget(target string) ([]string, error) {
	target = strings.TrimSpace(target)
	if target == "" {
		return nil, fmt.Errorf("cidr: empty target")
	}

	// Distinguish CIDR notation from a bare address by looking for a slash.
	if !strings.Contains(target, "/") {
		// Validate that it is a well-formed IP address.
		if ip := net.ParseIP(target); ip == nil {
			return nil, fmt.Errorf("cidr: %q is not a valid IP address", target)
		}
		return []string{target}, nil
	}

	// Parse the CIDR block.
	_, ipNet, err := net.ParseCIDR(target)
	if err != nil {
		return nil, fmt.Errorf("cidr: parse %q: %w", target, err)
	}

	// Only expand IPv4 CIDR ranges; for IPv6 just return the network address.
	if ipNet.IP.To4() == nil {
		return []string{ipNet.IP.String()}, nil
	}

	return expandIPv4CIDR(ipNet), nil
}

// expandIPv4CIDR enumerates all usable host addresses inside an IPv4 network.
// It excludes the network address (all-zeros host part) and broadcast address
// (all-ones host part) for prefix lengths shorter than /31.
// /31 and /32 are treated per RFC 3021: both addresses are returned for /31,
// and the single address is returned for /32.
func expandIPv4CIDR(ipNet *net.IPNet) []string {
	// Convert the base network address to a uint32 for arithmetic.
	ip4 := ipNet.IP.To4()
	base := binary.BigEndian.Uint32(ip4)

	ones, bits := ipNet.Mask.Size()
	totalHosts := uint32(1) << uint(bits-ones) // 2^(32-prefixLen)

	// Special cases: /32 and /31
	if ones == 32 {
		return []string{ip4.String()}
	}
	if ones == 31 {
		// RFC 3021: point-to-point links – both addresses are valid hosts.
		return []string{
			uint32ToIP(base).String(),
			uint32ToIP(base + 1).String(),
		}
	}

	// General case: exclude network (base) and broadcast (base+totalHosts-1).
	hosts := make([]string, 0, totalHosts-2)
	for i := uint32(1); i < totalHosts-1; i++ {
		hosts = append(hosts, uint32ToIP(base+i).String())
	}
	return hosts
}

// uint32ToIP converts a uint32 back into a net.IP (4-byte form).
func uint32ToIP(n uint32) net.IP {
	ip := make(net.IP, 4)
	binary.BigEndian.PutUint32(ip, n)
	return ip
}

// IsPrivateIP reports whether ip falls within one of the RFC 1918 / RFC 4193
// private address ranges.  Useful for filtering probe targets to stay within
// the LAN.
func IsPrivateIP(ipStr string) bool {
	ip := net.ParseIP(ipStr)
	if ip == nil {
		return false
	}

	privateRanges := []string{
		"10.0.0.0/8",
		"172.16.0.0/12",
		"192.168.0.0/16",
		"fc00::/7", // IPv6 unique-local
	}

	for _, cidr := range privateRanges {
		_, block, err := net.ParseCIDR(cidr)
		if err != nil {
			continue
		}
		if block.Contains(ip) {
			return true
		}
	}
	return false
}

// LocalBroadcastAddresses returns the subnet broadcast addresses for all
// active non-loopback IPv4 interfaces on this machine.  The global broadcast
// address 255.255.255.255 is always included in the result.
func LocalBroadcastAddresses() ([]string, error) {
	addrs := []string{"255.255.255.255"}

	ifaces, err := net.Interfaces()
	if err != nil {
		return addrs, fmt.Errorf("cidr: list interfaces: %w", err)
	}

	for _, iface := range ifaces {
		// Skip loopback, down, and point-to-point interfaces.
		if iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		if iface.Flags&net.FlagUp == 0 {
			continue
		}

		ifAddrs, err := iface.Addrs()
		if err != nil {
			continue
		}

		for _, addr := range ifAddrs {
			var ipNet *net.IPNet
			switch v := addr.(type) {
			case *net.IPNet:
				ipNet = v
			case *net.IPAddr:
				ipNet = &net.IPNet{IP: v.IP, Mask: v.IP.DefaultMask()}
			}
			if ipNet == nil {
				continue
			}

			ip4 := ipNet.IP.To4()
			if ip4 == nil {
				continue // skip IPv6
			}

			// Compute broadcast: network address OR (NOT mask).
			mask := ipNet.Mask
			if len(mask) == 16 {
				mask = mask[12:] // trim IPv6-mapped prefix
			}
			bcast := make(net.IP, 4)
			for i := 0; i < 4; i++ {
				bcast[i] = ip4[i] | ^mask[i]
			}

			bcastStr := bcast.String()
			// Avoid duplicates.
			dup := false
			for _, existing := range addrs {
				if existing == bcastStr {
					dup = true
					break
				}
			}
			if !dup {
				addrs = append(addrs, bcastStr)
			}
		}
	}

	return addrs, nil
}

// LocalIPv4Addresses returns all non-loopback IPv4 addresses assigned to
// active interfaces on this machine.  Used by the UDP listener to learn the
// local IP for embedding in PRESENCE packets.
func LocalIPv4Addresses() ([]string, error) {
	var result []string

	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, fmt.Errorf("cidr: list interfaces: %w", err)
	}

	for _, iface := range ifaces {
		if iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		if iface.Flags&net.FlagUp == 0 {
			continue
		}

		ifAddrs, err := iface.Addrs()
		if err != nil {
			continue
		}

		for _, addr := range ifAddrs {
			var ip net.IP
			switch v := addr.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}
			if ip == nil {
				continue
			}
			if ip4 := ip.To4(); ip4 != nil {
				result = append(result, ip4.String())
			}
		}
	}

	return result, nil
}
