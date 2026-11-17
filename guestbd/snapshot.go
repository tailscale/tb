package guestbd

import (
	"fmt"
	"io"
	"maps"
	"os"
	"slices"
	"sync"
)

// Compile-time interface checks.
var (
	_ io.ReaderAt = (*Snapshot)(nil)
	_ io.WriterAt = (*Snapshot)(nil)
)

// Snapshot is a writable layer on top of a read-only base image.
// Reads fall through to the shared base image, while writes are tracked
// in the snapshot. A Snapshot can outlive individual TCP connections,
// allowing reconnections to reattach to the same writable state.
//
// Snapshot implements io.ReaderAt and io.WriterAt.
type Snapshot struct {
	server *Server
	roFile *baseImageState

	mu           sync.Mutex
	closed       bool                    // Close has been called
	dirtyPages   map[int64]dirtyPageInfo // pageNum => dirty info
	dirtyFile    *os.File                // temp file for dirty page data; immutable
	nextDirtyNum int                     // next never-used slot in dirtyFile
	freeSlots    []int                   // dirtyFile slots available for reuse
	slots        []slotState             // by dirtyFile slot, for slots < nextDirtyNum
}

// dirtyPageInfo tracks a written page in a snapshot.
type dirtyPageInfo struct {
	hash     pageHash
	dirtyNum int // index into the snapshot's dirtyFile
}

// slotState tracks the readers of a dirtyFile slot.
//
// readPageData reads a slot without holding Snapshot.mu, so it pins the
// slot while reading to keep a concurrent WriteAt or trim from freeing the
// slot and letting another write overwrite it mid-read.
type slotState struct {
	readers int  // readPageData calls currently reading this slot
	freed   bool // freed while pinned; goes to freeSlots when readers hits 0
}

// newSnapshot creates a new Snapshot for the given read-only backing file.
// It creates an unlinked temporary file for dirty page storage.
func newSnapshot(srv *Server, ro *baseImageState) (*Snapshot, error) {
	tmpFile, err := os.CreateTemp("", "guestbd-dirty-*")
	if err != nil {
		return nil, fmt.Errorf("creating temp file: %w", err)
	}
	// Unlink immediately so it's cleaned up when the fd closes.
	os.Remove(tmpFile.Name())

	return &Snapshot{
		server:     srv,
		roFile:     ro,
		dirtyPages: make(map[int64]dirtyPageInfo),
		dirtyFile:  tmpFile,
	}, nil
}

// Close releases the snapshot's dirty page storage and unregisters it from
// the server. It does not close any TCP connection using the snapshot.
// Reads and writes of dirty pages after or during Close fail. Calls after
// the first do nothing.
func (c *Snapshot) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	c.mu.Unlock()

	// dirtyFile is never reassigned, and os.File allows Close concurrent
	// with ReadAt and WriteAt, which then fail with os.ErrClosed.
	c.dirtyFile.Close()

	c.server.mu.Lock()
	c.server.snapshots.Delete(c)
	c.server.mu.Unlock()

	c.server.releaseBaseImageState(c.roFile)
	return nil
}

// readPageData reads a full page into buf, checking dirty pages first,
// then falling back to the readonly file.
// buf must be at least pageSize bytes long or readPageData panics.
func (c *Snapshot) readPageData(buf []byte, pageNum int64) error {
	pageSize := c.server.pageSize
	_ = buf[pageSize-1] // bounds check hint; panics if too small

	c.server.readPages.Add(1)

	c.mu.Lock()
	dp, dirty := c.dirtyPages[pageNum]
	if dirty {
		c.slots[dp.dirtyNum].readers++
	}
	c.mu.Unlock()

	if dirty {
		defer c.unpinSlot(dp.dirtyNum)
		c.server.readPath.Add("from_write", 1)
		cache := c.server.cache
		if cache != nil {
			// Try the global cache first.
			if data, ok := cache.Get(dp.hash); ok {
				copy(buf, data)
				return nil
			}
		}
		// Read from the snapshot's dirty file. If the page is rewritten
		// meanwhile, this returns its contents from when the read began,
		// which still match dp.hash.
		_, err := c.dirtyFile.ReadAt(buf[:pageSize], int64(dp.dirtyNum)*int64(pageSize))
		if err != nil {
			return fmt.Errorf("reading dirty page: %w", err)
		}
		if cache != nil {
			cache.Put(dp.hash, buf[:pageSize])
		}
		return nil
	}

	_, result, err := c.roFile.readPage(buf, pageNum)
	if err != nil {
		return err
	}
	switch result {
	case readFromCache:
		c.server.readPath.Add("base_mem", 1)
	case readFromDiskCold:
		c.server.readPath.Add("base_disk_cold", 1)
	case readFromDiskMiss:
		c.server.readPath.Add("base_disk_miss", 1)
	}
	return nil
}

// ReadAt reads len(p) bytes from the snapshot starting at byte offset off.
// It implements io.ReaderAt.
func (c *Snapshot) ReadAt(p []byte, off int64) (int, error) {
	length := uint64(len(p))
	c.server.readBytes.Add(int64(length))

	pageSize := uint64(c.server.pageSize)

	pos := uint64(0)
	for pos < length {
		absOffset := uint64(off) + pos
		pageNum := int64(absOffset / pageSize)
		pageOffset := absOffset % pageSize

		// Read up to the end of the page or the end of the requested length,
		// whichever is smaller. Usually this will be pageSize, except for the
		// first and last pages if the ReadAt call isn't page-aligned.
		n := min(pageSize-pageOffset, length-pos)

		if pageOffset == 0 && n == pageSize {
			// Full page; read directly into the caller's buffer.
			if err := c.readPageData(p[pos:pos+pageSize], pageNum); err != nil {
				return int(pos), err
			}
		} else {
			// Partial page; need a scratch buffer from the pool.
			bufp := c.server.pageBufPool.Get().(*[]byte)
			if err := c.readPageData(*bufp, pageNum); err != nil {
				return int(pos), err
			}
			copy(p[pos:pos+n], (*bufp)[pageOffset:pageOffset+n])
			c.server.pageBufPool.Put(bufp)
		}
		pos += n
	}
	return int(pos), nil
}

// WriteAt writes len(p) bytes to the snapshot starting at byte offset off.
// Sub-page writes trigger a read-modify-write cycle.
// It implements io.WriterAt.
func (c *Snapshot) WriteAt(p []byte, off int64) (int, error) {
	length := uint64(len(p))
	pageSize := uint64(c.server.pageSize)

	c.server.writeBytes.Add(int64(length))

	offset := uint64(off)
	pos := uint64(0)
	for pos < length {
		absOffset := offset + pos
		pageNum := int64(absOffset / pageSize)
		pageOffset := absOffset % pageSize

		n := pageSize - pageOffset
		if n > length-pos {
			n = length - pos
		}

		pageData := make([]byte, pageSize)
		if pageOffset == 0 && n == pageSize {
			// Full page write.
			copy(pageData, p[pos:pos+n])
		} else {
			// Partial page write: read-modify-write.
			if err := c.readPageData(pageData, pageNum); err != nil {
				return int(pos), err
			}
			copy(pageData[pageOffset:], p[pos:pos+n])
		}

		cache := c.server.cache
		var h pageHash
		if cache != nil {
			h = hashPage(pageData)
		}

		// Write the page to a slot that no reader can see yet, then
		// publish it and free the page's previous slot, if any.
		c.mu.Lock()
		dirtyNum := c.allocSlotLocked()
		c.mu.Unlock()

		_, err := c.dirtyFile.WriteAt(pageData, int64(dirtyNum)*int64(pageSize))
		if err != nil {
			c.mu.Lock()
			c.freeSlotLocked(dirtyNum)
			c.mu.Unlock()
			return int(pos), fmt.Errorf("writing dirty page: %w", err)
		}

		c.mu.Lock()
		if old, ok := c.dirtyPages[pageNum]; ok {
			c.freeSlotLocked(old.dirtyNum)
		}
		c.dirtyPages[pageNum] = dirtyPageInfo{
			hash:     h,
			dirtyNum: dirtyNum,
		}
		c.mu.Unlock()

		if cache != nil {
			cache.Put(h, pageData)
		}
		c.server.writePages.Add(1)
		pos += n
	}
	return int(pos), nil
}

// allocSlotLocked returns a dirtyFile slot for a new page write, reusing a
// freed slot if one is available so that dirtyFile's size is bounded by the
// number of distinct dirty pages (plus writes and reads in flight) rather
// than the total number of writes. c.mu must be held.
func (c *Snapshot) allocSlotLocked() int {
	if n := len(c.freeSlots); n > 0 {
		slot := c.freeSlots[n-1]
		c.freeSlots = c.freeSlots[:n-1]
		return slot
	}
	slot := c.nextDirtyNum
	c.nextDirtyNum++
	c.slots = append(c.slots, slotState{})
	return slot
}

// freeSlotLocked marks a dirtyFile slot as no longer referenced by
// dirtyPages. The slot becomes available for reuse once no readers have it
// pinned. c.mu must be held.
func (c *Snapshot) freeSlotLocked(slot int) {
	if c.slots[slot].readers > 0 {
		c.slots[slot].freed = true
		return
	}
	c.freeSlots = append(c.freeSlots, slot)
}

// unpinSlot releases a slot pinned by readPageData, completing a deferred
// free if this was the last reader.
func (c *Snapshot) unpinSlot(slot int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	st := &c.slots[slot]
	st.readers--
	if st.readers == 0 && st.freed {
		st.freed = false
		c.freeSlots = append(c.freeSlots, slot)
	}
}

// WriteDirtyTo writes every page written to the snapshot, at its offset,
// to w, and returns the number of pages written. Writing them to a copy of
// the base image makes that copy equal to the snapshot's contents, which
// persists a snapshot (for example, a VM image warmed up by booting it over
// NBD). The final page is truncated to the image size.
//
// The caller should ensure nothing writes to the snapshot meanwhile;
// concurrent writes may or may not be included.
func (c *Snapshot) WriteDirtyTo(w io.WriterAt) (pages int, err error) {
	c.mu.Lock()
	pageNums := slices.Sorted(maps.Keys(c.dirtyPages))
	c.mu.Unlock()

	pageSize := int64(c.server.pageSize)
	size := c.roFile.size
	bufp := c.server.pageBufPool.Get().(*[]byte)
	defer c.server.pageBufPool.Put(bufp)
	buf := *bufp
	for _, p := range pageNums {
		off := p * pageSize
		if off >= size {
			continue
		}
		if err := c.readPageData(buf, p); err != nil {
			return pages, err
		}
		n := min(pageSize, size-off)
		if _, err := w.WriteAt(buf[:n], off); err != nil {
			return pages, err
		}
		pages++
	}
	return pages, nil
}

// handleTrim zeros the given byte range and reverts any fully-covered
// dirty pages to the base image. Partial pages at the start and end of
// the range are zero-filled via WriteAt.
func (c *Snapshot) handleTrim(offset, length uint64) error {
	end := offset + length
	pageSize := uint64(c.server.pageSize)

	// Round up to the first fully-covered page.
	firstFullPage := int64((offset + pageSize - 1) / pageSize)
	// Round down to the last fully-covered page (exclusive).
	lastFullPageExcl := int64(end / pageSize)

	// Zero partial start page.
	if startRem := offset % pageSize; startRem != 0 {
		n := min(pageSize-startRem, length)
		bufp := c.server.pageBufPool.Get().(*[]byte)
		clear((*bufp)[:n])
		_, err := c.WriteAt((*bufp)[:n], int64(offset))
		c.server.pageBufPool.Put(bufp)
		if err != nil {
			return err
		}
	}

	// Zero partial end page, if on a different page than the start.
	if endRem := end % pageSize; endRem != 0 && int64(end/pageSize) >= firstFullPage {
		bufp := c.server.pageBufPool.Get().(*[]byte)
		clear((*bufp)[:endRem])
		_, err := c.WriteAt((*bufp)[:endRem], int64(lastFullPageExcl)*int64(pageSize))
		c.server.pageBufPool.Put(bufp)
		if err != nil {
			return err
		}
	}

	// Delete fully-covered pages, reverting them to the base image.
	c.mu.Lock()
	defer c.mu.Unlock()
	for p := firstFullPage; p < lastFullPageExcl; p++ {
		if dp, ok := c.dirtyPages[p]; ok {
			c.freeSlotLocked(dp.dirtyNum)
			delete(c.dirtyPages, p)
		}
	}
	return nil
}
