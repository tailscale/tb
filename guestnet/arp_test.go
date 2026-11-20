// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package guestnet

import (
	"net"
	"net/netip"
	"testing"
	"time"

	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/header"
)

// buildARPRequest returns an Ethernet broadcast ARP request from mac/srcIP
// asking for dstIP. An ARP probe (RFC 5227) has an unspecified srcIP.
func buildARPRequest(mac net.HardwareAddr, srcIP, dstIP netip.Addr) []byte {
	frame := make([]byte, header.EthernetMinimumSize+header.ARPSize)
	header.Ethernet(frame).Encode(&header.EthernetFields{
		SrcAddr: tcpip.LinkAddress(mac),
		DstAddr: tcpip.LinkAddress("\xff\xff\xff\xff\xff\xff"),
		Type:    header.ARPProtocolNumber,
	})
	arp := header.ARP(frame[header.EthernetMinimumSize:])
	arp.SetIPv4OverEthernet()
	arp.SetOp(header.ARPRequest)
	copy(arp.HardwareAddressSender(), mac)
	src, dst := srcIP.As4(), dstIP.As4()
	copy(arp.ProtocolAddressSender(), src[:])
	copy(arp.ProtocolAddressTarget(), dst[:])
	return frame
}

// TestARPOnlyForOurAddresses checks that we answer ARP only for our own
// addresses. In particular the guest's DHCP client probes its offered
// address (RFC 5227) and declines the lease if anything answers; macOS does
// this, and a promiscuous gVisor NIC would answer for every address.
func TestARPOnlyForOurAddresses(t *testing.T) {
	host, peer := NewMemLinkPair()
	n, err := New(Config{Link: host, Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	defer n.Close()

	replies := make(chan netip.Addr, 16)
	go func() {
		buf := make([]byte, 65536)
		for {
			m, err := peer.ReadFrame(buf)
			if err != nil {
				return
			}
			frame := buf[:m]
			if header.Ethernet(frame).Type() != header.ARPProtocolNumber {
				continue
			}
			arp := header.ARP(frame[header.EthernetMinimumSize:])
			if arp.IsValid() && arp.Op() == header.ARPReply {
				replies <- netip.AddrFrom4([4]byte(arp.ProtocolAddressSender()))
			}
		}
	}()

	mac := DefaultGuestMAC
	unspec := netip.AddrFrom4([4]byte{})
	for _, tt := range []struct {
		src, dst netip.Addr
		want     bool
	}{
		{unspec, DefaultGuestIP, false},         // DHCP client's duplicate address probe
		{DefaultGuestIP, DefaultGuestIP, false}, // gratuitous ARP
		{DefaultGuestIP, netip.MustParseAddr("192.168.64.77"), false},
		{DefaultGuestIP, DefaultGatewayIP, true},
		{DefaultGuestIP, IMDSAddr, true},
		{DefaultGuestIP, DNSAddr, true},
	} {
		if err := peer.WriteFrame(buildARPRequest(mac, tt.src, tt.dst)); err != nil {
			t.Fatal(err)
		}
		var got bool
		select {
		case from := <-replies:
			got = true
			if from != tt.dst {
				t.Errorf("ARP for %v: reply claims %v", tt.dst, from)
			}
		case <-time.After(300 * time.Millisecond):
		}
		if got != tt.want {
			t.Errorf("ARP from %v for %v: replied = %v, want %v", tt.src, tt.dst, got, tt.want)
		}
	}
}
