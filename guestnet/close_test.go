// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package guestnet

import (
	"net"
	"net/netip"
	"testing"
	"time"

	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/checksum"
	"gvisor.dev/gvisor/pkg/tcpip/header"
)

// buildTCPSYNFrame assembles an Ethernet/IPv4/TCP SYN frame with checksums.
func buildTCPSYNFrame(srcMAC, dstMAC net.HardwareAddr, src, dst netip.AddrPort) []byte {
	ipLen := header.IPv4MinimumSize + header.TCPMinimumSize
	frame := make([]byte, header.EthernetMinimumSize+ipLen)
	header.Ethernet(frame).Encode(&header.EthernetFields{
		SrcAddr: tcpip.LinkAddress(srcMAC),
		DstAddr: tcpip.LinkAddress(dstMAC),
		Type:    header.IPv4ProtocolNumber,
	})
	srcIP, dstIP := tcpip.AddrFrom4(src.Addr().As4()), tcpip.AddrFrom4(dst.Addr().As4())
	ip := header.IPv4(frame[header.EthernetMinimumSize:])
	ip.Encode(&header.IPv4Fields{
		TotalLength: uint16(ipLen),
		TTL:         64,
		Protocol:    uint8(header.TCPProtocolNumber),
		SrcAddr:     srcIP,
		DstAddr:     dstIP,
	})
	ip.SetChecksum(^ip.CalculateChecksum())
	tcp := header.TCP(ip.Payload())
	tcp.Encode(&header.TCPFields{
		SrcPort:    src.Port(),
		DstPort:    dst.Port(),
		SeqNum:     1000,
		DataOffset: header.TCPMinimumSize,
		Flags:      header.TCPFlagSyn,
		WindowSize: 65535,
	})
	xsum := header.PseudoHeaderChecksum(header.TCPProtocolNumber, srcIP, dstIP, header.TCPMinimumSize)
	tcp.SetChecksum(^checksum.Checksum(tcp, xsum))
	return frame
}

// TestCloseDuringHandshake checks that Close doesn't wait for a TCP
// handshake the guest will never finish. A guest that is stopped just after
// sending a SYN (the VM stopping mid-connection) leaves the forwarder waiting
// in the handshake; Close used to wait for gVisor's handshake timeout (30s
// or more) before tearing down.
func TestCloseDuringHandshake(t *testing.T) {
	target, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		c, err := target.Accept()
		if err == nil {
			accepted <- c
		}
	}()

	host, peer := NewMemLinkPair()
	n, err := New(Config{Link: host, Dialer: anyDialer{tcpTarget: target.Addr().String()}, Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	// Answer ARP for the guest's address, as a real guest would, so the
	// SYN-ACK goes out, but never ACK it: the guest has stopped.
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
			req := header.ARP(frame[header.EthernetMinimumSize:])
			if !req.IsValid() || req.Op() != header.ARPRequest {
				continue
			}
			reply := make([]byte, header.EthernetMinimumSize+header.ARPSize)
			header.Ethernet(reply).Encode(&header.EthernetFields{
				SrcAddr: tcpip.LinkAddress(DefaultGuestMAC),
				DstAddr: tcpip.LinkAddress(req.HardwareAddressSender()),
				Type:    header.ARPProtocolNumber,
			})
			arp := header.ARP(reply[header.EthernetMinimumSize:])
			arp.SetIPv4OverEthernet()
			arp.SetOp(header.ARPReply)
			copy(arp.HardwareAddressSender(), DefaultGuestMAC)
			copy(arp.ProtocolAddressSender(), req.ProtocolAddressTarget())
			copy(arp.HardwareAddressTarget(), req.HardwareAddressSender())
			copy(arp.ProtocolAddressTarget(), req.ProtocolAddressSender())
			peer.WriteFrame(reply)
		}
	}()

	src := netip.AddrPortFrom(DefaultGuestIP, 40000)
	dst := netip.MustParseAddrPort("198.51.100.7:443")
	if err := peer.WriteFrame(buildTCPSYNFrame(DefaultGuestMAC, DefaultGatewayMAC, src, dst)); err != nil {
		t.Fatal(err)
	}
	select {
	case c := <-accepted:
		defer c.Close()
	case <-time.After(5 * time.Second):
		t.Fatal("egress dial never happened")
	}
	time.Sleep(100 * time.Millisecond) // let the forwarder enter the handshake

	done := make(chan struct{})
	t0 := time.Now()
	go func() {
		n.Close()
		close(done)
	}()
	select {
	case <-done:
		t.Logf("Close took %v", time.Since(t0))
	case <-time.After(5 * time.Second):
		t.Fatal("Close waited for the guest's unfinished TCP handshake")
	}
}
