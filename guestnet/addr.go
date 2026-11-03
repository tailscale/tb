// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package guestnet

import (
	"net"
	"net/netip"
)

var netipBroadcast = netip.AddrFrom4([4]byte{255, 255, 255, 255})

func netipFromIP(ip net.IP) (netip.Addr, bool) {
	if ip4 := ip.To4(); ip4 != nil {
		return netip.AddrFrom4([4]byte(ip4)), true
	}
	return netip.AddrFromSlice(ip)
}

// broadcastOf returns the directed broadcast address of an IPv4 prefix.
func broadcastOf(p netip.Prefix) net.IP {
	a := p.Masked().Addr().As4()
	bits := p.Bits()
	for i := bits; i < 32; i++ {
		a[i/8] |= 1 << (7 - uint(i%8))
	}
	return net.IP(a[:])
}
