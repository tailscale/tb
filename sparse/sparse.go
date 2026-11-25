// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

// Package sparse lists the data extents of sparse files, so callers can
// skip their holes.
package sparse

import (
	"errors"
	"iter"
	"os"

	"tailscale.com/types/result"
)

// Extent is a region of a file that holds data rather than a hole.
type Extent struct {
	Offset int64
	Length int64
}

// End returns the offset just past the extent.
func (e Extent) End() int64 { return e.Offset + e.Length }

// Extents returns an iterator over the data extents of all of f, as of its
// size when iteration starts. See [ExtentsInRange].
func Extents(f *os.File) iter.Seq[result.Of[Extent]] {
	return func(yield func(result.Of[Extent]) bool) {
		fi, err := f.Stat()
		if err != nil {
			yield(result.Error[Extent](err))
			return
		}
		ExtentsInRange(f, 0, fi.Size())(yield)
	}
}

// ExtentsInRange returns an iterator over the data extents of f within
// [off, off+n), in ascending order, clipped to that range. Before yielding
// each extent it sets f's position to the extent's start, but the caller may
// read at any offset, as with an io.NewSectionReader. Iteration stops after
// the first error.
//
// It finds extents with SEEK_DATA and SEEK_HOLE on Linux, macOS, and
// FreeBSD, and with FSCTL_QUERY_ALLOCATED_RANGES on Windows, where only
// files marked sparse have holes. Where the file system can't report holes,
// or on other platforms, it reports the rest of the range as one extent, so
// the result is always correct, just not always sparse.
func ExtentsInRange(f *os.File, off, n int64) iter.Seq[result.Of[Extent]] {
	return func(yield func(result.Of[Extent]) bool) {
		if off < 0 || n < 0 {
			yield(result.Error[Extent](errors.New("sparse: negative offset or length")))
			return
		}
		end := off + n
		if end < off {
			end = 1<<63 - 1 // overflow
		}
		errStop := errors.New("stop")
		err := forEachExtent(f, off, end, func(e Extent) error {
			if !yield(result.Value(e)) {
				return errStop
			}
			return nil
		})
		if err != nil && err != errStop {
			yield(result.Error[Extent](err))
		}
	}
}

// Collect returns all the extents from seq, or the first error.
func Collect(seq iter.Seq[result.Of[Extent]]) ([]Extent, error) {
	var extents []Extent
	for r := range seq {
		e, err := r.Value()
		if err != nil {
			return nil, err
		}
		extents = append(extents, e)
	}
	return extents, nil
}
