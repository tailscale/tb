// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

//go:build !windows

package sparse

import (
	"os"
	"testing"
)

// makeSparse does nothing, since these file systems leave unwritten
// regions as holes.
func makeSparse(t *testing.T, f *os.File) {}
