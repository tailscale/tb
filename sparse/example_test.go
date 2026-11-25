// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package sparse_test

import (
	"encoding/binary"
	"io"
	"os"

	"github.com/tailscale/tb/sparse"
)

// This sends a file's size and then each data extent as an offset, a length,
// and that many bytes, so the receiver can recreate the file with its holes.
func ExampleExtentsInRange() {
	send := func(w io.Writer, f *os.File) error {
		fi, err := f.Stat()
		if err != nil {
			return err
		}
		size := fi.Size()
		if err := binary.Write(w, binary.BigEndian, size); err != nil {
			return err
		}
		// Use the same size as the header, in case f grows meanwhile.
		for r := range sparse.ExtentsInRange(f, 0, size) {
			e, err := r.Value()
			if err != nil {
				return err
			}
			if err := binary.Write(w, binary.BigEndian, [2]int64{e.Offset, e.Length}); err != nil {
				return err
			}
			if _, err := io.Copy(w, io.NewSectionReader(f, e.Offset, e.Length)); err != nil {
				return err
			}
		}
		return nil
	}
	_ = send
}
