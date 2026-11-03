// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

// Package guestnettest provides a fake guest for testing code built on
// guestnet: a gVisor stack with a static address attached to the far end of
// a frame link, standing in for a VM (as one restored from a snapshot would
// look, already holding its lease).
//
// It deliberately does not import guestnet, so guestnet's own tests can use
// it.
package guestnettest

import (
	"context"
	"net"
	"net/netip"
	"testing"

	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/link/channel"
	"gvisor.dev/gvisor/pkg/tcpip/link/ethernet"
	"gvisor.dev/gvisor/pkg/tcpip/network/arp"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/icmp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"
)

// FrameLink is the subset of guestnet.FrameLink the fake guest needs.
type FrameLink interface {
	ReadFrame(buf []byte) (int, error)
	WriteFrame(frame []byte) error
}

// Guest is a fake guest VM's network stack.
type Guest struct {
	s  *stack.Stack
	ep *channel.Endpoint
}

// Config configures a Guest.
type Config struct {
	IP      netip.Addr
	Subnet  netip.Prefix
	Gateway netip.Addr
	MAC     net.HardwareAddr
	MTU     uint32 // defaults to 1500
}

// New starts a fake guest on link. It is torn down by t.Cleanup; closing the
// link is the caller's job.
func New(t testing.TB, link FrameLink, cfg Config) *Guest {
	t.Helper()
	if cfg.MTU == 0 {
		cfg.MTU = 1500
	}
	s := stack.New(stack.Options{
		NetworkProtocols:   []stack.NetworkProtocolFactory{ipv4.NewProtocol, arp.NewProtocol},
		TransportProtocols: []stack.TransportProtocolFactory{tcp.NewProtocol, udp.NewProtocol, icmp.NewProtocol4},
	})
	ep := channel.New(1024, cfg.MTU, tcpip.LinkAddress(cfg.MAC))
	if err := s.CreateNIC(1, ethernet.New(ep)); err != nil {
		t.Fatalf("guest CreateNIC: %v", err)
	}
	if err := s.AddProtocolAddress(1, tcpip.ProtocolAddress{
		Protocol:          ipv4.ProtocolNumber,
		AddressWithPrefix: tcpip.AddressWithPrefix{Address: tcpip.AddrFrom4(cfg.IP.As4()), PrefixLen: cfg.Subnet.Bits()},
	}, stack.AddressProperties{}); err != nil {
		t.Fatalf("guest AddProtocolAddress: %v", err)
	}
	s.SetRouteTable([]tcpip.Route{{
		Destination: header.IPv4EmptySubnet,
		Gateway:     tcpip.AddrFrom4(cfg.Gateway.As4()),
		NIC:         1,
	}})
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		buf := make([]byte, 65536)
		for {
			n, err := link.ReadFrame(buf)
			if err != nil {
				return
			}
			pkt := stack.NewPacketBuffer(stack.PacketBufferOptions{Payload: buffer.MakeWithData(append([]byte(nil), buf[:n]...))})
			ep.InjectInbound(header.Ethernet(buf).Type(), pkt)
			pkt.DecRef()
		}
	}()
	go func() {
		for {
			pkt := ep.ReadContext(ctx)
			if pkt == nil {
				return
			}
			v := pkt.ToView()
			link.WriteFrame(v.AsSlice())
			v.Release()
			pkt.DecRef()
		}
	}()
	t.Cleanup(func() {
		cancel()
		ep.Close()
		s.Destroy()
	})
	return &Guest{s: s, ep: ep}
}

func fullAddr(ap netip.AddrPort) tcpip.FullAddress {
	return tcpip.FullAddress{NIC: 1, Addr: tcpip.AddrFrom4(ap.Addr().As4()), Port: ap.Port()}
}

// DialTCP dials dst from the guest.
func (g *Guest) DialTCP(ctx context.Context, dst netip.AddrPort) (net.Conn, error) {
	return gonet.DialContextTCP(ctx, g.s, fullAddr(dst), ipv4.ProtocolNumber)
}

// DialUDP opens a UDP socket from the guest connected to dst.
func (g *Guest) DialUDP(dst netip.AddrPort) (net.Conn, error) {
	fa := fullAddr(dst)
	return gonet.DialUDP(g.s, nil, &fa, ipv4.ProtocolNumber)
}

// ListenTCP listens on port on the guest.
func (g *Guest) ListenTCP(port uint16) (net.Listener, error) {
	return gonet.ListenTCP(g.s, tcpip.FullAddress{NIC: 1, Port: port}, ipv4.ProtocolNumber)
}
