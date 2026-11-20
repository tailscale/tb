// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package guestnet

import (
	"fmt"
	"net/netip"
	"time"

	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"
	"gvisor.dev/gvisor/pkg/waiter"
)

// udpIdleTimeout ends a UDP session after this long without traffic in
// either direction.
const udpIdleTimeout = 60 * time.Second

// udpMaxPacket is the largest UDP payload we relay.
const udpMaxPacket = 65535

// handleUDP is called by the gVisor forwarder for the first packet of every
// UDP 4-tuple from the guest. Returning false drops the packet (and gVisor
// answers with ICMP port unreachable).
func (n *Net) handleUDP(r *udp.ForwarderRequest) bool {
	id := r.ID()
	dst := netip.AddrPortFrom(addrFromTCPIP(id.LocalAddress), id.LocalPort)
	src := netip.AddrPortFrom(addrFromTCPIP(id.RemoteAddress), id.RemotePort)
	flow := Flow{ID: flowIDs.Add(1), Proto: "udp", Src: src, Dst: dst, Start: n.cfg.Now()}

	switch dst.Port() {
	case 53:
		flow.Kind = "dns"
		conn := n.acceptUDP(r, flow)
		if conn == nil {
			return true
		}
		if !n.goTracked(func() {
			defer n.track(conn)()
			conn.closeWithErr(n.serveDNSUDP(conn, src, dst))
		}) {
			conn.Close()
		}
		return true
	}

	if !n.egressAllowed(dst.Addr()) {
		// Multicast (mDNS, SSDP) and broadcast chatter is dropped silently;
		// unicast blocks are worth a record.
		if !dst.Addr().IsMulticast() && dst.Addr() != netipBroadcast {
			flow.Kind = "blocked"
			flow.End = flow.Start
			n.Flows.Add(flow)
			n.Events.Add(Event{Time: flow.Start, Kind: "blocked", Msg: fmt.Sprintf("udp %v -> %v", src, dst)})
		}
		return false
	}

	flow.Kind = "egress"
	conn := n.acceptUDP(r, flow)
	if conn == nil {
		return true
	}
	if !n.goTracked(func() {
		defer n.track(conn)()
		conn.closeWithErr(n.relayUDP(conn, src, dst))
	}) {
		conn.Close()
	}
	return true
}

// acceptUDP creates the gVisor endpoint for the session (bound to the
// guest's chosen destination and connected to the guest) and records the
// flow's open record.
func (n *Net) acceptUDP(r *udp.ForwarderRequest, flow Flow) *flowConn {
	var wq waiter.Queue
	ep, err := r.CreateEndpoint(&wq)
	if err != nil {
		flow.Err = err.String()
		flow.End = n.cfg.Now()
		n.Flows.Add(flow)
		return nil
	}
	n.Flows.Add(flow)
	return newFlowConn(n, gonet.NewUDPConn(&wq, ep), flow)
}
