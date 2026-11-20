// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package guestnet

import (
	"context"
	"net/netip"
	"sync"
	"time"
)

// Flow is one TCP connection or UDP session from the guest.
type Flow struct {
	// ID is unique within a Net and identical for the open and close records
	// of one flow.
	ID    uint64         `json:"id"`
	Proto string         `json:"proto"` // "tcp" or "udp"
	Src   netip.AddrPort `json:"src"`
	Dst   netip.AddrPort `json:"dst"`
	Start time.Time      `json:"start"`
	// End is zero for the open record.
	End time.Time `json:"end,omitzero"`
	// Kind says how the flow was handled: "egress", "service", "dns", "ntp",
	// or "blocked".
	Kind string `json:"kind"`
	// Service names the registered service for Kind "service".
	Service string `json:"service,omitempty"`
	// BytesIn counts bytes sent to the guest, BytesOut from the guest.
	BytesIn  int64  `json:"bytes_in"`
	BytesOut int64  `json:"bytes_out"`
	Err      string `json:"err,omitempty"`
}

// Closed reports whether f is a close record.
func (f Flow) Closed() bool { return !f.End.IsZero() }

// DNSEvent is one DNS query from the guest and how it was answered.
type DNSEvent struct {
	Time     time.Time      `json:"time"`
	Src      netip.AddrPort `json:"src"`
	Dst      netip.AddrPort `json:"dst"`
	Proto    string         `json:"proto"`
	Name     string         `json:"name"`
	Type     string         `json:"type"`
	RCode    string         `json:"rcode,omitempty"`
	Answers  int            `json:"answers"`
	Upstream string         `json:"upstream,omitempty"`
	Duration time.Duration  `json:"duration"`
	Err      string         `json:"err,omitempty"`
}

// Event is a notable network event.
type Event struct {
	Time time.Time `json:"time"`
	// Kind is "dhcp-discover", "dhcp-lease", "blocked", "dial-error", ...
	Kind string `json:"kind"`
	Msg  string `json:"msg"`
}

// Log is a bounded ring of records with live subscribers.
type Log[T any] struct {
	mu   sync.Mutex
	ring []T
	next int
	full bool
	subs map[chan T]struct{}
}

// NewLog returns a Log keeping the last n records.
func NewLog[T any](n int) *Log[T] {
	return &Log[T]{ring: make([]T, n), subs: make(map[chan T]struct{})}
}

// Add appends a record and delivers it to subscribers that are keeping up.
// Slow subscribers miss records rather than block the network.
func (lg *Log[T]) Add(v T) {
	lg.mu.Lock()
	defer lg.mu.Unlock()
	lg.ring[lg.next] = v
	lg.next++
	if lg.next == len(lg.ring) {
		lg.next = 0
		lg.full = true
	}
	for ch := range lg.subs {
		select {
		case ch <- v:
		default:
		}
	}
}

// Recent returns the retained records, oldest first.
func (lg *Log[T]) Recent() []T {
	lg.mu.Lock()
	defer lg.mu.Unlock()
	if !lg.full {
		return append([]T(nil), lg.ring[:lg.next]...)
	}
	out := make([]T, 0, len(lg.ring))
	out = append(out, lg.ring[lg.next:]...)
	out = append(out, lg.ring[:lg.next]...)
	return out
}

// Subscribe returns a channel receiving new records until ctx is done.
func (lg *Log[T]) Subscribe(ctx context.Context) <-chan T {
	ch := make(chan T, 256)
	lg.mu.Lock()
	lg.subs[ch] = struct{}{}
	lg.mu.Unlock()
	go func() {
		<-ctx.Done()
		lg.mu.Lock()
		delete(lg.subs, ch)
		lg.mu.Unlock()
	}()
	return ch
}
