// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

//go:build !(linux || darwin || freebsd || windows)

package sparse

import (
	"io"
	"os"
)

// forEachExtent reports all of [off, end) as one extent, since this
// platform has no SEEK_DATA.
func forEachExtent(f *os.File, off, end int64, fn func(Extent) error) error {
	if off >= end {
		return nil
	}
	if _, err := f.Seek(off, io.SeekStart); err != nil {
		return err
	}
	return fn(Extent{Offset: off, Length: end - off})
}
