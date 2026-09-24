// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

// Package rendezvous assigns keys to members of a changing set with weighted
// rendezvous (highest random weight) hashing: every party with the same
// member list picks the same member for a key, adding or removing a member
// only moves the keys that member gains or loses, and a member's share of
// the keyspace is proportional to its weight.
//
// Throughout, a member is one of the parties that can own things (named by
// a string, such as a node ID) and a key is a thing to be owned (such as a
// cache entry's ID).
//
// The scoring is the weighted variant of rendezvous hashing from
// Schindelhauer and Schomaker, "Weighted Distributed Hash Tables" (SPAA
// 2005); see https://en.wikipedia.org/wiki/Rendezvous_hashing for the
// unweighted original by Thaler and Ravishankar and the weighted form.
//
// [Table] is the pure hashing. [Router] keeps a Table in step with a
// lansport.Server's reachable peers, so a caller can ask which peer, if
// any, should handle a key.
package rendezvous

import (
	"math"
	"slices"
	"sync"

	"github.com/cespare/xxhash/v2"
)

// Table picks a member for a key by weighted rendezvous hashing.
type Table struct {
	mu      sync.RWMutex
	members []member
}

// member is one party that can own keys.
type member struct {
	name   string
	weight float64
}

// Set replaces the members, a map from member name to weight. Weights must
// be positive; a member with weight 2 receives about twice the keys of one
// with weight 1. Members with non-positive weight are ignored.
func (t *Table) Set(members map[string]float64) {
	ms := make([]member, 0, len(members))
	for name, w := range members {
		if w > 0 {
			ms = append(ms, member{name: name, weight: w})
		}
	}
	t.mu.Lock()
	t.members = ms
	t.mu.Unlock()
}

// Pick returns the name of the member that owns key, or false if there are
// no members.
func (t *Table) Pick(key string) (string, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	best := -1
	var bestScore float64
	for i, m := range t.members {
		if s := Score(m.name, m.weight, key); best < 0 || s > bestScore {
			best, bestScore = i, s
		}
	}
	if best < 0 {
		return "", false
	}
	return t.members[best].name, true
}

// Members returns the names of the current members, sorted.
func (t *Table) Members() []string {
	t.mu.RLock()
	defer t.mu.RUnlock()
	out := make([]string, len(t.members))
	for i, m := range t.members {
		out[i] = m.name
	}
	slices.Sort(out)
	return out
}

// Score returns the weighted rendezvous score of key for the member with
// the given name and weight. The member with the highest score owns the
// key. Every party computes the same score for the same inputs, so they
// agree on owners without coordination.
func Score(member string, weight float64, key string) float64 {
	var d xxhash.Digest
	d.Reset()
	d.WriteString(member)
	d.WriteString("\x00")
	d.WriteString(key)
	// The weighted form: -weight / ln(u), for u uniform in (0, 1) derived
	// from hashing member and key together. Map the 64-bit hash to (0, 1);
	// the +1 and +2 keep u strictly inside the interval so the logarithm
	// is finite and negative.
	u := (float64(d.Sum64()>>11) + 1) / float64(1<<53+2)
	return -weight / math.Log(u)
}
