// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

//go:build linux || darwin || freebsd

package sparse

import (
	"errors"
	"io"
	"os"

	"golang.org/x/sys/unix"
)

// forEachExtent calls fn for each data extent of f within [off, end).
func forEachExtent(f *os.File, off, end int64, fn func(Extent) error) error {
	rc, err := f.SyscallConn()
	if err != nil {
		return err
	}
	// seek runs lseek on f's descriptor. The SEEK_DATA and SEEK_HOLE values
	// differ between platforms, so they come from x/sys/unix.
	seek := func(off int64, whence int) (pos int64, err error) {
		if cerr := rc.Control(func(fd uintptr) { pos, err = unix.Seek(int(fd), off, whence) }); cerr != nil {
			return 0, cerr
		}
		return pos, err
	}
	for pos := off; pos < end; {
		start, err := seek(pos, unix.SEEK_DATA)
		switch {
		case errors.Is(err, unix.ENXIO):
			return nil // The rest of the file is a hole.
		case errors.Is(err, unix.EINVAL):
			start = pos // The file system can't report holes.
		case err != nil:
			return err
		}
		if start >= end {
			return nil
		}
		stop, err := seek(start, unix.SEEK_HOLE)
		if err != nil {
			stop = end
		}
		stop = min(stop, end)
		if _, err := f.Seek(start, io.SeekStart); err != nil {
			return err
		}
		if err := fn(Extent{Offset: start, Length: stop - start}); err != nil {
			return err
		}
		pos = stop
	}
	return nil
}
