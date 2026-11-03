// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package guestnet

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

// DefaultUpstreams are the resolvers used when none are configured. They
// match corp CI's defaults.
var DefaultUpstreams = []netip.AddrPort{
	netip.MustParseAddrPort("8.8.8.8:53"),
	netip.MustParseAddrPort("1.1.1.1:53"),
}

// dnsQueryTimeout bounds one upstream exchange.
const dnsQueryTimeout = 5 * time.Second

// dnsIdleTimeout ends a guest DNS session (one source port) after this long
// without a query; the guest's resolver typically uses a fresh port per
// query anyway.
const dnsIdleTimeout = 10 * time.Second

// Resolver answers DNS queries on the guest's behalf.
type Resolver interface {
	// Exchange sends the wire-format query and returns the wire-format
	// response and a description of who answered (for logs).
	Exchange(ctx context.Context, query []byte) (resp []byte, upstream string, err error)
}

// UpstreamResolver forwards queries to upstream servers in order, over UDP
// with TCP fallback on truncation.
type UpstreamResolver struct {
	Servers []netip.AddrPort
	// Dialer defaults to a net.Dialer.
	Dialer Dialer
}

// NewUpstreamResolver returns an UpstreamResolver for servers.
func NewUpstreamResolver(servers ...netip.AddrPort) *UpstreamResolver {
	return &UpstreamResolver{Servers: servers}
}

// Exchange implements Resolver.
func (r *UpstreamResolver) Exchange(ctx context.Context, query []byte) ([]byte, string, error) {
	d := r.Dialer
	if d == nil {
		d = &net.Dialer{}
	}
	var lastErr error
	for _, s := range r.Servers {
		resp, err := exchangeUDP(ctx, d, s, query)
		if err == nil && len(resp) >= 12 && resp[2]&0x02 != 0 { // TC bit set
			resp, err = exchangeTCP(ctx, d, s, query)
		}
		if err == nil {
			return resp, s.String(), nil
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = errors.New("no upstream resolvers configured")
	}
	return nil, "", lastErr
}

func exchangeUDP(ctx context.Context, d Dialer, server netip.AddrPort, query []byte) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, dnsQueryTimeout)
	defer cancel()
	conn, err := d.DialContext(ctx, "udp", server.String())
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	if dl, ok := ctx.Deadline(); ok {
		conn.SetDeadline(dl)
	}
	if _, err := conn.Write(query); err != nil {
		return nil, err
	}
	buf := make([]byte, 4096)
	for {
		m, err := conn.Read(buf)
		if err != nil {
			return nil, err
		}
		// Match the transaction ID; ignore stray datagrams.
		if m >= 2 && len(query) >= 2 && buf[0] == query[0] && buf[1] == query[1] {
			return buf[:m], nil
		}
	}
}

func exchangeTCP(ctx context.Context, d Dialer, server netip.AddrPort, query []byte) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, dnsQueryTimeout)
	defer cancel()
	conn, err := d.DialContext(ctx, "tcp", server.String())
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	if dl, ok := ctx.Deadline(); ok {
		conn.SetDeadline(dl)
	}
	if err := writeDNSTCP(conn, query); err != nil {
		return nil, err
	}
	return readDNSTCP(conn)
}

func writeDNSTCP(w io.Writer, msg []byte) error {
	buf := make([]byte, 2+len(msg))
	binary.BigEndian.PutUint16(buf, uint16(len(msg)))
	copy(buf[2:], msg)
	_, err := w.Write(buf)
	return err
}

func readDNSTCP(r io.Reader) ([]byte, error) {
	var lenBuf [2]byte
	if _, err := io.ReadFull(r, lenBuf[:]); err != nil {
		return nil, err
	}
	msg := make([]byte, binary.BigEndian.Uint16(lenBuf[:]))
	if _, err := io.ReadFull(r, msg); err != nil {
		return nil, err
	}
	return msg, nil
}

// serveDNSUDP answers queries arriving on one guest UDP session.
func (n *Net) serveDNSUDP(conn net.Conn, src, dst netip.AddrPort) error {
	buf := make([]byte, 4096)
	for {
		conn.SetReadDeadline(time.Now().Add(dnsIdleTimeout))
		m, err := conn.Read(buf)
		if err != nil {
			return nil
		}
		resp := n.resolve(buf[:m], src, dst, "udp")
		if resp != nil {
			if _, err := conn.Write(resp); err != nil {
				return err
			}
		}
	}
}

// serveDNSTCP answers length-prefixed queries on one guest TCP connection.
func (n *Net) serveDNSTCP(conn net.Conn, src, dst netip.AddrPort) error {
	for {
		conn.SetReadDeadline(time.Now().Add(dnsIdleTimeout))
		q, err := readDNSTCP(conn)
		if err != nil {
			return nil
		}
		resp := n.resolve(q, src, dst, "tcp")
		if resp == nil {
			return nil
		}
		if err := writeDNSTCP(conn, resp); err != nil {
			return err
		}
	}
}

// resolve answers one query via the configured Resolver and logs it. On
// upstream failure it returns a SERVFAIL so the guest doesn't wait.
func (n *Net) resolve(query []byte, src, dst netip.AddrPort, proto string) []byte {
	ev := DNSEvent{Time: n.cfg.Now(), Src: src, Dst: dst, Proto: proto}
	var p dnsmessage.Parser
	hdr, err := p.Start(query)
	if err != nil {
		ev.Err = "unparseable query"
		n.DNS.Add(ev)
		return nil
	}
	if q, err := p.Question(); err == nil {
		ev.Name = q.Name.String()
		ev.Type = q.Type.String()
	}

	ctx, cancel := context.WithTimeout(n.ctx, dnsQueryTimeout)
	defer cancel()
	resp, upstream, err := n.cfg.Resolver.Exchange(ctx, query)
	ev.Duration = n.cfg.Now().Sub(ev.Time)
	ev.Upstream = upstream
	if err != nil {
		ev.Err = err.Error()
		ev.RCode = dnsmessage.RCodeServerFailure.String()
		n.DNS.Add(ev)
		return servfail(hdr, query)
	}
	var rp dnsmessage.Parser
	if rh, err := rp.Start(resp); err == nil {
		ev.RCode = rh.RCode.String()
		rp.SkipAllQuestions()
		if answers, err := rp.AllAnswers(); err == nil {
			ev.Answers = len(answers)
		}
	}
	n.DNS.Add(ev)
	return resp
}

// servfail builds a SERVFAIL response echoing the query's ID and question.
func servfail(hdr dnsmessage.Header, query []byte) []byte {
	resp := make([]byte, len(query))
	copy(resp, query)
	if len(resp) < 12 {
		return nil
	}
	resp[2] = 0x80 | (resp[2] & 0x79) // QR=1, keep opcode and RD
	resp[3] = byte(dnsmessage.RCodeServerFailure)
	// Zero the answer, authority and additional counts; keep QDCOUNT.
	binary.BigEndian.PutUint16(resp[6:], 0)
	binary.BigEndian.PutUint16(resp[8:], 0)
	binary.BigEndian.PutUint16(resp[10:], 0)
	_ = hdr
	return resp[:12+questionLen(resp[12:])]
}

// questionLen returns the encoded length of the first question section
// entry in b, or 0 if malformed.
func questionLen(b []byte) int {
	i := 0
	for i < len(b) {
		l := int(b[i])
		if l == 0 {
			i++
			break
		}
		if l&0xC0 == 0xC0 {
			i += 2
			break
		}
		i += 1 + l
	}
	if i+4 > len(b) {
		return 0
	}
	return i + 4
}

// StaticResolver answers every A query with a fixed address and everything
// else with NXDOMAIN. It exists for tests and for air-gapped experiments.
type StaticResolver struct {
	Addr netip.Addr
}

// Exchange implements Resolver.
func (r StaticResolver) Exchange(ctx context.Context, query []byte) ([]byte, string, error) {
	var p dnsmessage.Parser
	hdr, err := p.Start(query)
	if err != nil {
		return nil, "", err
	}
	q, err := p.Question()
	if err != nil {
		return nil, "", err
	}
	rh := dnsmessage.Header{ID: hdr.ID, Response: true, Authoritative: true, RecursionDesired: hdr.RecursionDesired, RecursionAvailable: true}
	b := dnsmessage.NewBuilder(nil, rh)
	b.EnableCompression()
	if err := b.StartQuestions(); err != nil {
		return nil, "", err
	}
	if err := b.Question(q); err != nil {
		return nil, "", err
	}
	if q.Type == dnsmessage.TypeA && r.Addr.Is4() {
		if err := b.StartAnswers(); err != nil {
			return nil, "", err
		}
		if err := b.AResource(dnsmessage.ResourceHeader{Name: q.Name, Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET, TTL: 60}, dnsmessage.AResource{A: r.Addr.As4()}); err != nil {
			return nil, "", err
		}
	} else {
		rh.RCode = dnsmessage.RCodeNameError
	}
	out, err := b.Finish()
	if err != nil {
		return nil, "", fmt.Errorf("build response: %w", err)
	}
	if rh.RCode != 0 {
		out[3] = out[3]&0xF0 | byte(rh.RCode)
	}
	return out, "static", nil
}
