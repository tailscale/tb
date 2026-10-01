// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package rendezvous

import (
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"testing"
	"testing/synctest"
	"time"

	"github.com/tailscale/tb/lansport"
	"github.com/tailscale/tb/lansport/lansporttest"
)

// probeInterval is the lansport probe interval the tests configure. Under
// synctest it is virtual.
const probeInterval = time.Second

// sleepAndWait advances the bubble's clock by d and then waits for every
// goroutine the passage of time woke to finish what it was doing, so the
// caller can assert on the resulting state. Callers say what d is for.
func sleepAndWait(d time.Duration) {
	time.Sleep(d)
	synctest.Wait()
}

// newServer starts a lansport server named name at ip on lan whose static
// peers are at the given advert addresses, and closes it when the test ends.
func newServer(t testing.TB, lan *lansporttest.LAN, name string, ip netip.Addr, peerAdverts ...string) *lansport.Server {
	t.Helper()
	s, err := lansport.Listen(lansport.Config{
		Source:         lansport.StaticSource(name, peerAdverts...),
		Name:           name,
		AdvertListener: lan.Listen(net.JoinHostPort(ip.String(), "7890")),
		TLSListener:    lan.Listen(net.JoinHostPort(ip.String(), "0")),
		LANIP:          ip,
		Dial:           lan.DialFrom(ip),
		Handler:        http.NotFoundHandler(),
		ProbeInterval:  probeInterval,
		Logf:           func(f string, args ...any) { t.Logf(name+": "+f, args...) },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// keysOwnedBy returns up to n keys that r assigns to the member named owner,
// or reports as the local process's if owner is "".
func keysOwnedBy(r *Router, owner string, n int) []string {
	var out []string
	for i := 0; len(out) < n && i < 10000; i++ {
		key := fmt.Sprintf("key-%d", i)
		p, ok := r.Pick(key)
		if (owner == "" && !ok) || (ok && p.Name == owner) {
			out = append(out, key)
		}
	}
	return out
}

// TestRouterHysteresis checks the join and leave delays: a newly reachable
// peer owns nothing until JoinDelay has passed, a peer that goes away keeps
// its keys (handled locally meanwhile) until LeaveDelay has passed, and one
// that comes back within LeaveDelay resumes as if nothing happened.
func TestRouterHysteresis(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const joinDelay, leaveDelay = 5 * time.Second, 30 * time.Second
		lan := new(lansporttest.LAN)
		ipA, ipB := netip.MustParseAddr("127.0.0.1"), netip.MustParseAddr("127.0.0.2")
		advA, advB := "127.0.0.1:7890", "127.0.0.2:7890"
		a := newServer(t, lan, "a", ipA, advB)
		// Let a's probe at start run before b exists, so that a first
		// reaches b at its next probe, one interval in, rather than at
		// start or one interval in depending on goroutine scheduling. The
		// timing assertions below count from there.
		synctest.Wait()
		b := newServer(t, lan, "b", ipB, advA)
		r := NewRouter(a, RouterConfig{JoinDelay: joinDelay, LeaveDelay: leaveDelay})
		defer r.Close()

		// a reaches b at its first probe after b started, one interval in
		// (see the lansport tests for why); b takes one more to reach a. b
		// is then reachable but not yet adopted.
		sleepAndWait(2 * probeInterval)
		if len(a.Peers()) != 1 {
			t.Fatalf("a's peers = %v, want b", a.Peers())
		}
		if got := r.Members(); !slices.Equal(got, []string{"a"}) {
			t.Fatalf("members right after b appeared = %v, want [a]", got)
		}
		if got := keysOwnedBy(r, "b", 1); len(got) != 0 {
			t.Errorf("b owns %v before adoption", got)
		}

		// Adoption is due JoinDelay after a first saw b, which was one
		// interval in. One interval short of that, still not adopted; at
		// it, adopted.
		sleepAndWait(joinDelay - 2*probeInterval)
		if got := r.Members(); !slices.Equal(got, []string{"a"}) {
			t.Fatalf("members before JoinDelay = %v, want [a]", got)
		}
		sleepAndWait(probeInterval)
		if got := r.Members(); !slices.Equal(got, []string{"a", "b"}) {
			t.Fatalf("members after JoinDelay = %v, want [a b]", got)
		}
		bKeys := keysOwnedBy(r, "b", 3)
		if len(bKeys) != 3 {
			t.Fatalf("b owns no keys after adoption")
		}

		// b goes away. One probe interval later a has noticed; b keeps
		// its keys, which the local process handles meanwhile.
		b.Close()
		sleepAndWait(probeInterval)
		if len(a.Peers()) != 0 {
			t.Fatalf("a still sees %v after b closed", a.Peers())
		}
		if got := r.Members(); !slices.Equal(got, []string{"a", "b"}) {
			t.Fatalf("members right after b left = %v, want [a b] during LeaveDelay", got)
		}
		for _, key := range bKeys {
			if p, ok := r.Pick(key); ok {
				t.Errorf("Pick(%s) during b's LeaveDelay = %v, want local", key, p.Name)
			}
		}

		// b comes back (same name, so the same routing key) well within
		// LeaveDelay: nothing moved, and its keys route to it again as
		// soon as it is reachable, with no JoinDelay since it was never
		// dropped.
		b = newServer(t, lan, "b", ipB, advA)
		sleepAndWait(2 * probeInterval)
		if got := r.Members(); !slices.Equal(got, []string{"a", "b"}) {
			t.Fatalf("members after b returned = %v, want [a b]", got)
		}
		for _, key := range bKeys {
			if p, ok := r.Pick(key); !ok || p.Name != "b" {
				t.Errorf("Pick(%s) after b returned = %v, %v; want b", key, p.Name, ok)
			}
		}

		// b goes away for good: LeaveDelay after a notices, its keys are
		// reassigned, which with two members means they are a's.
		b.Close()
		sleepAndWait(probeInterval)
		sleepAndWait(leaveDelay - probeInterval)
		if got := r.Members(); !slices.Equal(got, []string{"a", "b"}) {
			t.Fatalf("members just before LeaveDelay = %v, want [a b]", got)
		}
		sleepAndWait(probeInterval)
		if got := r.Members(); !slices.Equal(got, []string{"a"}) {
			t.Fatalf("members after LeaveDelay = %v, want [a]", got)
		}
	})
}

// TestRouterNoDelays checks that zero delays mean immediate adoption and
// reassignment, which is what callers that want the old behavior, and
// tests, get.
func TestRouterNoDelays(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		lan := new(lansporttest.LAN)
		ipA, ipB := netip.MustParseAddr("127.0.0.1"), netip.MustParseAddr("127.0.0.2")
		a := newServer(t, lan, "a", ipA, "127.0.0.2:7890")
		b := newServer(t, lan, "b", ipB, "127.0.0.1:7890")
		r := NewRouter(a, RouterConfig{})
		defer r.Close()

		sleepAndWait(2 * probeInterval) // for a and b to reach each other
		if got := r.Members(); !slices.Equal(got, []string{"a", "b"}) {
			t.Fatalf("members = %v, want [a b]", got)
		}
		b.Close()
		sleepAndWait(probeInterval) // for a's next probe to drop b
		if got := r.Members(); !slices.Equal(got, []string{"a"}) {
			t.Fatalf("members after b closed = %v, want [a]", got)
		}
	})
}
