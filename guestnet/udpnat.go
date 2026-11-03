// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package guestnet

import (
	"fmt"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"
)

// udpMapping is the host socket for one guest UDP source address. Every
// egress flow from that guest address leaves from the same host address and
// port, whatever its destination: an endpoint-independent mapping (RFC 4787
// section 4.1), as typical home routers do. Inbound datagrams reach the guest
// only from remote addresses and ports it has sent to (address- and
// port-dependent filtering, RFC 4787 section 5). Together that's an "easy
// NAT" for NAT traversal, so peer-to-peer protocols like Tailscale's
// disco can punch through it.
type udpMapping struct {
	n   *Net
	src netip.AddrPort // the guest's address
	pc  net.PacketConn
	// refs counts sessions using pc. It's guarded by Net.udpMu.
	refs int

	mu       sync.Mutex
	sessions map[netip.AddrPort]*udpSession // by remote address
}

// udpSession is one guest UDP flow (a source and destination pair) sharing a
// udpMapping.
type udpSession struct {
	guest net.Conn     // the gVisor endpoint, connected to the guest
	last  atomic.Int64 // unix nanos of the last inbound datagram
}

// acquireUDPMapping returns the mapping for guest address src, creating its
// host socket if needed. The caller must call releaseUDPMapping.
func (n *Net) acquireUDPMapping(src netip.AddrPort) (*udpMapping, error) {
	n.udpMu.Lock()
	defer n.udpMu.Unlock()
	if m, ok := n.udpMaps[src]; ok {
		m.refs++
		return m, nil
	}
	pc, err := n.cfg.ListenPacket(n.ctx, "udp4", ":0")
	if err != nil {
		return nil, err
	}
	m := &udpMapping{
		n:        n,
		src:      src,
		pc:       pc,
		refs:     1,
		sessions: make(map[netip.AddrPort]*udpSession),
	}
	if n.udpMaps == nil {
		n.udpMaps = make(map[netip.AddrPort]*udpMapping)
	}
	n.udpMaps[src] = m
	n.wg.Add(1)
	go m.readLoop()
	return m, nil
}

// releaseUDPMapping drops a reference to m, closing its host socket when the
// last session using it ends.
func (n *Net) releaseUDPMapping(m *udpMapping) {
	n.udpMu.Lock()
	defer n.udpMu.Unlock()
	m.refs--
	if m.refs == 0 {
		delete(n.udpMaps, m.src)
		m.pc.Close()
	}
}

// readLoop delivers datagrams from the host socket to the guest session for
// their sender, dropping those from remotes the guest hasn't sent to.
func (m *udpMapping) readLoop() {
	defer m.n.wg.Done()
	buf := make([]byte, udpMaxPacket)
	for {
		k, addr, err := m.pc.ReadFrom(buf)
		if err != nil {
			return
		}
		ua, ok := addr.(*net.UDPAddr)
		if !ok {
			continue
		}
		from := ua.AddrPort()
		from = netip.AddrPortFrom(from.Addr().Unmap(), from.Port())
		m.mu.Lock()
		s := m.sessions[from]
		m.mu.Unlock()
		if s == nil {
			continue // filtered: the guest never sent to from
		}
		s.last.Store(time.Now().UnixNano())
		s.guest.Write(buf[:k])
	}
}

// relayUDP shuttles datagrams between the guest session guest (from src to
// dst) and dst, via src's shared host socket, until the session is idle in
// both directions for udpIdleTimeout. Byte counts are kept by the flowConn.
func (n *Net) relayUDP(guest net.Conn, src, dst netip.AddrPort) error {
	m, err := n.acquireUDPMapping(src)
	if err != nil {
		n.Events.Add(Event{Time: n.cfg.Now(), Kind: "dial-error", Msg: fmt.Sprintf("udp %v: %v", dst, err)})
		return err
	}
	defer n.releaseUDPMapping(m)

	s := &udpSession{guest: guest}
	s.last.Store(time.Now().UnixNano())
	m.mu.Lock()
	m.sessions[dst] = s
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		if m.sessions[dst] == s {
			delete(m.sessions, dst)
		}
		m.mu.Unlock()
	}()

	to := net.UDPAddrFromAddrPort(dst)
	buf := make([]byte, udpMaxPacket)
	lastOut := time.Now()
	for {
		guest.SetReadDeadline(lastOut.Add(udpIdleTimeout))
		k, err := guest.Read(buf)
		if err != nil {
			if n.ctx.Err() == nil && isTimeout(err) {
				// Inbound traffic keeps the session alive too.
				if lastIn := time.Unix(0, s.last.Load()); time.Since(lastIn) < udpIdleTimeout {
					lastOut = lastIn
					continue
				}
			}
			// Idle, guest close, or Net shutdown: all normal.
			return nil
		}
		lastOut = time.Now()
		if _, err := m.pc.WriteTo(buf[:k], to); err != nil {
			return err
		}
	}
}

func isTimeout(err error) bool {
	ne, ok := err.(net.Error)
	return ok && ne.Timeout()
}
