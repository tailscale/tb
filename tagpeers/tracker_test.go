// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package tagpeers

import (
	"context"
	"net/netip"
	"slices"
	"testing"
	"testing/synctest"
	"time"

	"github.com/tailscale/tb/tagpeers/tagpeerstest"
	"tailscale.com/tailcfg"
)

const testTag = "tag:test-pool"

var (
	self     = tagpeerstest.Node{ID: 1, Name: "self", IP: netip.MustParseAddr("100.64.0.1"), Tags: []string{testTag}, Online: true}
	nodeB    = tagpeerstest.Node{ID: 2, Name: "b", IP: netip.MustParseAddr("100.64.0.2"), Tags: []string{testTag}, Online: true}
	untagged = tagpeerstest.Node{ID: 3, Name: "untagged", IP: netip.MustParseAddr("100.64.0.3"), Tags: []string{"tag:other"}, Online: true}
	offline  = tagpeerstest.Node{ID: 4, Name: "offline", IP: netip.MustParseAddr("100.64.0.4"), Tags: []string{testTag}, Online: false}
)

// ids returns the tracked node IDs, sorted.
func ids(tr *Tracker) []tailcfg.NodeID {
	var out []tailcfg.NodeID
	for _, n := range tr.Nodes() {
		out = append(out, n.ID)
	}
	return out
}

// expectSignal fails unless a change signal is pending on ch. It is called
// after synctest.Wait, so the tracker has already done everything it will.
func expectSignal(t testing.TB, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	default:
		t.Errorf("no change signal after %s", what)
	}
}

// expectNoSignal fails if a change signal is pending on ch.
func expectNoSignal(t testing.TB, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
		t.Errorf("unexpected change signal after %s", what)
	default:
	}
}

func TestTracker(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := tagpeerstest.New(t, self, nodeB, untagged, offline)
		tr, err := Start(context.Background(), Config{Tag: testTag, Dial: f.Dial, Logf: t.Logf})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(tr.Close)
		changes := tr.Subscribe()

		synctest.Wait()
		if !tr.State().Connected {
			t.Fatalf("not connected: %+v", tr.State())
		}
		expectSignal(t, changes, "initial status")
		if got, want := ids(tr), []tailcfg.NodeID{2, 4}; !slices.Equal(got, want) {
			t.Fatalf("nodes = %v, want %v", got, want)
		}
		s, ok := tr.Self()
		if !ok || s.ID != 1 || s.Hostname != "self" {
			t.Errorf("self = %+v, %v", s, ok)
		}
		nodes := tr.Nodes()
		if nodes[0].Hostname != "b" || !nodes[0].Online || nodes[1].Hostname != "offline" || nodes[1].Online {
			t.Errorf("nodes = %+v", nodes)
		}
		if ip, _ := nodes[0].IP(); ip != nodeB.IP {
			t.Errorf("b's IP = %v", ip)
		}

		// Coming online is a change worth signaling.
		on := offline
		on.Online = true
		f.ChangeNode(on)
		synctest.Wait()
		expectSignal(t, changes, "node coming online")
		if !tr.Nodes()[1].Online {
			t.Error("offline node still offline")
		}

		// Endpoint churn arrives as an identical full node and is not.
		f.ChangeNode(on)
		synctest.Wait()
		expectNoSignal(t, changes, "unchanged node")

		// Losing the tag drops the node; regaining it brings it back.
		retagged := nodeB
		retagged.Tags = []string{"tag:other"}
		f.ChangeNode(retagged)
		synctest.Wait()
		expectSignal(t, changes, "tag removal")
		if got := ids(tr); !slices.Equal(got, []tailcfg.NodeID{4}) {
			t.Errorf("nodes after tag removal = %v", got)
		}
		f.ChangeNode(nodeB)
		synctest.Wait()
		expectSignal(t, changes, "tag restored")
		if got := ids(tr); !slices.Equal(got, []tailcfg.NodeID{2, 4}) {
			t.Errorf("nodes after tag restored = %v", got)
		}

		// Leaving the tailnet drops it too.
		f.RemoveNode(nodeB.ID)
		synctest.Wait()
		expectSignal(t, changes, "node removal")
		if got := ids(tr); !slices.Equal(got, []tailcfg.NodeID{4}) {
			t.Errorf("nodes after removal = %v", got)
		}

		// The local node's changes are tracked as well.
		moved := self
		moved.IP = netip.MustParseAddr("100.64.0.11")
		f.ChangeSelf(moved)
		synctest.Wait()
		expectSignal(t, changes, "self change")
		if s, _ := tr.Self(); !slices.Contains(s.Addrs, moved.IP) {
			t.Errorf("self after change = %+v", s)
		}

		// IsTailnetAddr knows the ranges and the listed addresses.
		for _, tt := range []struct {
			ip   string
			want bool
		}{
			{"100.64.0.4", true}, // tracked node
			{"100.64.0.11", true},
			{"100.99.1.1", true},        // in range
			{"fd7a:115c:a1e0::1", true}, // in range
			{"10.0.0.1", false},
			{"127.0.0.1", false},
		} {
			if got := tr.IsTailnetAddr(netip.MustParseAddr(tt.ip)); got != tt.want {
				t.Errorf("IsTailnetAddr(%s) = %v, want %v", tt.ip, got, tt.want)
			}
		}

		// tailscaled going away is reported and leaves the set alone;
		// coming back re-seeds from the new stream's initial status, after
		// the reconnect backoff.
		f.Stop()
		synctest.Wait()
		if st := tr.State(); st.Connected || st.LastError == "" {
			t.Errorf("state after tailscaled stopped = %+v", st)
		}
		if got := ids(tr); !slices.Equal(got, []tailcfg.NodeID{4}) {
			t.Errorf("nodes after disconnect = %v", got)
		}
		f.ChangeNode(nodeB) // only the fake's status changes; no stream to deliver it
		f.Start()
		time.Sleep(time.Second) // the first reconnect backoff
		synctest.Wait()
		if !tr.State().Connected {
			t.Fatalf("not reconnected: %+v", tr.State())
		}
		if got := ids(tr); !slices.Equal(got, []tailcfg.NodeID{2, 4}) {
			t.Errorf("nodes after reconnect = %v", got)
		}
	})
}

func TestStartRejectsBadConfig(t *testing.T) {
	for _, cfg := range []Config{{}, {Tag: "ci-mac"}} {
		if _, err := Start(context.Background(), cfg); err == nil {
			t.Errorf("Start(%+v) succeeded, want error", cfg)
		}
	}
}
