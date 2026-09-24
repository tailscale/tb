// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package rendezvous

import (
	"context"
	"sync/atomic"

	"github.com/tailscale/tb/lansport"
)

// Router picks which of a lansport.Server's peers, or the local process,
// should handle a key. It follows the server's reachable set, so a peer that
// goes away stops being picked within one probe round, and includes the
// local process with its own advertised weight.
type Router struct {
	s      *lansport.Server
	cur    atomic.Pointer[routing]
	cancel context.CancelFunc
	done   chan struct{}
}

// routing is one consistent view of the reachable peers and the table built
// from them. Router replaces it wholesale on each change, so a Pick never
// sees a table and a peer map from different rounds.
type routing struct {
	table *Table
	peers map[lansport.Key]lansport.Peer
}

// NewRouter returns a Router following s. Close it when done.
func NewRouter(s *lansport.Server) *Router {
	ctx, cancel := context.WithCancel(context.Background())
	r := &Router{s: s, cancel: cancel, done: make(chan struct{})}
	changes := s.Subscribe()
	r.update()
	go r.run(ctx, changes)
	return r
}

// Close stops following the server.
func (r *Router) Close() {
	r.cancel()
	<-r.done
}

func (r *Router) run(ctx context.Context, changes <-chan struct{}) {
	defer close(r.done)
	for {
		select {
		case <-ctx.Done():
			return
		case <-changes:
			r.update()
		}
	}
}

// update rebuilds the table from the server's reachable peers and itself.
func (r *Router) update() {
	peers := r.s.Peers()
	members := make(map[string]float64, len(peers)+1)
	for key, p := range peers {
		members[string(key)] = p.Weight
	}
	self := r.s.Self()
	if self.Key != "" {
		members[string(self.Key)] = self.Weight
	}
	table := new(Table)
	table.Set(members)
	r.cur.Store(&routing{table: table, peers: peers})
}

// Pick returns the reachable peer that should handle key. It returns false
// when the local process should: because it is the owner, or because there
// are no reachable peers.
func (r *Router) Pick(key string) (lansport.Peer, bool) {
	cur := r.cur.Load()
	owner, ok := cur.table.Pick(key)
	if !ok {
		return lansport.Peer{}, false
	}
	p, ok := cur.peers[lansport.Key(owner)]
	return p, ok
}

// Members returns the keys currently in the table, sorted, including the
// local process's if known. It is for debugging pages: two routers whose
// members differ route some keys differently.
func (r *Router) Members() []string {
	return r.cur.Load().table.Members()
}
