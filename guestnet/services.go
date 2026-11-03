// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package guestnet

import (
	"context"
	"errors"
	"net"
	"sync"
)

// Service handles guest TCP connections to a registered destination.
type Service interface {
	// Name identifies the service in flow logs.
	Name() string
	// HandleConn serves one guest connection. It owns conn and must close it.
	// ctx is done when the Net closes.
	HandleConn(ctx context.Context, conn net.Conn)
}

// ServiceFunc adapts a function to a Service.
type ServiceFunc struct {
	ServiceName string
	Handle      func(ctx context.Context, conn net.Conn)
}

// Name implements Service.
func (s ServiceFunc) Name() string { return s.ServiceName }

// HandleConn implements Service.
func (s ServiceFunc) HandleConn(ctx context.Context, conn net.Conn) { s.Handle(ctx, conn) }

// ForwardService relays guest connections to a host-side address, such as
// "tcp:127.0.0.1:2049" or "unix:/var/run/foo.sock". It replaces the pf rdr
// rules corp CI uses on macOS today.
type ForwardService struct {
	ServiceName string
	// Network and Address are passed to Dialer.DialContext.
	Network, Address string
	// Dialer defaults to a net.Dialer.
	Dialer Dialer
}

// Name implements Service.
func (s *ForwardService) Name() string { return s.ServiceName }

// HandleConn implements Service.
func (s *ForwardService) HandleConn(ctx context.Context, conn net.Conn) {
	defer conn.Close()
	d := s.Dialer
	if d == nil {
		d = &net.Dialer{}
	}
	remote, err := d.DialContext(ctx, s.Network, s.Address)
	if err != nil {
		return
	}
	defer remote.Close()
	splice(conn, remote)
}

// ListenerService is a Service that exposes guest connections as a
// net.Listener, so an http.Server (or anything else that Accepts) can serve
// the guest without knowing about gVisor.
type ListenerService struct {
	ServiceName string

	once   sync.Once
	conns  chan net.Conn
	closed chan struct{}
	cmu    sync.Once
}

// NewListenerService returns a ListenerService named name.
func NewListenerService(name string) *ListenerService {
	s := &ListenerService{ServiceName: name}
	s.init()
	return s
}

func (s *ListenerService) init() {
	s.once.Do(func() {
		s.conns = make(chan net.Conn)
		s.closed = make(chan struct{})
	})
}

// Name implements Service.
func (s *ListenerService) Name() string { return s.ServiceName }

// HandleConn implements Service by handing conn to Accept.
func (s *ListenerService) HandleConn(ctx context.Context, conn net.Conn) {
	s.init()
	select {
	case s.conns <- conn:
	case <-s.closed:
		conn.Close()
	case <-ctx.Done():
		conn.Close()
	}
}

// Accept implements net.Listener.
func (s *ListenerService) Accept() (net.Conn, error) {
	s.init()
	select {
	case c := <-s.conns:
		return c, nil
	case <-s.closed:
		return nil, errListenerClosed
	}
}

// Close implements net.Listener.
func (s *ListenerService) Close() error {
	s.init()
	s.cmu.Do(func() { close(s.closed) })
	return nil
}

// Addr implements net.Listener.
func (s *ListenerService) Addr() net.Addr { return serviceAddr(s.ServiceName) }

var errListenerClosed = errors.New("guestnet: listener closed")

type serviceAddr string

func (a serviceAddr) Network() string { return "guestnet" }
func (a serviceAddr) String() string  { return string(a) }
