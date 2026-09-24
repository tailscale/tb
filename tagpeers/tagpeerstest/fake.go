// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

// Package tagpeerstest provides a fake tailscaled for testing code built on
// [tagpeers]: it serves the LocalAPI IPN bus watch endpoint over an
// in-memory connection, starting each stream with a status the test controls
// and pushing peer deltas as the test changes nodes. It works under
// testing/synctest.
package tagpeerstest

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net"
	"net/http"
	"net/netip"
	"sync"
	"testing"

	"tailscale.com/ipn"
	"tailscale.com/ipn/ipnstate"
	"tailscale.com/net/memnet"
	"tailscale.com/tailcfg"
	"tailscale.com/types/key"
	"tailscale.com/types/views"
	"tailscale.com/util/set"
)

// Node describes a tailnet node for the fake.
type Node struct {
	ID     tailcfg.NodeID
	Name   string // hostname; the DNS name is derived from it
	IP     netip.Addr
	Tags   []string
	Online bool
}

func (d Node) peerStatus() *ipnstate.PeerStatus {
	tags := views.SliceOf(d.Tags)
	return &ipnstate.PeerStatus{
		ID:           tailcfg.StableNodeID(fmt.Sprintf("stable-%d", d.ID)),
		NodeID:       d.ID,
		HostName:     d.Name,
		DNSName:      d.Name + ".example.ts.net.",
		TailscaleIPs: []netip.Addr{d.IP},
		Tags:         &tags,
		Online:       d.Online,
	}
}

func (d Node) netmapNode() *tailcfg.Node {
	online := d.Online
	return &tailcfg.Node{
		ID:        d.ID,
		StableID:  tailcfg.StableNodeID(fmt.Sprintf("stable-%d", d.ID)),
		Name:      d.Name + ".example.ts.net.",
		Addresses: []netip.Prefix{netip.PrefixFrom(d.IP, d.IP.BitLen())},
		Hostinfo:  (&tailcfg.Hostinfo{Hostname: d.Name}).View(),
		Tags:      d.Tags,
		Online:    &online,
	}
}

// listenerName is the memnet address the fake listens at. It never leaves
// the process, so any name will do.
const listenerName = "tailscaled"

// Tailscaled is a fake tailscaled. Hand its Dial method to the code under
// test (as tagpeers.Config.Dial) to make it talk to the fake.
type Tailscaled struct {
	t testing.TB

	mu       sync.Mutex
	ln       *memnet.Listener // nil while stopped
	hs       *http.Server
	status   ipnstate.Status
	watchers set.Set[chan ipn.Notify]
}

// New starts a fake tailscaled whose local node is self and whose tailnet
// peers are peers. It is stopped when the test ends.
func New(t testing.TB, self Node, peers ...Node) *Tailscaled {
	t.Helper()
	f := &Tailscaled{
		t:        t,
		watchers: make(set.Set[chan ipn.Notify]),
	}
	f.status = ipnstate.Status{Self: self.peerStatus(), Peer: map[key.NodePublic]*ipnstate.PeerStatus{}}
	for _, p := range peers {
		f.status.Peer[key.NewNode().Public()] = p.peerStatus()
	}
	f.Start()
	t.Cleanup(f.Stop)
	return f
}

// Dial connects to the fake. Pass it as tagpeers.Config.Dial. While the
// fake is stopped it fails, as connecting to a dead tailscaled would.
func (f *Tailscaled) Dial(ctx context.Context, _, _ string) (net.Conn, error) {
	f.mu.Lock()
	ln := f.ln
	f.mu.Unlock()
	if ln == nil {
		return nil, &net.OpError{Op: "dial", Net: "unix", Err: net.ErrClosed}
	}
	return ln.Dial(ctx, "tcp", listenerName)
}

// Start serves the IPN bus watch endpoint. New calls it; tests call it
// again after Stop to simulate tailscaled coming back.
func (f *Tailscaled) Start() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.ln != nil {
		f.t.Fatal("tagpeerstest: Start called while running")
	}
	f.ln = memnet.Listen(listenerName)
	f.hs = &http.Server{Handler: http.HandlerFunc(f.serveWatch)}
	go f.hs.Serve(f.ln)
}

// Stop shuts the fake down, closing its listener and any open streams, as a
// tailscaled restart or crash would. It is a no-op while stopped.
func (f *Tailscaled) Stop() {
	f.mu.Lock()
	hs := f.hs
	f.ln, f.hs = nil, nil
	f.mu.Unlock()
	if hs != nil {
		hs.Close()
	}
}

func (f *Tailscaled) serveWatch(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/localapi/v0/watch-ipn-bus" {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	ch := make(chan ipn.Notify, 16)
	f.mu.Lock()
	f.watchers.Add(ch)
	initial := f.status
	initial.Peer = maps.Clone(f.status.Peer)
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		f.watchers.Delete(ch)
		f.mu.Unlock()
	}()

	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	rc := http.NewResponseController(w)
	if err := enc.Encode(ipn.Notify{InitialStatus: &initial}); err != nil {
		return
	}
	rc.Flush()
	for {
		select {
		case n := <-ch:
			if err := enc.Encode(n); err != nil {
				return
			}
			rc.Flush()
		case <-r.Context().Done():
			return
		}
	}
}

// Notify sends n on every open stream.
func (f *Tailscaled) Notify(n ipn.Notify) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for ch := range f.watchers {
		ch <- n
	}
}

// ChangeNode announces d as added or replaced, as control does when a node
// appears or changes (without the peer patches bit, that includes coming
// online or offline).
func (f *Tailscaled) ChangeNode(d Node) {
	f.mu.Lock()
	for k, p := range f.status.Peer {
		if p.NodeID == d.ID {
			delete(f.status.Peer, k)
		}
	}
	f.status.Peer[key.NewNode().Public()] = d.peerStatus()
	f.mu.Unlock()
	f.Notify(ipn.Notify{PeersChanged: []*tailcfg.Node{d.netmapNode()}})
}

// RemoveNode announces that node id left the tailnet.
func (f *Tailscaled) RemoveNode(id tailcfg.NodeID) {
	f.mu.Lock()
	for k, p := range f.status.Peer {
		if p.NodeID == id {
			delete(f.status.Peer, k)
		}
	}
	f.mu.Unlock()
	f.Notify(ipn.Notify{PeersRemoved: []tailcfg.NodeID{id}})
}

// ChangeSelf announces that the local node changed.
func (f *Tailscaled) ChangeSelf(d Node) {
	f.mu.Lock()
	f.status.Self = d.peerStatus()
	f.mu.Unlock()
	f.Notify(ipn.Notify{SelfChange: d.netmapNode()})
}
