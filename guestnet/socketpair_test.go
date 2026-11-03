// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

//go:build unix

package guestnet

import (
	"testing"
	"time"
)

// TestSocketPairCloseUnblocks checks that a Net on a real socketpair shuts
// down while its frame reader is blocked, which requires our end of the
// pair to use the runtime poller.
func TestSocketPairCloseUnblocks(t *testing.T) {
	vmSide, link, err := NewSocketPairLink()
	if err != nil {
		t.Fatal(err)
	}
	defer vmSide.Close()
	n, err := New(Config{Link: link})
	if err != nil {
		t.Fatal(err)
	}
	// Let the frame reader block in ReadFrame.
	time.Sleep(50 * time.Millisecond)
	done := make(chan error, 1)
	go func() { done <- n.Close() }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Close hung with a blocked reader")
	}
}
