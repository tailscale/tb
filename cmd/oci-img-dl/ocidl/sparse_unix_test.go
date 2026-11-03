// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

//go:build unix

package ocidl

import (
	"os"
	"syscall"
	"testing"
)

// allocatedBlocks returns the number of 512-byte blocks allocated to path.
func allocatedBlocks(t *testing.T, path string) int64 {
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Sys().(*syscall.Stat_t).Blocks
}
