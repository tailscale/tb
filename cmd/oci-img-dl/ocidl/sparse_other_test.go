// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

//go:build !unix

package ocidl

import "testing"

// allocatedBlocks returns -1: allocation isn't checked on this platform.
func allocatedBlocks(t *testing.T, path string) int64 { return -1 }
