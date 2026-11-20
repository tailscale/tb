// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package guestnet

import (
	"bufio"
	"bytes"
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
	"gvisor.dev/gvisor/pkg/tcpip/header"
)

// anyDialer dials every TCP address to a single local target, standing in
// for the internet in tests.
type anyDialer struct {
	tcpTarget string
}

func (d anyDialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	var nd net.Dialer
	if strings.HasPrefix(network, "tcp") {
		return nd.DialContext(ctx, network, d.tcpTarget)
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

// parseUDPFrame extracts the addressing and payload of an IPv4 UDP frame.
func parseUDPFrame(frame []byte) (src, dst [4]byte, srcPort, dstPort uint16, payload []byte, ok bool) {
	if len(frame) < header.EthernetMinimumSize+header.IPv4MinimumSize+header.UDPMinimumSize {
		return
	}
	eth := header.Ethernet(frame)
	if eth.Type() != header.IPv4ProtocolNumber {
		return
	}
	ip := header.IPv4(frame[header.EthernetMinimumSize:])
	if !ip.IsValid(len(ip)) || ip.Protocol() != uint8(header.UDPProtocolNumber) {
		return
	}
	u := header.UDP(ip.Payload())
	if len(u) < header.UDPMinimumSize {
		return
	}
	return ip.SourceAddress().As4(), ip.DestinationAddress().As4(), u.SourcePort(), u.DestinationPort(), bytes.Clone(u.Payload()), true
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
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// Subscribe before looking at what's already logged, so a close record
	// can't slip in between.
	flows := n.Flows.Subscribe(ctx)
	for _, f := range n.Flows.Recent() {
		if f.Dst == dst && f.Closed() {
			return f
		}
	}
	for {
		select {
		case f := <-flows:
			if f.Dst == dst && f.Closed() {
				return f
			}
		case <-ctx.Done():
			t.Fatalf("no closed flow to %v; flows: %+v", dst, n.Flows.Recent())
			return Flow{}
		}
	}
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

	// A forwarding service whose target is down closes the guest's
	// connection and records why.
	dead, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	deadAddr := dead.Addr().String()
	dead.Close()
	down := netip.AddrPortFrom(DefaultGatewayIP, 9418)
	e.n.SetService(down, &ForwardService{ServiceName: "down", Network: "tcp", Address: deadAddr})
	c3, err := e.guest.DialTCP(ctx, down)
	if err != nil {
		t.Fatalf("guest dial down service: %v", err)
	}
	defer c3.Close()
	io.ReadAll(c3) // until the service closes it
	if f := waitForClosedFlow(t, e.n, down); f.Kind != "service" || f.Service != "down" || f.Err == "" {
		t.Errorf("flow = %+v, want service down with an error", f)
	}
	var dialErr bool
	for _, ev := range e.n.Events.Recent() {
		dialErr = dialErr || ev.Kind == "dial-error"
	}
	if !dialErr {
		t.Errorf("no dial-error event; events: %+v", e.n.Events.Recent())
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
