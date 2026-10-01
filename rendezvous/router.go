// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package rendezvous

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/tailscale/tb/lansport"
)

// RouterConfig configures a [Router]'s hysteresis: how long membership
// changes must persist before ownership moves. Without it, a peer that flaps
// or a pool whose members are discovered one by one at startup would move
// keys around on every change.
type RouterConfig struct {
	// JoinDelay is how long after the most recent newly reachable peer
	// appeared the Router waits before giving the new peers ownership of
	// keys. Peers that appear within the window are adopted together, so a
	// pool coming up one member at a time changes ownership once rather
	// than once per member. Until adopted, a new peer owns nothing. Zero
	// adopts new peers at once.
	JoinDelay time.Duration

	// LeaveDelay is how long a member must stay unreachable before its keys
	// are reassigned to other members. Until then it still owns its keys,
	// and [Router.Pick] reports them as the local process's to handle, so
	// a brief outage or restart neither moves keys away nor back. Zero
	// reassigns at once.
	LeaveDelay time.Duration
}

// Router picks which of a lansport.Server's peers, or the local process,
// should handle a key. It follows the server's reachable set, with the
// hysteresis in [RouterConfig], and includes the local process with its own
// advertised weight.
type Router struct {
	s      *lansport.Server
	cfg    RouterConfig
	cur    atomic.Pointer[routing]
	cancel context.CancelFunc
	done   chan struct{}

	// The following fields are only touched by the run goroutine (and by
	// NewRouter before it starts), so they need no lock.
	adopted   map[lansport.Key]float64   // members in the table, by weight; not including the local process
	lostAt    map[lansport.Key]time.Time // when each adopted member became unreachable, while it is
	firstSeen map[lansport.Key]time.Time // when each reachable but not yet adopted peer appeared
	timer     *time.Timer                // for the next adoption or reassignment; nil if none pending
}

// routing is one consistent view of the table and the reachable peers.
// Router replaces it wholesale on each change, so a Pick never sees a table
// and a peer map from different rounds.
type routing struct {
	table *Table
	peers map[lansport.Key]lansport.Peer
}

// NewRouter returns a Router following s. Close it when done.
func NewRouter(s *lansport.Server, cfg RouterConfig) *Router {
	ctx, cancel := context.WithCancel(context.Background())
	r := &Router{
		s:         s,
		cfg:       cfg,
		cancel:    cancel,
		done:      make(chan struct{}),
		adopted:   make(map[lansport.Key]float64),
		lostAt:    make(map[lansport.Key]time.Time),
		firstSeen: make(map[lansport.Key]time.Time),
	}
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
		var fire <-chan time.Time
		if r.timer != nil {
			fire = r.timer.C
		}
		select {
		case <-ctx.Done():
			if r.timer != nil {
				r.timer.Stop()
			}
			return
		case <-changes:
		case <-fire:
			r.timer = nil
		}
		r.update()
	}
}

// update brings the adopted member set in line with the server's reachable
// peers, applying the configured delays, rebuilds the table, and arms the
// timer for the next delayed change, if any.
func (r *Router) update() {
	now := time.Now()
	peers := r.s.Peers()
	var next time.Time // when the next delayed change is due; zero if none

	// Reachable peers not yet adopted are joining. Note when each
	// appeared, and forget those that left before being adopted.
	for key := range peers {
		if _, ok := r.adopted[key]; ok {
			continue
		}
		if _, ok := r.firstSeen[key]; !ok {
			r.firstSeen[key] = now
		}
	}
	for key := range r.firstSeen {
		if _, ok := peers[key]; !ok {
			delete(r.firstSeen, key)
		}
	}
	// Adopt all joiners together once JoinDelay has passed since the
	// latest one appeared.
	if len(r.firstSeen) > 0 {
		var latest time.Time
		for _, t := range r.firstSeen {
			if t.After(latest) {
				latest = t
			}
		}
		if due := latest.Add(r.cfg.JoinDelay); !now.Before(due) {
			for key := range r.firstSeen {
				r.adopted[key] = peers[key].Weight
				delete(r.firstSeen, key)
			}
		} else {
			next = due
		}
	}

	// Adopted members that are unreachable are leaving; drop each one
	// LeaveDelay after it was lost, unless it came back first.
	for key := range r.adopted {
		if p, ok := peers[key]; ok {
			r.adopted[key] = p.Weight
			delete(r.lostAt, key)
			continue
		}
		lost, ok := r.lostAt[key]
		if !ok {
			lost = now
			r.lostAt[key] = now
		}
		if due := lost.Add(r.cfg.LeaveDelay); !now.Before(due) {
			delete(r.adopted, key)
			delete(r.lostAt, key)
		} else if next.IsZero() || due.Before(next) {
			next = due
		}
	}

	members := make(map[string]float64, len(r.adopted)+1)
	for key, w := range r.adopted {
		members[string(key)] = w
	}
	if self := r.s.Self(); self.Key != "" {
		members[string(self.Key)] = self.Weight
	}
	table := new(Table)
	table.Set(members)
	r.cur.Store(&routing{table: table, peers: peers})

	if r.timer != nil {
		r.timer.Stop()
		r.timer = nil
	}
	if !next.IsZero() {
		r.timer = time.NewTimer(next.Sub(now))
	}
}

// Pick returns the reachable peer that should handle key. It returns false
// when the local process should: because it is the owner, because the owner
// is unreachable but still within its LeaveDelay, or because there are no
// members besides the local process.
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
