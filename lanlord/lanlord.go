// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

//go:build unix

// Package lanlord lets a process that may not dial the LAN get LAN TCP
// connections from a root process that may.
//
// On macOS, local network privacy blocks a non-root launchd daemon (one with
// a UserName) from connecting to LAN addresses: connect fails with "no route
// to host", and there is no user session to prompt. Root launchd daemons are
// exempt, and inbound connections are not restricted. So a small root daemon,
// cmd/lanlord, runs a [Server]: it accepts requests on a unix socket, checks
// the caller's uid and the destination against its policy, dials, and passes
// the connected socket back with SCM_RIGHTS. The caller then owns an ordinary
// TCP connection; no bytes go through the root process.
//
// A [Client] is the caller's side. Its [Client.DialContext] has the signature
// of a net.Dialer's, so it can be the LAN dialer of a lansport.Server.
//
// Only IPv4 TCP is supported, since lansport advertises only IPv4 LAN
// addresses.
package lanlord

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"os/user"
	"strings"
	"time"

	"github.com/tailscale/peercred"
	"golang.org/x/sys/unix"
	"tailscale.com/net/netx"
	"tailscale.com/types/logger"
	"tailscale.com/util/set"
)

// DefaultSocket is the unix socket path that cmd/lanlord listens on and that
// a zero [Client] uses.
const DefaultSocket = "/var/run/lanlord.sock"

// requestTimeout bounds how long a server waits for a request line and how
// long a client waits for a reply when its context has no deadline.
const requestTimeout = 10 * time.Second

// defaultDialTimeout bounds a server's dial when [Server.Dial] is nil.
const defaultDialTimeout = 5 * time.Second

// The protocol is one request per unix connection. The client writes
// "dial tcp4 IP:PORT\n". The server replies with one line: "ok\n" with the
// connected socket attached as SCM_RIGHTS, or "err MESSAGE\n".
const (
	reqPrefix = "dial tcp4 "
	replyOK   = "ok"
	replyErr  = "err "
)

// maxLine bounds a request or reply line.
const maxLine = 512

// Server dials TCP connections for local processes and hands them the
// connected sockets. Use [Listen] for its listener and [Server.Serve] to run
// it.
type Server struct {
	// AllowUsers are the usernames or decimal uids whose processes may
	// request connections. If empty, any local process may. Root is always
	// allowed.
	//
	// Each request is checked against it when it arrives, so it may name
	// users that don't exist on this host, and one list can serve hosts
	// whose CI user differs (such as "runner" and "ec2-user").
	AllowUsers set.Set[string]

	// Allow decides which destinations may be dialed. It is required.
	Allow func(netip.AddrPort) bool

	// Dial, if non-nil, replaces how destinations are dialed. It must
	// return a *net.TCPConn.
	Dial netx.DialFunc

	// Logf, if non-nil, logs each request: the connection dialed for it, or
	// why it was refused. A nil value means to not log.
	Logf logger.Logf
}

// Listen removes any stale socket at path and listens on it. Any local user
// may connect; [Server.AllowUsers] decides who is served.
func Listen(path string) (*net.UnixListener, error) {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o666); err != nil {
		ln.Close()
		return nil, err
	}
	return ln, nil
}

// Serve accepts and serves requests on ln until ln fails. It fails at once
// if s.Allow is nil.
func (s *Server) Serve(ln *net.UnixListener) error {
	if s.Allow == nil {
		return errors.New("lanlord: Server.Allow is required")
	}
	for {
		c, err := ln.AcceptUnix()
		if err != nil {
			return err
		}
		go s.serveConn(c)
	}
}

func (s *Server) logf(format string, args ...any) {
	if s.Logf != nil {
		s.Logf(format, args...)
	}
}

// userAllowed reports whether a process running as uid may request
// connections.
func (s *Server) userAllowed(uid string) bool {
	if len(s.AllowUsers) == 0 || uid == "0" {
		return true // Root could dial for itself anyway.
	}
	if s.AllowUsers.Contains(uid) {
		return true
	}
	u, err := user.LookupId(uid)
	return err == nil && s.AllowUsers.Contains(u.Username)
}

func (s *Server) serveConn(c *net.UnixConn) {
	defer c.Close()
	fail := func(format string, args ...any) {
		msg := fmt.Sprintf(format, args...)
		s.logf("lanlord: %s", msg)
		c.Write([]byte(replyErr + msg + "\n"))
	}
	uid, err := peerUID(c)
	if err != nil {
		fail("peer credentials: %v", err)
		return
	}
	if !s.userAllowed(uid) {
		fail("uid %s not allowed", uid)
		return
	}
	c.SetReadDeadline(time.Now().Add(requestTimeout))
	line, err := bufio.NewReaderSize(io.LimitReader(c, maxLine), maxLine).ReadString('\n')
	if err != nil {
		fail("uid %s: reading request: %v", uid, err)
		return
	}
	dest, ok := strings.CutPrefix(strings.TrimSuffix(line, "\n"), reqPrefix)
	if !ok {
		fail("uid %s: malformed request", uid)
		return
	}
	ap, err := netip.ParseAddrPort(dest)
	if err != nil {
		fail("uid %s: bad address: %v", uid, err)
		return
	}
	if !s.Allow(ap) {
		fail("uid %s: %v is not an allowed destination", uid, ap)
		return
	}
	tc, err := s.dial(ap)
	if err != nil {
		fail("uid %s: dial %v: %v", uid, ap, err)
		return
	}
	defer tc.Close() // The client gets its own descriptor.
	f, err := tc.File()
	if err != nil {
		fail("uid %s: %v: %v", uid, ap, err)
		return
	}
	defer f.Close()
	if _, _, err := c.WriteMsgUnix([]byte(replyOK+"\n"), unix.UnixRights(int(f.Fd())), nil); err != nil {
		s.logf("lanlord: uid %s: passing connection to %v: %v", uid, ap, err)
		return
	}
	s.logf("lanlord: uid %s: dialed %v", uid, ap)
	// Keep this process's copies until the client closes the connection,
	// which it does once it holds its own. If the descriptor in flight
	// were the socket's only reference, the kernel could close the TCP
	// connection before the client receives it, as seen on macOS.
	c.SetReadDeadline(time.Now().Add(requestTimeout))
	io.Copy(io.Discard, c)
}

func (s *Server) dial(ap netip.AddrPort) (*net.TCPConn, error) {
	ctx, cancel := context.WithTimeout(context.Background(), defaultDialTimeout)
	defer cancel()
	dial := s.Dial
	if dial == nil {
		dial = (&net.Dialer{KeepAlive: 30 * time.Second}).DialContext
	}
	c, err := dial(ctx, "tcp4", ap.String())
	if err != nil {
		return nil, err
	}
	tc, ok := c.(*net.TCPConn)
	if !ok {
		c.Close()
		return nil, fmt.Errorf("dialer returned %T, not *net.TCPConn", c)
	}
	return tc, nil
}

// peerUID returns the uid of the process on the other end of c, in decimal.
func peerUID(c *net.UnixConn) (string, error) {
	creds, err := peercred.Get(c)
	if err != nil {
		return "", err
	}
	uid, ok := creds.UserID()
	if !ok {
		return "", errors.New("peer uid unknown")
	}
	return uid, nil
}

// Client asks a [Server] for connections. The zero value uses
// [DefaultSocket].
type Client struct {
	// Socket is the server's unix socket path. If empty, DefaultSocket is
	// used.
	Socket string
}

// DialContext asks the server to dial addr, an IPv4 address and port, and
// returns the connection it passes back. network must be "tcp" or "tcp4".
func (c *Client) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	if network != "tcp" && network != "tcp4" {
		return nil, fmt.Errorf("lanlord: unsupported network %q", network)
	}
	ap, err := netip.ParseAddrPort(addr)
	if err != nil || !ap.Addr().Is4() {
		return nil, fmt.Errorf("lanlord: %q is not an IPv4 address and port", addr)
	}
	socket := c.Socket
	if socket == "" {
		socket = DefaultSocket
	}
	var d net.Dialer
	uc, err := d.DialContext(ctx, "unix", socket)
	if err != nil {
		return nil, fmt.Errorf("lanlord: %w", err)
	}
	// Closing uc tells the server it may drop its copies of the connection,
	// so it must happen only after FileConn below has made ours.
	defer uc.Close()
	deadline, ctxHasDeadline := ctx.Deadline()
	if !ctxHasDeadline {
		deadline = time.Now().Add(requestTimeout)
	}
	uc.SetDeadline(deadline)
	stop := context.AfterFunc(ctx, func() { uc.SetDeadline(time.Now()) })
	defer stop()
	// ctxErr returns the context's error for an I/O error the context
	// caused, even if the deadline fired on uc before the context noticed.
	ctxErr := func(err error) error {
		if cerr := ctx.Err(); cerr != nil {
			return cerr
		}
		if ctxHasDeadline && errors.Is(err, os.ErrDeadlineExceeded) {
			return context.DeadlineExceeded
		}
		return nil
	}

	if _, err := io.WriteString(uc, reqPrefix+ap.String()+"\n"); err != nil {
		if cerr := ctxErr(err); cerr != nil {
			return nil, cerr
		}
		return nil, fmt.Errorf("lanlord: sending request: %w", err)
	}
	buf := make([]byte, maxLine)
	oob := make([]byte, unix.CmsgSpace(4))
	n, oobn, _, _, err := uc.(*net.UnixConn).ReadMsgUnix(buf, oob)
	if err != nil {
		if cerr := ctxErr(err); cerr != nil {
			return nil, cerr
		}
		return nil, fmt.Errorf("lanlord: reading reply: %w", err)
	}
	fd, fdErr := receivedFD(oob[:oobn])
	reply := strings.TrimSuffix(string(buf[:n]), "\n")
	if reply != replyOK {
		if fdErr == nil {
			unix.Close(fd)
		}
		if msg, ok := strings.CutPrefix(reply, replyErr); ok {
			return nil, fmt.Errorf("lanlord: %s", msg)
		}
		return nil, fmt.Errorf("lanlord: malformed reply %q", reply)
	}
	if fdErr != nil {
		return nil, fmt.Errorf("lanlord: %w", fdErr)
	}
	f := os.NewFile(uintptr(fd), "lanlord:"+ap.String())
	defer f.Close() // FileConn dups it.
	conn, err := net.FileConn(f)
	if err != nil {
		return nil, fmt.Errorf("lanlord: %w", err)
	}
	return conn, nil
}

// receivedFD returns the one descriptor passed in the control message oob.
func receivedFD(oob []byte) (int, error) {
	msgs, err := unix.ParseSocketControlMessage(oob)
	if err != nil {
		return -1, err
	}
	if len(msgs) != 1 {
		return -1, fmt.Errorf("reply has %d control messages, want 1", len(msgs))
	}
	fds, err := unix.ParseUnixRights(&msgs[0])
	if err != nil {
		return -1, err
	}
	if len(fds) != 1 {
		for _, fd := range fds {
			unix.Close(fd)
		}
		return -1, fmt.Errorf("reply has %d descriptors, want 1", len(fds))
	}
	return fds[0], nil
}
