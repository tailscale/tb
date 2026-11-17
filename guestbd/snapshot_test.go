package guestbd

import (
	"bytes"
	"encoding/binary"
	"errors"
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

			// Slots in use are at most one live slot per page, plus one
			// unpublished slot per writer, plus one pinned slot per
			// reader.
			if got, max := dirtyFileSize(t, snap), int64(3*pages*pageSize); got > max {
				t.Errorf("dirty file is %d bytes; want <= %d", got, max)
			}
		})
	}
}

// TestPinnedSlotNotReused checks that a dirty file slot freed while a
// reader has it pinned isn't reused until the reader unpins it.
func TestPinnedSlotNotReused(t *testing.T) {
	const pageSize = 4096
	snap := newTestSnapshot(t, pageSize, 4, false)

	page := make([]byte, pageSize)
	if _, err := snap.WriteAt(page, 0); err != nil {
		t.Fatal(err)
	}
	snap.mu.Lock()
	slot := snap.dirtyPages[0].dirtyNum
	snap.slots[slot].readers++ // as readPageData does
	snap.mu.Unlock()

	// Rewriting page 0 frees its old slot, but the pin must keep the
	// next write from reusing it.
	for p := range int64(2) {
		if _, err := snap.WriteAt(page, p*pageSize); err != nil {
			t.Fatal(err)
		}
		snap.mu.Lock()
		got := snap.dirtyPages[p].dirtyNum
		snap.mu.Unlock()
		if got == slot {
			t.Fatalf("write to page %d reused pinned slot %d", p, slot)
		}
	}

	snap.unpinSlot(slot)
	if _, err := snap.WriteAt(page, 2*pageSize); err != nil {
		t.Fatal(err)
	}
	snap.mu.Lock()
	got := snap.dirtyPages[2].dirtyNum
	snap.mu.Unlock()
	if got != slot {
		t.Errorf("write after unpin got slot %d; want reused slot %d", got, slot)
	}
}

// TestCloseDuringIO checks that closing a snapshot while reads and writes
// are in flight makes them fail cleanly, without a data race, and that
// closing it twice releases its base image only once.
func TestCloseDuringIO(t *testing.T) {
	const pageSize = 4096
	snap := newTestSnapshot(t, pageSize, 4, false)
	srv := snap.server

	// A second snapshot shares the base image, so a double release by
	// snap would mark the base image inactive while other still uses it.
	other, err := srv.NewSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()

	page := make([]byte, pageSize)
	if _, err := snap.WriteAt(page, 0); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			buf := make([]byte, pageSize)
			for {
				if _, err := snap.WriteAt(buf, 0); err != nil {
					if !errors.Is(err, os.ErrClosed) {
						t.Errorf("WriteAt: got %v; want os.ErrClosed", err)
					}
					return
				}
				if _, err := snap.ReadAt(buf, 0); err != nil {
					if !errors.Is(err, os.ErrClosed) {
						t.Errorf("ReadAt: got %v; want os.ErrClosed", err)
					}
					return
				}
			}
		})
	}
	snap.Close()
	wg.Wait()

	snap.Close()
	if got := srv.baseImagesActive.Value(); got != 1 {
		t.Errorf("after closing one of two snapshots twice, %d base images active; want 1", got)
	}
}
