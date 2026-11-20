// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package guestnet

import (
	"fmt"
	"net"
	"net/netip"
	"slices"
)

// DenyLocal returns base plus a single-address prefix for each of the host's
// own interface addresses. Egress is dialed from the host, so without these a
// guest could reach any host listener bound to a wildcard or non-loopback
// address (for example, a service on the host's LAN or VPC address).
//
// The addresses are snapshotted; addresses added to the host later are not
// covered.
func DenyLocal(base []netip.Prefix) ([]netip.Prefix, error) {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil, fmt.Errorf("guestnet: listing interface addresses: %w", err)
	}
	deny := slices.Clone(base)
	for _, a := range addrs {
		ipn, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		ip, ok := netip.AddrFromSlice(ipn.IP)
		if !ok {
			continue
		}
		ip = ip.Unmap()
		deny = append(deny, netip.PrefixFrom(ip, ip.BitLen()))
	}
	return deny, nil
}
