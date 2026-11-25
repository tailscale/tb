// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package sparse

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// The test file is 64 MiB with data at these offsets and holes elsewhere,
// including at the start and the end.
const (
	testSize = 64 << 20
	chunk    = 1 << 20
)

var dataAt = []int64{8 << 20, 9 << 20, 40 << 20}

// writeTestFile writes the test file and returns it open for reading.
func writeTestFile(t *testing.T) *os.File {
	t.Helper()
	path := filepath.Join(t.TempDir(), "sparse.img")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	makeSparse(t, f)
	if err := f.Truncate(testSize); err != nil {
		t.Fatal(err)
	}
	for i, off := range dataAt {
		if _, err := f.WriteAt(bytes.Repeat([]byte{byte('a' + i)}, chunk), off); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	f, err = os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}

// checkBounds reports extents that are empty, out of order, overlapping, or
// outside [off, off+n).
func checkBounds(t *testing.T, extents []Extent, off, n int64) {
	t.Helper()
	prevEnd := off
	for _, e := range extents {
		if e.Offset < prevEnd || e.Length <= 0 || e.End() > off+n {
			t.Errorf("bad extent %+v after %d in range [%d, %d)", e, prevEnd, off, off+n)
		}
		prevEnd = e.End()
	}
}

func TestExtents(t *testing.T) {
	f := writeTestFile(t)
	var extents []Extent
	for r := range Extents(f) {
		e, err := r.Value()
		if err != nil {
			t.Fatal(err)
		}
		// The file position is at the extent's start.
		if pos, err := f.Seek(0, io.SeekCurrent); err != nil || pos != e.Offset {
			t.Errorf("position %d, %v at extent %+v", pos, err, e)
		}
		extents = append(extents, e)
	}
	checkBounds(t, extents, 0, testSize)
	var covered int64
	for _, e := range extents {
		covered += e.Length
	}
	t.Logf("extents cover %d of %d bytes", covered, int64(testSize))

	// Every written byte must be in an extent. Whether the holes are left
	// out depends on the file system, so that's only logged.
	for _, off := range dataAt {
		if !slices.ContainsFunc(extents, func(e Extent) bool { return e.Offset <= off && off+chunk <= e.End() }) {
			t.Errorf("data at %d is not inside an extent of %v", off, extents)
		}
	}

	// Bytes outside the extents read as zeros.
	buf := make([]byte, chunk)
	for off := int64(0); off < testSize; off += chunk {
		if slices.ContainsFunc(extents, func(e Extent) bool { return off < e.End() && e.Offset < off+chunk }) {
			continue
		}
		if _, err := f.ReadAt(buf, off); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(buf, make([]byte, chunk)) {
			t.Errorf("nonzero bytes in the hole at %d", off)
		}
	}
}

func TestExtentsInRange(t *testing.T) {
	f := writeTestFile(t)
	tests := []struct {
		name   string
		off, n int64
		full   bool // the whole range is data, so extents must cover it
	}{
		{"prefix", 0, dataAt[0] + chunk/2, false},
		{"inside data", dataAt[0] + chunk/2, chunk, true},
		{"across data and hole", dataAt[1] + chunk/2, 4 * chunk, false},
		{"hole only", 20 << 20, chunk, false},
		{"past EOF", testSize - chunk, 4 * chunk, false},
		{"empty", dataAt[0], 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			extents, err := Collect(ExtentsInRange(f, tt.off, tt.n))
			if err != nil {
				t.Fatal(err)
			}
			checkBounds(t, extents, tt.off, tt.n)
			if tt.full {
				var covered int64
				for _, e := range extents {
					covered += e.Length
				}
				if covered != tt.n {
					t.Errorf("extents %v cover %d bytes, want all %d", extents, covered, tt.n)
				}
			}
		})
	}
}

func TestExtentsInRangeNegative(t *testing.T) {
	f := writeTestFile(t)
	for _, r := range [][2]int64{{-1, 10}, {0, -1}} {
		if _, err := Collect(ExtentsInRange(f, r[0], r[1])); err == nil {
			t.Errorf("ExtentsInRange(%d, %d): no error", r[0], r[1])
		}
	}
}

// TestManyExtents covers files with more extents than one Windows
// FSCTL_QUERY_ALLOCATED_RANGES call returns.
func TestManyExtents(t *testing.T) {
	const (
		islands = 200     // more than the 64 ranges per Windows query
		stride  = 1 << 20 // well past NTFS's 64 KiB sparse unit
		island  = 4 << 10
	)
	path := filepath.Join(t.TempDir(), "islands.img")
	w, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	makeSparse(t, w)
	size := int64(islands * stride)
	if err := w.Truncate(size); err != nil {
		t.Fatal(err)
	}
	for i := range islands {
		if _, err := w.WriteAt(bytes.Repeat([]byte{'d'}, island), int64(i)*stride); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	extents, err := Collect(Extents(f))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%d islands in %d extents", islands, len(extents))
	for i := range islands {
		off := int64(i) * stride
		if !slices.ContainsFunc(extents, func(e Extent) bool { return e.Offset <= off && off+island <= e.End() }) {
			t.Fatalf("island at %d is not inside an extent", off)
		}
	}
	checkBounds(t, extents, 0, size)
}

func TestBreak(t *testing.T) {
	f := writeTestFile(t)
	n := 0
	for r := range Extents(f) {
		if _, err := r.Value(); err != nil {
			t.Fatal(err)
		}
		n++
		break // The iterator must not yield again, or range panics.
	}
	if n != 1 {
		t.Errorf("got %d extents before break, want 1", n)
	}
}

func TestDenseFile(t *testing.T) {
	const size = 3*chunk + 17
	path := filepath.Join(t.TempDir(), "dense")
	if err := os.WriteFile(path, bytes.Repeat([]byte("x"), size), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	extents, err := Collect(Extents(f))
	if err != nil {
		t.Fatal(err)
	}
	var covered int64
	for _, e := range extents {
		covered += e.Length
	}
	if covered != size {
		t.Errorf("extents %v cover %d bytes, want all %d", extents, covered, size)
	}
}

func TestEmptyFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "empty")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if extents, err := Collect(Extents(f)); err != nil || len(extents) != 0 {
		t.Errorf("Extents of an empty file = %v, %v", extents, err)
	}
}

func TestClosedFile(t *testing.T) {
	f := writeTestFile(t)
	f.Close()
	if _, err := Collect(Extents(f)); err == nil {
		t.Error("Extents of a closed file: no error")
	}
}
