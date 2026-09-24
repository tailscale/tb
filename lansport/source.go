// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package lansport

import (
	"net"
	"net/netip"
	"slices"
	"strconv"

	"github.com/tailscale/tb/tagpeers"
)

// Key identifies a peer for routing purposes. Every member of a pool must
// have a distinct Key, and each member's Key must be the same on every
// other member, since the rendezvous package hashes on it to decide which
// member owns what: two members that disagree about a Key route some keys
// differently. It should also survive a node renaming or changing address,
// or those events move ownership around. A tailnet node's stable node ID
// is the usual choice; a static peer's advertised name is the fallback.
type Key string

// Candidate is a node that might be a peer, as reported by a [Source]: where
// to fetch its advert from, over a path the source vouches for.
type Candidate struct {
	// Key is the node's routing key. If empty, the name the node
	// advertises is used, which must then be unique.
	Key Key

	// Name is for display until the node's advert supplies its own.
	Name string

	// AdvertAddr is the host:port to fetch the node's advert from with
	// plain HTTP. The source guarantees that whatever answers there is the
	// node (for a tailnet source, WireGuard does).
	AdvertAddr string

	// Online reports whether the source believes the node is up. Offline
	// candidates are kept but not probed.
	Online bool
}

// Source tells a [Server] which nodes might be peers and which connections
// may fetch its advert. [TailnetSource] and [StaticSource] implement it.
type Source interface {
	// Candidates returns the current candidates.
	Candidates() []Candidate

	// Changes returns a channel that receives a value when Candidates may
	// have changed. Signals may be coalesced. A Source has one consumer,
	// its Server, so one channel is enough.
	Changes() <-chan struct{}

	// TrustedAddr reports whether a connection from ip arrived over a path
	// the source vouches for, and so may fetch this node's advert.
	TrustedAddr(ip netip.Addr) bool

	// SelfKey returns this node's own routing [Key], if known: what other
	// members use for this node, so the rendezvous table here agrees with
	// theirs about which keys this node owns.
	SelfKey() (_ Key, ok bool)
}

// TailnetSource adapts a [tagpeers.Tracker] into a [Source]: every tracked
// node is a candidate whose advert is fetched at its tailnet address on
// advertPort, over WireGuard.
func TailnetSource(tr *tagpeers.Tracker, advertPort int) Source {
	return &tailnetSource{tr: tr, port: advertPort, changes: tr.Subscribe()}
}

type tailnetSource struct {
	tr      *tagpeers.Tracker
	port    int
	changes <-chan struct{}
}

func (s *tailnetSource) Candidates() []Candidate {
	nodes := s.tr.Nodes()
	out := make([]Candidate, 0, len(nodes))
	for _, n := range nodes {
		ip, ok := n.IP()
		if !ok {
			continue
		}
		out = append(out, Candidate{
			Key:        Key(n.StableID),
			Name:       n.Hostname,
			AdvertAddr: netip.AddrPortFrom(ip, uint16(s.port)).String(),
			Online:     n.Online,
		})
	}
	return out
}

func (s *tailnetSource) Changes() <-chan struct{}       { return s.changes }
func (s *tailnetSource) TrustedAddr(ip netip.Addr) bool { return s.tr.IsTailnetAddr(ip) }

func (s *tailnetSource) SelfKey() (_ Key, ok bool) {
	self, ok := s.tr.Self()
	if !ok || self.StableID == "" {
		return "", false
	}
	return Key(self.StableID), true
}

// StaticSource returns a [Source] with a fixed list of advert addresses,
// trusted because the operator configured them. It is for tests and for
// hosts without a tailscaled. Peers are keyed by the names they advertise,
// and the local node by selfName, so every member must have a distinct
// name.
func StaticSource(selfName string, advertAddrs ...string) Source {
	s := &staticSource{selfKey: Key(selfName), changes: make(chan struct{})}
	for _, a := range advertAddrs {
		s.cands = append(s.cands, Candidate{Name: a, AdvertAddr: a, Online: true})
		if host, _, err := net.SplitHostPort(a); err == nil {
			if ip, err := netip.ParseAddr(host); err == nil {
				s.hosts = append(s.hosts, ip.Unmap())
			}
		}
	}
	return s
}

type staticSource struct {
	selfKey Key
	cands   []Candidate
	hosts   []netip.Addr
	changes chan struct{} // never signaled
}

func (s *staticSource) Candidates() []Candidate        { return slices.Clone(s.cands) }
func (s *staticSource) Changes() <-chan struct{}       { return s.changes }
func (s *staticSource) TrustedAddr(ip netip.Addr) bool { return slices.Contains(s.hosts, ip.Unmap()) }
func (s *staticSource) SelfKey() (_ Key, ok bool)      { return s.selfKey, s.selfKey != "" }
func (s *staticSource) String() string {
	return "static list of " + strconv.Itoa(len(s.cands)) + " peers"
}
