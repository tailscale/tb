// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package sparse

import (
	"errors"
	"io"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

// allocatedRange is FILE_ALLOCATED_RANGE_BUFFER, the input and output of
// FSCTL_QUERY_ALLOCATED_RANGES.
type allocatedRange struct {
	FileOffset int64
	Length     int64
}

// rangesPerQuery is how many ranges one FSCTL_QUERY_ALLOCATED_RANGES call
// returns at most; more need another call.
const rangesPerQuery = 64

// forEachExtent reports f's allocated ranges within [off, end) as its
// extents. A file that isn't marked sparse is allocated throughout, so it is
// one extent.
func forEachExtent(f *os.File, off, end int64, fn func(Extent) error) error {
	rc, err := f.SyscallConn()
	if err != nil {
		return err
	}
	for pos := off; pos < end; {
		in := allocatedRange{FileOffset: pos, Length: end - pos}
		var out [rangesPerQuery]allocatedRange
		var n uint32
		var qerr error
		if err := rc.Control(func(h uintptr) {
			qerr = windows.DeviceIoControl(windows.Handle(h), windows.FSCTL_QUERY_ALLOCATED_RANGES,
				(*byte)(unsafe.Pointer(&in)), uint32(unsafe.Sizeof(in)),
				(*byte)(unsafe.Pointer(&out[0])), uint32(unsafe.Sizeof(out)), &n, nil)
		}); err != nil {
			return err
		}
		more := errors.Is(qerr, windows.ERROR_MORE_DATA)
		switch {
		case errors.Is(qerr, windows.ERROR_INVALID_FUNCTION):
			// The file system can't report allocated ranges.
			return emit(f, Extent{Offset: pos, Length: end - pos}, fn)
		case qerr != nil && !more:
			return qerr
		}
		ranges := out[:n/uint32(unsafe.Sizeof(out[0]))]
		if len(ranges) == 0 {
			return nil // The rest of the file is unallocated.
		}
		for _, r := range ranges {
			start, stop := max(r.FileOffset, pos), min(r.FileOffset+r.Length, end)
			if start >= stop {
				continue
			}
			if err := emit(f, Extent{Offset: start, Length: stop - start}, fn); err != nil {
				return err
			}
		}
		if !more {
			return nil
		}
		last := ranges[len(ranges)-1]
		pos = last.FileOffset + last.Length
	}
	return nil
}

// emit sets f's position to e's start and calls fn with e.
func emit(f *os.File, e Extent, fn func(Extent) error) error {
	if _, err := f.Seek(e.Offset, io.SeekStart); err != nil {
		return err
	}
	return fn(e)
}
