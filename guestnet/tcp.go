// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package guestnet

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/waiter"
)

// egressDialTimeout bounds how long the guest's SYN waits for the host-side
// dial before we answer with a RST.
const egressDialTimeout = 10 * time.Second

var flowIDs atomic.Uint64

// handleTCP is called by the gVisor forwarder for every SYN from the guest.
func (n *Net) handleTCP(r *tcp.ForwarderRequest) {
	id := r.ID()
	dst := netip.AddrPortFrom(addrFromTCPIP(id.LocalAddress), id.LocalPort)
	src := netip.AddrPortFrom(addrFromTCPIP(id.RemoteAddress), id.RemotePort)
	flow := Flow{ID: flowIDs.Add(1), Proto: "tcp", Src: src, Dst: dst, Start: n.cfg.Now()}

	if svc, ok := n.service(dst); ok {
		flow.Kind, flow.Service = "service", svc.Name()
		conn := n.acceptTCP(r, flow)
		if conn == nil {
			return
		}
		n.wg.Add(1)
		go func() {
			defer n.wg.Done()
			defer n.track(conn)()
			// The service owns conn and records the flow's close when it
			// closes it.
			svc.HandleConn(n.ctx, conn)
		}()
		return
	}

	if dst.Port() == 53 {
		flow.Kind = "dns"
		conn := n.acceptTCP(r, flow)
		if conn == nil {
			return
		}
		n.wg.Add(1)
		go func() {
			defer n.wg.Done()
			defer n.track(conn)()
			conn.closeWithErr(n.serveDNSTCP(conn, src, dst))
		}()
		return
	}

	if !n.egressAllowed(dst.Addr()) {
		flow.Kind = "blocked"
		flow.End = flow.Start
		n.Flows.Add(flow)
		n.Events.Add(Event{Time: flow.Start, Kind: "blocked", Msg: fmt.Sprintf("tcp %v -> %v", src, dst)})
		r.Complete(true)
		return
	}

	// Egress: dial first so a refused or unreachable destination becomes a
	// RST to the guest instead of an accepted-then-closed connection. The
	// forwarder drops the guest's retransmitted SYNs meanwhile.
	flow.Kind = "egress"
	n.wg.Add(1)
	go func() {
		defer n.wg.Done()
		ctx, cancel := context.WithTimeout(n.ctx, egressDialTimeout)
		defer cancel()
		remote, err := n.cfg.Dialer.DialContext(ctx, "tcp", dst.String())
		if err != nil {
			flow.Err = err.Error()
			flow.End = n.cfg.Now()
			n.Flows.Add(flow)
			n.Events.Add(Event{Time: flow.End, Kind: "dial-error", Msg: fmt.Sprintf("tcp %v: %v", dst, err)})
			r.Complete(true)
			return
		}
		conn := n.acceptTCP(r, flow)
		if conn == nil {
			remote.Close()
			return
		}
		defer n.track(conn)()
		defer n.track(remote)()
		splice(conn, remote)
		conn.Close()
	}()
}

// acceptTCP completes the handshake and returns the guest-side connection
// wrapped for flow accounting, recording the flow's open record. It returns
// nil (and records the failure) if the endpoint could not be created.
func (n *Net) acceptTCP(r *tcp.ForwarderRequest, flow Flow) *flowConn {
	var wq waiter.Queue
	ep, err := r.CreateEndpoint(&wq)
	if err != nil {
		flow.Err = err.String()
		flow.End = n.cfg.Now()
		n.Flows.Add(flow)
		r.Complete(true)
		return nil
	}
	ep.SocketOptions().SetKeepAlive(true)
	r.Complete(false)
	n.Flows.Add(flow)
	return newFlowConn(n, gonet.NewTCPConn(&wq, ep), flow)
}

// track arranges for conn to be closed when the Net closes, so no goroutine
// outlives it. The returned func cancels that arrangement; call it when the
// conn is done.
func (n *Net) track(conn net.Conn) func() {
	stop := context.AfterFunc(n.ctx, func() {
		// gonet conns don't reliably wake a blocked Read on Close alone; a
		// past deadline does.
		conn.SetReadDeadline(time.Unix(1, 0))
		conn.Close()
	})
	return func() { stop() }
}

// flowConn wraps a guest-side connection to count bytes and record the
// flow's close record exactly once, when whoever owns it closes it.
type flowConn struct {
	net.Conn
	n    *Net
	flow Flow
	in   atomic.Int64 // bytes written to the guest
	out  atomic.Int64 // bytes read from the guest
	once sync.Once
}

func newFlowConn(n *Net, c net.Conn, flow Flow) *flowConn {
	return &flowConn{Conn: c, n: n, flow: flow}
}

func (c *flowConn) Read(b []byte) (int, error) {
	m, err := c.Conn.Read(b)
	c.out.Add(int64(m))
	return m, err
}

func (c *flowConn) Write(b []byte) (int, error) {
	m, err := c.Conn.Write(b)
	c.in.Add(int64(m))
	return m, err
}

// CloseWrite half-closes the guest side, for splice.
func (c *flowConn) CloseWrite() error {
	if cw, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return nil
}

// Close implements net.Conn and records the flow's close record.
func (c *flowConn) Close() error { return c.closeWithErr(nil) }

// closeWithErr closes the conn, recording err (if any) in the flow's close
// record. Only the first close records anything.
func (c *flowConn) closeWithErr(err error) error {
	cerr := c.Conn.Close()
	c.once.Do(func() {
		f := c.flow
		f.End = c.n.cfg.Now()
		f.BytesIn, f.BytesOut = c.in.Load(), c.out.Load()
		if err != nil {
			f.Err = err.Error()
		}
		c.n.Flows.Add(f)
	})
	return cerr
}

// splice copies bidirectionally between a and b with half-close
// propagation, returning when both directions are done.
//
// When one direction hits EOF it half-closes the other side, then waits a
// bounded time for the remaining direction. gVisor's gonet.TCPConn does not
// reliably wake a blocked Read on Close, so after the grace period both
// connections get a past read deadline before Close to guarantee the second
// copier exits and the endpoint is released.
func splice(a, b net.Conn) {
	done := make(chan struct{}, 2)
	cp := func(dst, src net.Conn) {
		io.Copy(dst, src)
		if cw, ok := dst.(interface{ CloseWrite() error }); ok {
			cw.CloseWrite()
		}
		done <- struct{}{}
	}
	go cp(a, b)
	go cp(b, a)

	<-done
	t := time.NewTimer(5 * time.Second)
	defer t.Stop()
	select {
	case <-done:
	case <-t.C:
		past := time.Unix(1, 0)
		a.SetReadDeadline(past)
		b.SetReadDeadline(past)
		a.Close()
		b.Close()
		<-done
	}
	a.Close()
	b.Close()
}
