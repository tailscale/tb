// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package sparse

import (
	"os"
	"testing"

	"golang.org/x/sys/windows"
)

// makeSparse marks f sparse, without which NTFS allocates a file's holes.
func makeSparse(t *testing.T, f *os.File) {
	t.Helper()
	var n uint32
	if err := windows.DeviceIoControl(windows.Handle(f.Fd()), windows.FSCTL_SET_SPARSE, nil, 0, nil, 0, &n, nil); err != nil {
		t.Fatalf("FSCTL_SET_SPARSE: %v", err)
	}
}
