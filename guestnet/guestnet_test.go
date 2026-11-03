// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package guestnet

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/insomniacslk/dhcp/dhcpv4"
	"github.com/tailscale/tb/guestnet/guestnettest"
	"golang.org/x/net/dns/dnsmessage"
)

// anyDialer dials every address to a single local target, standing in for
// the internet in tests.
type anyDialer struct {
	tcpTarget string
	udpTarget string
}

func (d anyDialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	var nd net.Dialer
	switch {
	case strings.HasPrefix(network, "tcp"):
		return nd.DialContext(ctx, network, d.tcpTarget)
	case strings.HasPrefix(network, "udp"):
		return nd.DialContext(ctx, network, d.udpTarget)
	}
	return nil, fmt.Errorf("unsupported network %q", network)
}

type env struct {
	n     *Net
	guest *guestnettest.Guest
	peer  *MemLink
}

func newEnv(t testing.TB, cfg Config) *env {
	t.Helper()
	host, peer := NewMemLinkPair()
	cfg.Link = host
	cfg.Logf = t.Logf
	n, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { n.Close() })
	g := guestnettest.New(t, peer, guestnettest.Config{
		IP:      n.cfg.GuestIP,
		Subnet:  n.cfg.Subnet,
		Gateway: n.cfg.GatewayIP,
		MAC:     n.cfg.GuestMAC,
	})
	return &env{n: n, guest: g, peer: peer}
}

func testContext(t testing.TB) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// dhcpExchange writes a DHCP message from an addressless client and returns
// the reply.
func dhcpExchange(t testing.TB, peer *MemLink, mac net.HardwareAddr, msg *dhcpv4.DHCPv4) *dhcpv4.DHCPv4 {
	t.Helper()
	frame := buildUDPFrame(mac, net.HardwareAddr{0xff, 0xff, 0xff, 0xff, 0xff, 0xff}, [4]byte{}, [4]byte{255, 255, 255, 255}, dhcpv4.ClientPort, dhcpv4.ServerPort, msg.ToBytes())
	if err := peer.WriteFrame(frame); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 65536)
	deadline := time.After(5 * time.Second)
	for {
		select {
		case <-deadline:
			t.Fatal("timed out waiting for DHCP reply")
		default:
		}
		n, err := peer.ReadFrame(buf)
		if err != nil {
			t.Fatal(err)
		}
		_, _, srcPort, dstPort, payload, ok := parseUDPFrame(buf[:n])
		if !ok || srcPort != dhcpv4.ServerPort || dstPort != dhcpv4.ClientPort {
			continue
		}
		reply, err := dhcpv4.FromBytes(payload)
		if err != nil {
			t.Fatalf("bad DHCP reply: %v", err)
		}
		return reply
	}
}

func TestDHCP(t *testing.T) {
	host, peer := NewMemLinkPair()
	n, err := New(Config{Link: host, Logf: t.Logf, MTU: 9000})
	if err != nil {
		t.Fatal(err)
	}
	defer n.Close()

	mac := DefaultGuestMAC
	disc, err := dhcpv4.NewDiscovery(mac, dhcpv4.WithBroadcast(true))
	if err != nil {
		t.Fatal(err)
	}
	offer := dhcpExchange(t, peer, mac, disc)
	if offer.MessageType() != dhcpv4.MessageTypeOffer {
		t.Fatalf("got %v, want OFFER", offer.MessageType())
	}
	if got := offer.YourIPAddr.String(); got != DefaultGuestIP.String() {
		t.Errorf("yiaddr = %s, want %v", got, DefaultGuestIP)
	}
	if got := offer.Router(); len(got) != 1 || got[0].String() != DefaultGatewayIP.String() {
		t.Errorf("router = %v, want %v", got, DefaultGatewayIP)
	}
	if got := offer.DNS(); len(got) != 1 || got[0].String() != DNSAddr.String() {
		t.Errorf("dns = %v, want %v", got, DNSAddr)
	}
	if mtu := offer.Options.Get(dhcpv4.OptionInterfaceMTU); len(mtu) != 2 || binary.BigEndian.Uint16(mtu) != 9000 {
		t.Errorf("mtu option = %x, want 9000", mtu)
	}
	if lt := offer.IPAddressLeaseTime(0); lt != 24*time.Hour {
		t.Errorf("lease = %v, want 24h", lt)
	}

	select {
	case <-n.Leased():
		t.Fatal("Leased closed after OFFER; should wait for ACK")
	default:
	}

	req, err := dhcpv4.NewRequestFromOffer(offer, dhcpv4.WithBroadcast(true))
	if err != nil {
		t.Fatal(err)
	}
	ack := dhcpExchange(t, peer, mac, req)
	if ack.MessageType() != dhcpv4.MessageTypeAck {
		t.Fatalf("got %v, want ACK", ack.MessageType())
	}
	select {
	case <-n.Leased():
	case <-time.After(time.Second):
		t.Fatal("Leased not closed after ACK")
	}

	// A request for a foreign address is refused.
	bad, err := dhcpv4.NewRequestFromOffer(offer, dhcpv4.WithBroadcast(true), dhcpv4.WithOption(dhcpv4.OptRequestedIPAddress(net.IPv4(10, 1, 2, 3))))
	if err != nil {
		t.Fatal(err)
	}
	if nak := dhcpExchange(t, peer, mac, bad); nak.MessageType() != dhcpv4.MessageTypeNak {
		t.Errorf("got %v for foreign address, want NAK", nak.MessageType())
	}

	var kinds []string
	for _, ev := range n.Events.Recent() {
		kinds = append(kinds, ev.Kind)
	}
	if got := strings.Join(kinds, ","); got != "dhcp-discover,dhcp-lease" {
		t.Errorf("events = %q", got)
	}
}

func TestTCPEgress(t *testing.T) {
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "hello %s", r.Host)
	})}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go srv.Serve(ln)

	e := newEnv(t, Config{Dialer: anyDialer{tcpTarget: ln.Addr().String()}})
	ctx := testContext(t)

	dst := netip.MustParseAddrPort("93.184.216.34:80")
	c, err := e.guest.DialTCP(ctx, dst)
	if err != nil {
		t.Fatalf("guest dial: %v", err)
	}
	defer c.Close()
	fmt.Fprintf(c, "GET / HTTP/1.1\r\nHost: example.com\r\nConnection: close\r\n\r\n")
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "hello example.com" {
		t.Fatalf("body = %q", body)
	}
	c.Close()

	flows := waitForClosedFlow(t, e.n, dst)
	if flows.Kind != "egress" {
		t.Errorf("kind = %q, want egress", flows.Kind)
	}
	if flows.BytesOut == 0 || flows.BytesIn == 0 {
		t.Errorf("bytes in/out = %d/%d, want both > 0", flows.BytesIn, flows.BytesOut)
	}
	if flows.Src.Addr() != DefaultGuestIP {
		t.Errorf("src = %v, want guest IP", flows.Src)
	}
}

func waitForClosedFlow(t testing.TB, n *Net, dst netip.AddrPort) Flow {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, f := range n.Flows.Recent() {
			if f.Dst == dst && f.Closed() {
				return f
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("no closed flow to %v; flows: %+v", dst, n.Flows.Recent())
	return Flow{}
}

func TestTCPBlocked(t *testing.T) {
	e := newEnv(t, Config{Dialer: anyDialer{}})
	ctx := testContext(t)
	for _, dst := range []netip.AddrPort{
		netip.MustParseAddrPort("100.100.100.100:80"), // tailnet CGNAT
		netip.MustParseAddrPort("192.168.64.7:80"),    // guest subnet, nonexistent
		netip.MustParseAddrPort("192.168.64.1:80"),    // gateway with no service
	} {
		c, err := e.guest.DialTCP(ctx, dst)
		if err == nil {
			c.Close()
			t.Errorf("dial %v succeeded, want refusal", dst)
			continue
		}
		f := waitForClosedFlow(t, e.n, dst)
		if f.Kind != "blocked" {
			t.Errorf("%v: kind = %q, want blocked", dst, f.Kind)
		}
	}
}

func TestTCPDialError(t *testing.T) {
	// Point egress at a closed port so the host-side dial fails; the guest
	// must see a refused connection, not an accepted one.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	e := newEnv(t, Config{Dialer: anyDialer{tcpTarget: addr}})
	ctx := testContext(t)
	dst := netip.MustParseAddrPort("93.184.216.34:443")
	c, err := e.guest.DialTCP(ctx, dst)
	if err == nil {
		c.Close()
		t.Fatal("dial succeeded, want refusal")
	}
	f := waitForClosedFlow(t, e.n, dst)
	if f.Kind != "egress" || f.Err == "" {
		t.Errorf("flow = %+v, want egress with error", f)
	}
}

func TestService(t *testing.T) {
	imds := NewListenerService("imds")
	go http.Serve(imds, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "meta for %s", r.RemoteAddr)
	}))
	defer imds.Close()

	e := newEnv(t, Config{
		Dialer:   anyDialer{},
		Services: map[netip.AddrPort]Service{netip.AddrPortFrom(IMDSAddr, 80): imds},
	})
	ctx := testContext(t)
	dst := netip.AddrPortFrom(IMDSAddr, 80)
	c, err := e.guest.DialTCP(ctx, dst)
	if err != nil {
		t.Fatalf("guest dial IMDS: %v", err)
	}
	defer c.Close()
	fmt.Fprintf(c, "GET /v0/meta HTTP/1.1\r\nHost: 169.254.169.254\r\nConnection: close\r\n\r\n")
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.HasPrefix(string(body), "meta for 192.168.64.2:") {
		t.Fatalf("body = %q", body)
	}
	c.Close()
	f := waitForClosedFlow(t, e.n, dst)
	if f.Kind != "service" || f.Service != "imds" {
		t.Errorf("flow = %+v, want service imds", f)
	}

	// A late-registered forwarding service on the gateway works too.
	echo, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer echo.Close()
	go func() {
		for {
			c, err := echo.Accept()
			if err != nil {
				return
			}
			go func() { io.Copy(c, c); c.Close() }()
		}
	}()
	nfs := netip.AddrPortFrom(DefaultGatewayIP, 2049)
	e.n.SetService(nfs, &ForwardService{ServiceName: "nfs", Network: "tcp", Address: echo.Addr().String()})
	c2, err := e.guest.DialTCP(ctx, nfs)
	if err != nil {
		t.Fatalf("guest dial nfs: %v", err)
	}
	defer c2.Close()
	if _, err := c2.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4)
	if _, err := io.ReadFull(c2, buf); err != nil || string(buf) != "ping" {
		t.Fatalf("echo = %q, %v", buf, err)
	}
}

func TestDNS(t *testing.T) {
	e := newEnv(t, Config{Dialer: anyDialer{}, Resolver: StaticResolver{Addr: netip.MustParseAddr("10.9.8.7")}})

	// Guest queries a public resolver; we intercept regardless of address.
	dst := netip.MustParseAddrPort("8.8.8.8:53")
	c, err := e.guest.DialUDP(dst)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	q := dnsmessage.Message{
		Header:    dnsmessage.Header{ID: 0xBEEF, RecursionDesired: true},
		Questions: []dnsmessage.Question{{Name: dnsmessage.MustNewName("example.com."), Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET}},
	}
	qb, err := q.Pack()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Write(qb); err != nil {
		t.Fatal(err)
	}
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 4096)
	n, err := c.Read(buf)
	if err != nil {
		t.Fatalf("read DNS response: %v", err)
	}
	var m dnsmessage.Message
	if err := m.Unpack(buf[:n]); err != nil {
		t.Fatal(err)
	}
	if m.ID != 0xBEEF || len(m.Answers) != 1 {
		t.Fatalf("response = %+v", m)
	}
	if a, ok := m.Answers[0].Body.(*dnsmessage.AResource); !ok || a.A != [4]byte{10, 9, 8, 7} {
		t.Fatalf("answer = %+v", m.Answers[0].Body)
	}

	// Also over TCP, to the advertised resolver address.
	tc, err := e.guest.DialTCP(testContext(t), netip.AddrPortFrom(DNSAddr, 53))
	if err != nil {
		t.Fatal(err)
	}
	defer tc.Close()
	if err := writeDNSTCP(tc, qb); err != nil {
		t.Fatal(err)
	}
	tc.SetReadDeadline(time.Now().Add(5 * time.Second))
	resp, err := readDNSTCP(tc)
	if err != nil {
		t.Fatalf("TCP DNS: %v", err)
	}
	if err := m.Unpack(resp); err != nil || len(m.Answers) != 1 {
		t.Fatalf("TCP response = %+v, %v", m, err)
	}

	events := e.n.DNS.Recent()
	if len(events) != 2 {
		t.Fatalf("got %d DNS events, want 2: %+v", len(events), events)
	}
	for _, ev := range events {
		if ev.Name != "example.com." || ev.Type != "TypeA" || ev.Answers != 1 || ev.Upstream != "static" {
			t.Errorf("event = %+v", ev)
		}
	}
	if events[0].Proto != "udp" || events[1].Proto != "tcp" {
		t.Errorf("protos = %s, %s", events[0].Proto, events[1].Proto)
	}
}

func TestNTP(t *testing.T) {
	now := time.Date(2026, 9, 29, 12, 0, 0, 500_000_000, time.UTC)
	e := newEnv(t, Config{Dialer: anyDialer{}, Now: func() time.Time { return now }})
	c, err := e.guest.DialUDP(netip.MustParseAddrPort("17.253.2.1:123"))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	req := make([]byte, 48)
	req[0] = 4<<3 | 3 // v4, client
	binary.BigEndian.PutUint64(req[40:], 0x1122334455667788)
	if _, err := c.Write(req); err != nil {
		t.Fatal(err)
	}
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	resp := make([]byte, 64)
	n, err := c.Read(resp)
	if err != nil {
		t.Fatal(err)
	}
	if n != 48 || resp[0]&0x07 != 4 {
		t.Fatalf("resp len %d mode %d", n, resp[0]&0x07)
	}
	if binary.BigEndian.Uint64(resp[24:]) != 0x1122334455667788 {
		t.Error("originate timestamp not echoed")
	}
	ts := binary.BigEndian.Uint64(resp[40:])
	secs := int64(ts>>32) - ntpEpochOffset
	frac := float64(ts&0xffffffff) / (1 << 32)
	got := time.Unix(secs, int64(frac*1e9))
	if d := got.Sub(now); d < -time.Millisecond || d > time.Millisecond {
		t.Errorf("transmit = %v, want %v", got, now)
	}
}

func TestUDPEgress(t *testing.T) {
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	go func() {
		buf := make([]byte, 1500)
		for {
			n, addr, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			pc.WriteTo(append([]byte("echo:"), buf[:n]...), addr)
		}
	}()
	e := newEnv(t, Config{Dialer: anyDialer{udpTarget: pc.LocalAddr().String()}})
	dst := netip.MustParseAddrPort("203.0.113.9:4000")
	c, err := e.guest.DialUDP(dst)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.Write([]byte("hi")); err != nil {
		t.Fatal(err)
	}
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 100)
	n, err := c.Read(buf)
	if err != nil || string(buf[:n]) != "echo:hi" {
		t.Fatalf("got %q, %v", buf[:n], err)
	}
	var open bool
	for _, f := range e.n.Flows.Recent() {
		if f.Dst == dst && f.Kind == "egress" {
			open = true
		}
	}
	if !open {
		t.Errorf("no egress flow record for %v", dst)
	}
}

func TestDialGuest(t *testing.T) {
	e := newEnv(t, Config{Dialer: anyDialer{}})
	ln, err := e.guest.ListenTCP(22)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		fmt.Fprint(c, "SSH-2.0-fake\r\n")
		c.Close()
	}()
	ctx := testContext(t)
	c, err := e.n.DialGuest(ctx, 22)
	if err != nil {
		t.Fatalf("DialGuest: %v", err)
	}
	defer c.Close()
	line, err := bufio.NewReader(c).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if line != "SSH-2.0-fake\r\n" {
		t.Fatalf("banner = %q", line)
	}
	if !strings.HasPrefix(c.LocalAddr().String(), DefaultGatewayIP.String()+":") {
		t.Errorf("local addr = %v, want gateway", c.LocalAddr())
	}
}

func TestCloseUnblocks(t *testing.T) {
	host, _ := NewMemLinkPair()
	n, err := New(Config{Link: host})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- n.Close() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close hung")
	}
	if _, err := New(Config{}); !errors.Is(err, err) || err == nil {
		t.Fatal("New without a link should fail")
	}
}
