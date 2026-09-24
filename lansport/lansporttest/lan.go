// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

// Package lansporttest provides an in-memory network for testing code built
// on [lansport]: listeners at IPv4 addresses, and dialers that connect from a
// chosen source address, so servers see the remote addresses they would on
// a real LAN. Its connections are memnet pipes, which block in ways
// testing/synctest recognizes, so tests can run whole peer topologies under
// synctest with virtual time.
package lansporttest

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"sync"

	"tailscale.com/net/memnet"
	"tailscale.com/net/netx"
)

// LAN is an in-memory network. The zero value is ready to use.
type LAN struct {
	mu       sync.Mutex
	lns      map[netip.AddrPort]*listener
	nextPort uint16 // for listeners asking for port 0
	nextSrc  uint16 // ephemeral source ports for dialers
}

// Listen returns a listener at addr, an ip:port; port 0 picks an unused
// one. It panics on a malformed address or one already listening, which in
// a test is a bug.
func (l *LAN) Listen(addr string) net.Listener {
	ap, err := netip.ParseAddrPort(addr)
	if err != nil {
		panic("lansporttest: Listen(" + addr + "): " + err.Error())
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.lns == nil {
		l.lns = make(map[netip.AddrPort]*listener)
	}
	if ap.Port() == 0 {
		for {
			l.nextPort++
			ap = netip.AddrPortFrom(ap.Addr(), 40000+l.nextPort)
			if _, taken := l.lns[ap]; !taken {
				break
			}
		}
	} else if _, taken := l.lns[ap]; taken {
		panic("lansporttest: Listen(" + addr + "): address in use")
	}
	ln := &listener{lan: l, addr: ap, ch: make(chan net.Conn), closed: make(chan struct{})}
	l.lns[ap] = ln
	return ln
}

// DialFrom returns a dialer whose connections appear to come from src.
func (l *LAN) DialFrom(src netip.Addr) netx.DialFunc {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		dst, err := netip.ParseAddrPort(addr)
		if err != nil {
			return nil, &net.OpError{Op: "dial", Net: network, Err: err}
		}
		l.mu.Lock()
		ln := l.lns[dst]
		l.nextSrc++
		srcAP := netip.AddrPortFrom(src, 50000+l.nextSrc)
		l.mu.Unlock()
		if ln == nil {
			return nil, &net.OpError{Op: "dial", Net: network, Addr: net.TCPAddrFromAddrPort(dst), Err: errors.New("connection refused")}
		}
		c, s := memnet.NewTCPConn(srcAP, dst, 1<<20)
		select {
		case ln.ch <- s:
			return c, nil
		case <-ln.closed:
			c.Close()
			s.Close()
			return nil, &net.OpError{Op: "dial", Net: network, Addr: net.TCPAddrFromAddrPort(dst), Err: errors.New("connection refused")}
		case <-ctx.Done():
			c.Close()
			s.Close()
			return nil, &net.OpError{Op: "dial", Net: network, Addr: net.TCPAddrFromAddrPort(dst), Err: ctx.Err()}
		}
	}
}

type listener struct {
	lan    *LAN
	addr   netip.AddrPort
	ch     chan net.Conn
	closed chan struct{}
	once   sync.Once
}

func (ln *listener) Accept() (net.Conn, error) {
	select {
	case c := <-ln.ch:
		return c, nil
	case <-ln.closed:
		return nil, net.ErrClosed
	}
}

func (ln *listener) Close() error {
	ln.once.Do(func() {
		close(ln.closed)
		ln.lan.mu.Lock()
		delete(ln.lan.lns, ln.addr)
		ln.lan.mu.Unlock()
	})
	return nil
}

func (ln *listener) Addr() net.Addr { return net.TCPAddrFromAddrPort(ln.addr) }
