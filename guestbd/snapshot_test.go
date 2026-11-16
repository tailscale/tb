package guestbd

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// newTestSnapshot returns a Snapshot over a zeroed base image of the given
// number of pages. If cache is false, the server has no page cache, so
// every dirty read goes to the snapshot's dirty file.
func newTestSnapshot(t *testing.T, pageSize, pages int, cache bool) *Snapshot {
	t.Helper()
	basePath := filepath.Join(t.TempDir(), "base.img")
	if err := os.WriteFile(basePath, make([]byte, pageSize*pages), 0o644); err != nil {
		t.Fatal(err)
	}
	opts := []ServerOption{WithPageSize(pageSize)}
	if !cache {
		opts = append(opts, WithMaxMem(0))
	}
	srv := NewServer(FileSource(basePath), opts...)
	t.Cleanup(func() { srv.Close() })
	snap, err := srv.NewSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { snap.Close() })
	return snap
}

func dirtyFileSize(t *testing.T, snap *Snapshot) int64 {
	t.Helper()
	fi, err := snap.dirtyFile.Stat()
	if err != nil {
		t.Fatal(err)
	}
	return fi.Size()
}

// TestDirtyFileReusesSlots checks that rewriting and trimming pages reuses
// dirty file slots rather than growing the file with every write.
func TestDirtyFileReusesSlots(t *testing.T) {
	const pageSize = 4096
	snap := newTestSnapshot(t, pageSize, 4, false)

	page := make([]byte, pageSize)
	for i := range 1000 {
		page[0] = byte(i)
		if _, err := snap.WriteAt(page, pageSize); err != nil {
			t.Fatal(err)
		}
		// Partial writes do a read-modify-write of the whole page.
		if _, err := snap.WriteAt([]byte{byte(i)}, 2*pageSize+7); err != nil {
			t.Fatal(err)
		}
	}
	// Two pages are dirty; at most one more slot may be in use transiently.
	if got, max := dirtyFileSize(t, snap), int64(3*pageSize); got > max {
		t.Errorf("after rewrites, dirty file is %d bytes; want <= %d", got, max)
	}

	if err := snap.handleTrim(0, 4*pageSize); err != nil {
		t.Fatal(err)
	}
	for range 100 {
		for p := range int64(3) {
			if _, err := snap.WriteAt(page, p*pageSize); err != nil {
				t.Fatal(err)
			}
		}
		if err := snap.handleTrim(0, 4*pageSize); err != nil {
			t.Fatal(err)
		}
	}
	if got, max := dirtyFileSize(t, snap), int64(4*pageSize); got > max {
		t.Errorf("after trims, dirty file is %d bytes; want <= %d", got, max)
	}
}

// TestDirtySlotReuseConcurrent checks that concurrent readers never see
// another page's data when a slot they're reading from is freed and reused
// by a concurrent write.
func TestDirtySlotReuseConcurrent(t *testing.T) {
	for _, cache := range []bool{false, true} {
		t.Run(fmt.Sprintf("cache=%v", cache), func(t *testing.T) {
			const (
				pageSize = 4096
				pages    = 4
				writes   = 2000
			)
			snap := newTestSnapshot(t, pageSize, pages, cache)

			// Each page's contents are its page number followed by a
			// write counter, repeated, so readers can tell whose data
			// they got and whether it's torn.
			fill := func(buf []byte, pageNum, gen uint64) {
				for i := 0; i < len(buf); i += 16 {
					binary.LittleEndian.PutUint64(buf[i:], pageNum)
					binary.LittleEndian.PutUint64(buf[i+8:], gen)
				}
			}

			var wg sync.WaitGroup
			done := make(chan struct{})
			for p := range uint64(pages) {
				wg.Go(func() {
					buf := make([]byte, pageSize)
					for gen := range uint64(writes) {
						fill(buf, p, gen+1)
						if _, err := snap.WriteAt(buf, int64(p)*pageSize); err != nil {
							t.Error(err)
							return
						}
					}
				})
			}
			var readers sync.WaitGroup
			for p := range uint64(pages) {
				readers.Go(func() {
					buf := make([]byte, pageSize)
					want := make([]byte, pageSize)
					for {
						select {
						case <-done:
							return
						default:
						}
						if _, err := snap.ReadAt(buf, int64(p)*pageSize); err != nil {
							t.Error(err)
							return
						}
						gen := binary.LittleEndian.Uint64(buf[8:])
						if gen == 0 && bytes.Equal(buf, make([]byte, pageSize)) {
							continue // not yet written
						}
						fill(want, p, gen)
						if !bytes.Equal(buf, want) {
							t.Errorf("page %d: read wrong or torn data (first words %d, %d)",
								p, binary.LittleEndian.Uint64(buf), gen)
							return
						}
					}
				})
			}
			wg.Wait()
			close(done)
			readers.Wait()

			if got, max := dirtyFileSize(t, snap), int64(2*pages*pageSize); got > max {
				t.Errorf("dirty file is %d bytes; want <= %d", got, max)
			}
		})
	}
}
