// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package lansport

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
	"testing/synctest"
	"time"

	"github.com/tailscale/tb/lansport/lansporttest"
	"github.com/tailscale/tb/tagpeers"
	"github.com/tailscale/tb/tagpeers/tagpeerstest"
)

// probeInterval is the probe interval the tests configure. Under synctest
// it is virtual, so it costs nothing to wait out.
const probeInterval = time.Second

// rounds lets n probe rounds happen and everything they trigger settle.
func rounds(n int) {
	for range n {
		time.Sleep(probeInterval)
		synctest.Wait()
	}
}

var (
	ipA = netip.MustParseAddr("127.0.0.1")
	ipB = netip.MustParseAddr("127.0.0.2")
)

// echoName is a handler that answers with the name it was given, so tests
// can tell which server they reached through a transport, and which peer
// the server thought sent the request.
func echoName(name string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		from, _ := FromPeer(r)
		fmt.Fprintf(w, "%s saw %s", name, from.Name)
	})
}

// newServer starts a lansport server named name at ip on lan, with its
// advert listener at advertAddr, and closes it when the test ends.
func newServer(t testing.TB, lan *lansporttest.LAN, name string, ip netip.Addr, advertAddr string, src Source) *Server {
	t.Helper()
	s, err := Listen(Config{
		Source:         src,
		Name:           name,
		AdvertListener: lan.Listen(advertAddr),
		TLSListener:    lan.Listen(net.JoinHostPort(ip.String(), "0")),
		LANIP:          ip,
		Dial:           lan.DialFrom(ip),
		Handler:        echoName(name),
		ProbeInterval:  probeInterval,
		Logf:           func(f string, args ...any) { t.Logf(name+": "+f, args...) },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// newStaticPair starts two servers named a and b that know each other
// through static sources and lets them reach each other. a is on ipA, b on
// ipB, both with adverts on port 7890.
func newStaticPair(t testing.TB, lan *lansporttest.LAN) (a, b *Server) {
	t.Helper()
	advA, advB := net.JoinHostPort(ipA.String(), "7890"), net.JoinHostPort(ipB.String(), "7890")
	a = newServer(t, lan, "a", ipA, advA, StaticSource("a", advB))
	b = newServer(t, lan, "b", ipB, advB, StaticSource("b", advA))
	// Round 0 runs at Listen: a can't reach b's advert yet (b didn't
	// exist), b fetches a's advert but a doesn't know b's pin. Round 1: a
	// fetches b's advert and its LAN check passes, since b knows a. b's
	// LAN check races a's fetch, so allow one more round for it.
	rounds(2)
	if len(a.Peers()) != 1 || len(b.Peers()) != 1 {
		t.Fatalf("after two rounds: a sees %v, b sees %v", a.Peers(), b.Peers())
	}
	return a, b
}

// get fetches path from peer p and returns the status and body.
func get(t testing.TB, p Peer, path string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest("GET", p.URL(path), nil)
	res, err := p.Transport.RoundTrip(req)
	if err != nil {
		t.Fatalf("GET %s: %v", p.URL(path), err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(body)
}

func TestStaticPair(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		lan := new(lansporttest.LAN)
		a, b := newStaticPair(t, lan)

		// Static peers are keyed by their advertised names, matching what
		// each side uses for itself, so routing tables agree.
		pb, ok := a.Peers()["b"]
		if !ok {
			t.Fatalf("a's peers = %v, want b", a.Peers())
		}
		if a.Self().Key != "a" || pb.Weight != 1 || pb.Addr != b.TLSAddr().String() {
			t.Errorf("a.Self = %+v, a's view of b = %+v", a.Self(), pb)
		}

		// Requests through the transport reach the peer's handler, which
		// knows who sent them.
		if code, body := get(t, pb, "/hello"); code != 200 || body != "b saw a" {
			t.Errorf("GET via transport: %d %q", code, body)
		}

		// Anyone else on the LAN gets nothing from the TLS port, not even
		// the advert.
		rogue, _ := NewIdentity("rogue")
		c := &http.Client{Transport: &http.Transport{
			DialContext:     lan.DialFrom(netip.MustParseAddr("127.0.0.9")),
			TLSClientConfig: rogue.ClientTLSConfig(nil),
		}}
		for _, path := range []string{AdvertPath, "/hello"} {
			res, err := c.Get("https://" + b.TLSAddr().String() + path)
			if err != nil {
				t.Fatal(err)
			}
			res.Body.Close()
			if res.StatusCode != http.StatusForbidden {
				t.Errorf("rogue GET %s on TLS port: %d, want 403", path, res.StatusCode)
			}
		}
		c.CloseIdleConnections()

		// A peer going away drops out at the next round.
		b.Close()
		rounds(1)
		if len(a.Peers()) != 0 {
			t.Errorf("a still sees %v after b closed", a.Peers())
		}
		snap := a.Snapshot()
		if len(snap) != 1 || snap[0].Reachable || snap[0].AdvertErr == "" {
			t.Errorf("snapshot after b left = %+v", snap)
		}
	})
}

// TestRestartRepins covers a peer restarting: a new certificate behind the
// same addresses is detected through the advert, the transport is rebuilt,
// and requests flow again.
func TestRestartRepins(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		lan := new(lansporttest.LAN)
		a, b := newStaticPair(t, lan)
		oldTransport := a.Peers()["b"].Transport
		advB, tlsB := b.AdvertAddr().String(), b.TLSAddr().String()
		b.Close()
		rounds(1)
		if len(a.Peers()) != 0 {
			t.Fatalf("a still sees %v after b closed", a.Peers())
		}

		b2, err := Listen(Config{
			Source:         StaticSource("b", a.AdvertAddr().String()),
			Name:           "b",
			AdvertListener: lan.Listen(advB),
			TLSListener:    lan.Listen(tlsB),
			LANIP:          ipB,
			Dial:           lan.DialFrom(ipB),
			Handler:        echoName("b2"),
			ProbeInterval:  probeInterval,
			Logf:           t.Logf,
		})
		if err != nil {
			t.Fatal(err)
		}
		defer b2.Close()
		rounds(2)
		pb, ok := a.Peers()["b"]
		if !ok {
			t.Fatalf("a doesn't see the restarted b: %v", a.Peers())
		}
		if pb.Transport == oldTransport {
			t.Error("transport not rebuilt after restart")
		}
		if code, body := get(t, pb, "/x"); code != 200 || body != "b2 saw a" {
			t.Errorf("GET after restart: %d %q", code, body)
		}
		for _, st := range a.Snapshot() {
			if st.Rebuilds != 2 {
				t.Errorf("rebuilds = %d, want 2 (initial and after restart)", st.Rebuilds)
			}
		}
	})
}

// TestLANPathBroken covers an advert that is fetched fine over the trusted
// path but names a LAN address nothing answers at: the peer is not
// reachable, and the snapshot says why.
func TestLANPathBroken(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		lan := new(lansporttest.LAN)
		liar, _ := NewIdentity("liar")
		liarLn := lan.Listen("127.0.0.3:7890")
		liarSrv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprintf(w, `{"name":"liar","ipPort":"127.0.0.3:1","tlsCertHash":%q,"weight":1}`, liar.Pin())
		})}
		go liarSrv.Serve(liarLn)
		defer liarSrv.Close()

		a := newServer(t, lan, "a", ipA, "127.0.0.1:7890", StaticSource("a", liarLn.Addr().String()))
		synctest.Wait() // round 0
		snap := a.Snapshot()
		if len(snap) != 1 || snap[0].Advert == nil || snap[0].LANErr == "" || snap[0].Reachable {
			t.Errorf("snapshot = %+v", snap)
		}
		if len(a.Peers()) != 0 {
			t.Error("peer with broken LAN path is reachable")
		}
	})
}

// TestAdvertUntrusted checks that the advert port refuses addresses the
// source doesn't vouch for.
func TestAdvertUntrusted(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		lan := new(lansporttest.LAN)
		a, _ := newStaticPair(t, lan)
		for _, tt := range []struct {
			from string
			want int
		}{
			{"127.0.0.2", http.StatusOK},        // b's host, in a's static list
			{"127.0.0.9", http.StatusForbidden}, // nobody a knows
		} {
			c := &http.Client{Transport: &http.Transport{DialContext: lan.DialFrom(netip.MustParseAddr(tt.from))}}
			res, err := c.Get("http://" + a.AdvertAddr().String() + AdvertPath)
			if err != nil {
				t.Fatal(err)
			}
			res.Body.Close()
			c.CloseIdleConnections()
			if res.StatusCode != tt.want {
				t.Errorf("advert fetched from %s: %d, want %d", tt.from, res.StatusCode, tt.want)
			}
		}
	})
}

// TestTailnetSource runs two servers whose peers come from fake tailscaleds
// through TailnetSource: adverts are fetched at the nodes' tailnet addresses
// on a shared advert port, and routing keys are the stable node IDs.
func TestTailnetSource(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const tag = "tag:test"
		lan := new(lansporttest.LAN)
		nodeA := tagpeerstest.Node{ID: 1, Name: "a", IP: ipA, Tags: []string{tag}, Online: true}
		nodeB := tagpeerstest.Node{ID: 2, Name: "b", IP: ipB, Tags: []string{tag}, Online: true}
		tsA := tagpeerstest.New(t, nodeA, nodeB)
		tsB := tagpeerstest.New(t, nodeB, nodeA)

		mk := func(name string, ts *tagpeerstest.Tailscaled, ip netip.Addr) *Server {
			tr, err := tagpeers.Start(context.Background(), tagpeers.Config{Tag: tag, Dial: ts.Dial, Logf: t.Logf})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(tr.Close)
			return newServer(t, lan, name, ip, net.JoinHostPort(ip.String(), "7890"), TailnetSource(tr, 7890))
		}
		a := mk("a", tsA, ipA)
		b := mk("b", tsB, ipB)
		rounds(2)
		pb, ok := a.Peers()["stable-2"]
		if !ok || pb.Name != "b" || pb.Addr != b.TLSAddr().String() {
			t.Fatalf("a's peers = %+v", a.Peers())
		}
		if a.Self().Key != "stable-1" {
			t.Errorf("a.Self().Key = %q", a.Self().Key)
		}
		if len(b.Peers()) != 1 {
			t.Errorf("b's peers = %+v", b.Peers())
		}
		if code, body := get(t, pb, "/y"); code != 200 || body != "b saw a" {
			t.Errorf("GET via tailnet-sourced transport: %d %q", code, body)
		}

		// Losing the tag drops the peer at once: the tracker signals, the
		// server re-probes, and b is no longer a candidate.
		untagged := nodeB
		untagged.Tags = []string{"tag:other"}
		tsA.ChangeNode(untagged)
		synctest.Wait()
		if len(a.Peers()) != 0 || len(a.Snapshot()) != 0 {
			t.Errorf("after untagging b: peers %v, snapshot %+v", a.Peers(), a.Snapshot())
		}
	})
}

// TestIdentityPinning checks the TLS configurations: a pinned client
// accepts the pinned server and rejects another, the server learns the
// client's pin, and an unpinned client reports the server's pin.
func TestIdentityPinning(t *testing.T) {
	server, err := NewIdentity("server")
	if err != nil {
		t.Fatal(err)
	}
	client, err := NewIdentity("client")
	if err != nil {
		t.Fatal(err)
	}
	if server.Pin() == client.Pin() {
		t.Fatal("two identities share a pin")
	}
	if p, err := ParsePin(server.Pin().String()); err != nil || p != server.Pin() {
		t.Errorf("ParsePin round trip: %v, %v", p, err)
	}

	var gotClientPin Pin
	hs := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotClientPin, _ = RequestPin(r)
	}))
	hs.TLS = server.ServerTLSConfig()
	hs.Config.ErrorLog = log.New(io.Discard, "", 0) // the wrong-pin handshake below is expected to fail
	hs.StartTLS()
	defer hs.Close()

	c := &http.Client{Transport: &http.Transport{TLSClientConfig: client.ClientTLSConfig(nil)}}
	res, err := c.Get(hs.URL)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if pin, ok := ResponsePin(res); !ok || pin != server.Pin() {
		t.Errorf("ResponsePin = %v, %v; want %v", pin, ok, server.Pin())
	}
	if gotClientPin != client.Pin() {
		t.Errorf("server saw client pin %v, want %v", gotClientPin, client.Pin())
	}

	want := server.Pin()
	c = &http.Client{Transport: &http.Transport{TLSClientConfig: client.ClientTLSConfig(&want)}}
	if res, err := c.Get(hs.URL); err != nil {
		t.Errorf("pinned GET: %v", err)
	} else {
		res.Body.Close()
	}

	wrong := client.Pin()
	d := tls.Dialer{Config: client.ClientTLSConfig(&wrong)}
	if conn, err := d.Dial("tcp", hs.Listener.Addr().String()); err == nil {
		conn.Close()
		t.Error("connection with the wrong pin succeeded")
	} else if !errors.Is(err, ErrPinMismatch) {
		t.Errorf("wrong pin: %v, want ErrPinMismatch", err)
	}
}
