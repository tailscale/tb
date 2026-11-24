// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

//go:build unix

package lanlord

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/netip"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"tailscale.com/util/set"
)

// shortSocketPath returns a unix socket path short enough for macOS, whose
// limit is 104 bytes.
func shortSocketPath(t *testing.T) string {
	dir, err := os.MkdirTemp("", "ll")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return filepath.Join(dir, "s")
}

// startServer runs s on a new socket and returns a client for it.
func startServer(t *testing.T, s *Server) *Client {
	path := shortSocketPath(t)
	ln, err := Listen(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	if s.Logf == nil {
		s.Logf = t.Logf
	}
	go s.Serve(ln)
	return &Client{Socket: path}
}

// startEcho runs a TCP echo server on loopback and returns its address.
func startEcho(t *testing.T) netip.AddrPort {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				io.Copy(c, c)
			}()
		}
	}()
	return ln.Addr().(*net.TCPAddr).AddrPort()
}

func allowLoopback(ap netip.AddrPort) bool { return ap.Addr().IsLoopback() }

func myUID() string { return strconv.Itoa(os.Getuid()) }

func TestDial(t *testing.T) {
	echo := startEcho(t)
	c := startServer(t, &Server{AllowUsers: set.Of(myUID()), Allow: allowLoopback})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := c.DialContext(ctx, "tcp", echo.String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, ok := conn.(*net.TCPConn); !ok {
		t.Errorf("conn is %T, want *net.TCPConn", conn)
	}
	if got := conn.RemoteAddr().(*net.TCPAddr).AddrPort(); got != echo {
		t.Errorf("RemoteAddr = %v, want %v", got, echo)
	}
	// The connection must work in both directions after the handoff.
	for i, msg := range []string{"hello\n", "again\n"} {
		if _, err := io.WriteString(conn, msg); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
		got, err := bufio.NewReader(conn).ReadString('\n')
		if err != nil {
			t.Fatalf("read %d: %v", i, err)
		}
		if got != msg {
			t.Errorf("read %d = %q, want %q", i, got, msg)
		}
	}
}

func TestRefusals(t *testing.T) {
	echo := startEcho(t)
	tests := []struct {
		name    string
		srv     *Server
		addr    string
		wantErr string
	}{
		{
			name:    "uid",
			srv:     &Server{AllowUsers: set.Of("no-such-user-lanlord"), Allow: allowLoopback},
			addr:    echo.String(),
			wantErr: "not allowed",
		},
		{
			name:    "destination",
			srv:     &Server{AllowUsers: set.Of(myUID()), Allow: func(netip.AddrPort) bool { return false }},
			addr:    echo.String(),
			wantErr: "not an allowed destination",
		},
		{
			name:    "dial-error",
			srv:     &Server{AllowUsers: set.Of(myUID()), Allow: allowLoopback},
			addr:    "127.0.0.1:1", // Nothing listens on port 1.
			wantErr: "dial 127.0.0.1:1",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := startServer(t, tt.srv)
			conn, err := c.DialContext(context.Background(), "tcp4", tt.addr)
			if err == nil {
				conn.Close()
				t.Fatal("DialContext succeeded")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error %q does not contain %q", err, tt.wantErr)
			}
		})
	}
}

func TestAllowUsers(t *testing.T) {
	me, err := user.Current()
	if err != nil {
		t.Skipf("no current user: %v", err)
	}
	for _, tt := range []struct {
		name  string
		users set.Set[string]
		want  bool
	}{
		{"empty-allows-anyone", nil, true},
		{"uid", set.Of(myUID()), true},
		{"username", set.Of(me.Username), true},
		{"username-among-missing", set.Of("no-such-user-lanlord", me.Username), true},
		{"only-missing", set.Of("no-such-user-lanlord"), false},
		{"other-uid", set.Of(strconv.Itoa(os.Getuid() + 1)), false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s := &Server{AllowUsers: tt.users}
			if got := s.userAllowed(myUID()); got != tt.want && os.Getuid() != 0 {
				t.Errorf("userAllowed = %v, want %v", got, tt.want)
			}
			if !s.userAllowed("0") {
				t.Error("root not allowed")
			}
		})
	}
}

func TestServeRequiresAllow(t *testing.T) {
	ln, err := Listen(shortSocketPath(t))
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	if err := (&Server{}).Serve(ln); err == nil || !strings.Contains(err.Error(), "Allow is required") {
		t.Errorf("Serve without Allow = %v, want an error", err)
	}
}

func TestClientRejectsBadAddresses(t *testing.T) {
	c := &Client{Socket: "/nonexistent"}
	for _, tt := range []struct{ network, addr string }{
		{"udp", "10.0.0.1:53"},
		{"tcp6", "[fd00::1]:22"},
		{"tcp", "[fd00::1]:22"},
		{"tcp", "host.example:22"},
	} {
		if _, err := c.DialContext(context.Background(), tt.network, tt.addr); err == nil {
			t.Errorf("DialContext(%q, %q) succeeded", tt.network, tt.addr)
		}
	}
}

func TestClientContextDeadline(t *testing.T) {
	// A server that accepts but never replies.
	path := shortSocketPath(t)
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		if c, err := ln.Accept(); err == nil {
			accepted <- c
		}
	}()
	t.Cleanup(func() {
		select {
		case c := <-accepted:
			c.Close()
		default:
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_, err = (&Client{Socket: path}).DialContext(ctx, "tcp", "10.0.0.1:31369")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want context.DeadlineExceeded", err)
	}
}
