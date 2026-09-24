// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

// Package tagpeers tracks the tailnet nodes that carry a given tag, using the
// local tailscaled's IPN bus.
//
// A [Tracker] tells a daemon which nodes carry the tag, as the control plane
// sees it: their tailnet addresses, hostnames, and whether tailscaled thinks
// they're online, updated as nodes appear, change, and leave. It says nothing
// about whether a node is reachable or what it's running; that is for the
// layer above, such as the lansport package, which turns tagged nodes into
// working LAN connections.
package tagpeers

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"time"

	"tailscale.com/client/local"
	"tailscale.com/ipn"
	"tailscale.com/ipn/ipnstate"
	"tailscale.com/net/netx"
	"tailscale.com/net/tsaddr"
	"tailscale.com/tailcfg"
	"tailscale.com/types/logger"
)

// watchMask selects what the IPN bus watcher receives from tailscaled: the
// current status once, then peer additions, replacements, and removals.
// Narrow per-field changes (online state, endpoints) arrive promoted to full
// nodes, which is fine: only the fields in [Node] are compared.
const watchMask = ipn.NotifyInitialStatus | ipn.NotifyPeerChanges

// maxBackoff caps the wait between attempts to reconnect to tailscaled's IPN
// bus after the stream fails.
const maxBackoff = 30 * time.Second

// Node is a tailnet node carrying the tracked tag, or the local node.
type Node struct {
	ID       tailcfg.NodeID
	StableID tailcfg.StableNodeID
	Hostname string // the node's own hostname, not necessarily unique
	DNSName  string // MagicDNS FQDN with trailing dot, if any
	Addrs    []netip.Addr
	Tags     []string
	Online   bool
}

// IP returns the node's IPv4 tailnet address, else its first address.
func (n Node) IP() (netip.Addr, bool) {
	for _, ip := range n.Addrs {
		if ip.Is4() {
			return ip, true
		}
	}
	if len(n.Addrs) > 0 {
		return n.Addrs[0], true
	}
	return netip.Addr{}, false
}

// equal reports whether n and o agree on every field.
func (n Node) equal(o Node) bool {
	return n.ID == o.ID && n.StableID == o.StableID && n.Hostname == o.Hostname &&
		n.DNSName == o.DNSName && n.Online == o.Online &&
		slices.Equal(n.Addrs, o.Addrs) && slices.Equal(n.Tags, o.Tags)
}

func nodeFromStatus(ps *ipnstate.PeerStatus) Node {
	n := Node{
		ID:       ps.NodeID,
		StableID: ps.ID,
		Hostname: ps.HostName,
		DNSName:  ps.DNSName,
		Addrs:    slices.Clone(ps.TailscaleIPs),
		Online:   ps.Online,
	}
	if ps.Tags != nil {
		n.Tags = ps.Tags.AsSlice()
	}
	return n
}

// nodeFromNetmap converts a netmap node. Online is unknown for some nodes
// (nil); prev supplies the value to keep in that case, defaulting to true.
func nodeFromNetmap(tn *tailcfg.Node, prev *Node) Node {
	n := Node{
		ID:       tn.ID,
		StableID: tn.StableID,
		DNSName:  tn.Name,
		Tags:     slices.Clone(tn.Tags),
		Online:   true,
	}
	if tn.Hostinfo.Valid() && tn.Hostinfo.Hostname() != "" {
		n.Hostname = tn.Hostinfo.Hostname()
	} else if i := strings.IndexByte(tn.Name, '.'); i > 0 {
		n.Hostname = tn.Name[:i]
	} else {
		n.Hostname = tn.Name
	}
	for _, pfx := range tn.Addresses {
		n.Addrs = append(n.Addrs, pfx.Addr())
	}
	if tn.Online != nil {
		n.Online = *tn.Online
	} else if prev != nil {
		n.Online = prev.Online
	}
	return n
}

// Config configures a [Tracker].
type Config struct {
	// Tag is the tag (such as "tag:my-pool") whose nodes to track.
	Tag string

	// Socket is the path of tailscaled's LocalAPI socket. If empty, the
	// platform's default is used, including the macOS GUI variants'.
	Socket string

	// Dial, if non-nil, replaces how tailscaled's LocalAPI is reached and
	// Socket is ignored. Tests use it to reach a fake tailscaled; see the
	// tagpeerstest package.
	Dial netx.DialFunc

	// Logf receives log lines; nil discards them.
	Logf logger.Logf
}

// State describes the tracker's connection to tailscaled.
type State struct {
	// Connected reports whether an IPN bus stream is open and has
	// delivered its initial status. Since is when that happened.
	Connected bool
	Since     time.Time

	// LastError is why the most recent stream ended, or the most recent
	// failure to open one; empty while connected.
	LastError string

	// Events counts notifications applied across all streams, and
	// LastEvent is when the most recent one arrived.
	Events    int64
	LastEvent time.Time
}

// Tracker maintains the set of tailnet nodes carrying a tag by watching the
// local tailscaled's IPN bus. Use [Start] to create one.
type Tracker struct {
	cfg  Config
	lc   *local.Client
	logf logger.Logf // never nil

	// subs are the channels handed out by Subscribe, each signaled
	// (coalesced) whenever the tracked set or the local node changes.
	// subsMu is never held together with mu.
	subsMu sync.Mutex
	subs   []chan struct{}

	cancel context.CancelFunc
	done   chan struct{}

	// mu guards the fields below. It is never held together with subsMu:
	// the merge functions decide under mu whether anything changed and
	// signal subscribers only after releasing it.
	mu       sync.Mutex
	nodes    map[tailcfg.NodeID]Node
	self     Node
	haveSelf bool
	state    State
}

// Start begins tracking cfg.Tag's nodes. It returns before the first
// connection to tailscaled completes; see [Tracker.State] and
// [Tracker.Subscribe]. The tracker runs until ctx ends or [Tracker.Close] is
// called.
func Start(ctx context.Context, cfg Config) (*Tracker, error) {
	if cfg.Tag == "" {
		return nil, errors.New("tagpeers: Tag is required")
	}
	if !strings.HasPrefix(cfg.Tag, "tag:") {
		return nil, fmt.Errorf("tagpeers: tag %q must start with \"tag:\"", cfg.Tag)
	}
	if cfg.Logf == nil {
		cfg.Logf = logger.Discard
	}
	ctx, cancel := context.WithCancel(ctx)
	t := &Tracker{
		cfg:  cfg,
		logf: cfg.Logf,
		lc: &local.Client{
			Socket: cfg.Socket,
			Dial:   cfg.Dial,
			// An explicit socket path means exactly that; only the default
			// gets the platform's fallbacks (such as the macOS GUI
			// variants' port lookup).
			UseSocketOnly: cfg.Socket != "",
		},
		cancel: cancel,
		done:   make(chan struct{}),
		nodes:  make(map[tailcfg.NodeID]Node),
	}
	go t.run(ctx)
	return t, nil
}

// Close stops the tracker and waits for its goroutine to exit.
func (t *Tracker) Close() {
	t.cancel()
	<-t.done
}

// Subscribe returns a channel that receives a value whenever the set of
// tracked nodes, any tracked node's fields, or the local node changes.
// Signals are coalesced: a receiver that is slow sees one signal for many
// changes and should re-read [Tracker.Nodes]. Each call returns its own
// channel, so several parties can follow the tracker.
func (t *Tracker) Subscribe() <-chan struct{} {
	ch := make(chan struct{}, 1)
	t.subsMu.Lock()
	t.subs = append(t.subs, ch)
	t.subsMu.Unlock()
	return ch
}

// Nodes returns the nodes currently carrying the tag, sorted by ID. The
// local node is not included; see [Tracker.Self].
func (t *Tracker) Nodes() []Node {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]Node, 0, len(t.nodes))
	for _, n := range t.nodes {
		out = append(out, n)
	}
	slices.SortFunc(out, func(a, b Node) int { return int(a.ID - b.ID) })
	return out
}

// Self returns the local node as tailscaled reports it, if known yet.
func (t *Tracker) Self() (Node, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.self, t.haveSelf
}

// State returns the tracker's connection state.
func (t *Tracker) State() State {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.state
}

// IsTailnetAddr reports whether ip is a tailnet address: in the ranges
// Tailscale assigns from, or an address tailscaled reported for the local
// node or a tracked node. A TCP connection whose remote address satisfies
// this arrived through WireGuard, since a spoofed tailnet source on another
// network couldn't complete the handshake.
func (t *Tracker) IsTailnetAddr(ip netip.Addr) bool {
	ip = ip.Unmap()
	if tsaddr.IsTailscaleIP(ip) {
		return true
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.haveSelf && slices.Contains(t.self.Addrs, ip) {
		return true
	}
	for _, n := range t.nodes {
		if slices.Contains(n.Addrs, ip) {
			return true
		}
	}
	return false
}

func (t *Tracker) signal() {
	t.subsMu.Lock()
	defer t.subsMu.Unlock()
	for _, ch := range t.subs {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

// run keeps an IPN bus stream open, reconnecting with backoff when it
// fails.
func (t *Tracker) run(ctx context.Context) {
	defer close(t.done)
	backoff := time.Second
	for ctx.Err() == nil {
		start := time.Now()
		err := t.watch(ctx)
		if ctx.Err() != nil {
			return
		}
		t.mu.Lock()
		if t.state.LastError == "" {
			t.logf("tagpeers: watching tailscaled: %v", err)
		}
		t.state.LastError = err.Error()
		t.state.Connected = false
		t.state.Since = time.Time{}
		t.mu.Unlock()
		if time.Since(start) > time.Minute {
			backoff = time.Second
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, maxBackoff)
	}
}

// watch runs one IPN bus stream until it fails or ctx ends.
func (t *Tracker) watch(ctx context.Context) error {
	w, err := t.lc.WatchIPNBus(ctx, watchMask)
	if err != nil {
		return err
	}
	defer w.Close()
	n, err := w.Next()
	if err != nil {
		return err
	}
	if n.InitialStatus == nil {
		return errors.New("first notification carried no status; tailscaled may be too old for peer change notifications")
	}
	t.applyStatus(n.InitialStatus)
	t.applyNotify(&n)
	for {
		n, err := w.Next()
		if err != nil {
			return err
		}
		t.applyNotify(&n)
	}
}

// applyStatus replaces the tracked set with the tagged nodes in st, the
// full status that starts every stream, and signals subscribers if anything
// changed.
func (t *Tracker) applyStatus(st *ipnstate.Status) {
	if t.mergeStatus(st) {
		t.signal()
	}
}

// mergeStatus does applyStatus's work under mu and reports whether the
// tracked set or the local node changed.
func (t *Tracker) mergeStatus(st *ipnstate.Status) (changed bool) {
	now := time.Now()
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.state.LastError != "" {
		t.logf("tagpeers: watching tailscaled works again")
	}
	t.state = State{Connected: true, Since: now, Events: t.state.Events + 1, LastEvent: now}
	if st.Self != nil {
		self := nodeFromStatus(st.Self)
		if !t.haveSelf || !self.equal(t.self) {
			t.self, t.haveSelf = self, true
			changed = true
		}
	}
	fresh := make(map[tailcfg.NodeID]Node)
	for _, ps := range st.Peer {
		n := nodeFromStatus(ps)
		if slices.Contains(n.Tags, t.cfg.Tag) {
			fresh[n.ID] = n
		}
	}
	for id, n := range fresh {
		if old, ok := t.nodes[id]; !ok || !old.equal(n) {
			changed = true
			if !ok {
				t.logf("tagpeers: %s (%v) carries %s", n.Hostname, n.Addrs, t.cfg.Tag)
			}
		}
	}
	for id, old := range t.nodes {
		if _, ok := fresh[id]; !ok {
			t.logf("tagpeers: %s (%v) no longer carries %s", old.Hostname, old.Addrs, t.cfg.Tag)
			changed = true
		}
	}
	t.nodes = fresh
	return changed
}

// applyNotify applies the deltas in one notification and signals
// subscribers if anything changed.
func (t *Tracker) applyNotify(n *ipn.Notify) {
	if len(n.PeersChanged) == 0 && len(n.PeersRemoved) == 0 && n.SelfChange == nil {
		return
	}
	if t.mergeNotify(n) {
		t.signal()
	}
}

// mergeNotify does applyNotify's work under mu and reports whether the
// tracked set or the local node changed.
func (t *Tracker) mergeNotify(n *ipn.Notify) (changed bool) {
	now := time.Now()
	t.mu.Lock()
	defer t.mu.Unlock()
	t.state.Events++
	t.state.LastEvent = now
	if n.SelfChange != nil {
		var prev *Node
		if t.haveSelf {
			prev = &t.self
		}
		self := nodeFromNetmap(n.SelfChange, prev)
		if !t.haveSelf || !self.equal(t.self) {
			t.self, t.haveSelf = self, true
			changed = true
		}
	}
	for _, tn := range n.PeersChanged {
		old, had := t.nodes[tn.ID]
		var prev *Node
		if had {
			prev = &old
		}
		node := nodeFromNetmap(tn, prev)
		if !slices.Contains(node.Tags, t.cfg.Tag) {
			if had {
				t.logf("tagpeers: %s (%v) no longer carries %s", old.Hostname, old.Addrs, t.cfg.Tag)
				delete(t.nodes, tn.ID)
				changed = true
			}
			continue
		}
		if !had {
			t.logf("tagpeers: %s (%v) carries %s", node.Hostname, node.Addrs, t.cfg.Tag)
		}
		if !had || !old.equal(node) {
			t.nodes[tn.ID] = node
			changed = true
		}
	}
	for _, id := range n.PeersRemoved {
		if old, ok := t.nodes[id]; ok {
			t.logf("tagpeers: %s (%v) left the tailnet", old.Hostname, old.Addrs)
			delete(t.nodes, id)
			changed = true
		}
	}
	return changed
}
