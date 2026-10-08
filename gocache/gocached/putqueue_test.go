package gocached

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/pierrec/lz4/v4"
)

func newTestPutQueue(t *testing.T) *putQueue {
	t.Helper()
	return newPutQueue(&Server{clock: time.Now}, t.TempDir())
}

func TestPutQueueInsertDup(t *testing.T) {
	q := newTestPutQueue(t)

	reserved, err := q.reserve(context.Background(), 2)
	if err != nil {
		t.Fatal(err)
	}
	p1 := &pendingPut{key: actionKey{ActionID: "aa11"}, sha256hex: "s1", reservation: reserved}
	if dup := q.insert(p1); dup {
		t.Fatal("first insert reported dup")
	}
	p2 := &pendingPut{key: actionKey{ActionID: "aa11"}, sha256hex: "s2"}
	if dup := q.insert(p2); !dup {
		t.Fatal("second insert of same key not reported as dup")
	}

	// First PUT wins: the original entry is still the one served.
	got, ok := q.lookup(p1.key)
	if !ok || got.sha256hex != "s1" {
		t.Fatalf("lookup = %+v, %v; want the first entry", got, ok)
	}

	// Same ActionID in a different namespace is not a dup.
	p3 := &pendingPut{key: actionKey{NamespaceID: 7, ActionID: "aa11"}}
	if dup := q.insert(p3); dup {
		t.Fatal("insert in different namespace reported dup")
	}

	q.retire(p1)
	if _, ok := q.lookup(p1.key); ok {
		t.Fatal("entry still pending after retire")
	}
}

func TestPutQueueReserveBackpressure(t *testing.T) {
	q := newTestPutQueue(t)
	ctx := context.Background()

	// Fill the byte budget entirely.
	r1, err := q.reserve(ctx, q.spoolCap)
	if err != nil {
		t.Fatal(err)
	}
	if r1.bytes != q.spoolCap || r1.inline {
		t.Fatalf("reserved = %+v, want %d spooled bytes", r1, q.spoolCap)
	}

	// A blocked reservation aborts when its context is canceled (e.g. the
	// HTTP client goes away).
	const spooled = smallObjectSize + 1
	cctx, cancel := context.WithCancel(ctx)
	errc := make(chan error, 1)
	go func() {
		_, err := q.reserve(cctx, spooled)
		errc <- err
	}()
	select {
	case err := <-errc:
		t.Fatalf("reserve unexpectedly returned %v while at capacity", err)
	case <-time.After(50 * time.Millisecond):
	}
	cancel()
	select {
	case err := <-errc:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("blocked reserve = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("blocked reserve didn't abort on context cancel")
	}

	// Freed capacity admits new reservations.
	q.unreserve(r1)
	r2, err := q.reserve(ctx, spooled)
	if err != nil {
		t.Fatal(err)
	}
	q.unreserve(r2)

	// A single blob bigger than the whole budget is clamped and admitted.
	big, err := q.reserve(ctx, q.spoolCap*3)
	if err != nil {
		t.Fatal(err)
	}
	if big.bytes != q.spoolCap {
		t.Fatalf("oversized reservation = %+v, want clamp to %d", big, q.spoolCap)
	}
	q.unreserve(big)
}

func TestPutQueueReserveCountCap(t *testing.T) {
	q := newTestPutQueue(t)
	ctx := context.Background()

	const spooled = smallObjectSize + 1
	for range putQueuePendingCountCap {
		if _, err := q.reserve(ctx, spooled); err != nil {
			t.Fatal(err)
		}
	}

	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := q.reserve(cctx, spooled); !errors.Is(err, context.Canceled) {
		t.Fatalf("reserve over count cap = %v, want context.Canceled", err)
	}

	q.unreserve(putReservation{bytes: spooled})
	if _, err := q.reserve(ctx, spooled); err != nil {
		t.Fatalf("reserve after freeing a slot: %v", err)
	}
}

func TestCreateCreatingDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "ab")
	path := filepath.Join(dir, "file")
	calls := 0
	create := func() (*os.File, error) {
		calls++
		return os.Create(path)
	}
	check := func(wantCalls int) {
		t.Helper()
		f, err := createCreatingDir(dir, create)
		if err != nil {
			t.Fatalf("createCreatingDir: %v", err)
		}
		f.Close()
		if calls != wantCalls {
			t.Errorf("create called %d times; want %d", calls, wantCalls)
		}
		calls = 0
	}

	// The directory doesn't exist yet: created on demand, then retried.
	check(2)
	// The directory exists: one call.
	check(1)
	// The directory was removed out from under us: recreated, not cached.
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	check(2)

	// Other errors are returned as is, without creating anything.
	errBoom := errors.New("boom")
	other := filepath.Join(t.TempDir(), "other")
	if _, err := createCreatingDir(other, func() (*os.File, error) { return nil, errBoom }); err != errBoom {
		t.Errorf("err = %v; want %v", err, errBoom)
	}
	if _, err := os.Stat(other); !os.IsNotExist(err) {
		t.Errorf("directory created after a non-ENOENT error: %v", err)
	}
}

// TestPutQueueReserveInlineLane checks that small (inline) PUTs, including
// zero-byte ones, are admitted through their own lane while the spooled
// lane is completely full, and that the inline lane has its own cap.
func TestPutQueueReserveInlineLane(t *testing.T) {
	q := newTestPutQueue(t)
	ctx := context.Background()

	// reserve tries a non-blocking acquire before consulting ctx, so a
	// pre-canceled context distinguishes "admitted immediately" (nil)
	// from "would have blocked" (context.Canceled) without any waiting.
	canceled, cancel := context.WithCancel(ctx)
	cancel()

	// Exhaust the spooled lane's byte budget with one big blob.
	big, err := q.reserve(ctx, q.spoolCap)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := q.reserve(canceled, smallObjectSize+1); !errors.Is(err, context.Canceled) {
		t.Fatalf("spooled reserve at capacity = %v, want context.Canceled", err)
	}

	// Inline PUTs, including empty ones, are still admitted immediately.
	for _, size := range []int64{0, 1, smallObjectSize} {
		r, err := q.reserve(canceled, size)
		if err != nil {
			t.Fatalf("inline reserve(%d) while spooled lane full = %v, want admitted", size, err)
		}
		if !r.inline || r.bytes != 0 {
			t.Fatalf("inline reserve(%d) = %+v, want inline reservation", size, r)
		}
	}

	// The inline lane has its own cap, independent of the spooled lane.
	for range putQueuePendingInlineCap - 3 {
		if _, err := q.reserve(canceled, 0); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := q.reserve(canceled, 0); !errors.Is(err, context.Canceled) {
		t.Fatalf("inline reserve over inline cap = %v, want context.Canceled", err)
	}
	q.unreserve(putReservation{inline: true})
	if _, err := q.reserve(canceled, 0); err != nil {
		t.Fatalf("inline reserve after freeing a slot: %v", err)
	}

	// And freeing spooled room doesn't require touching the inline lane.
	q.unreserve(big)
	if _, err := q.reserve(canceled, smallObjectSize+1); err != nil {
		t.Fatalf("spooled reserve after freeing bytes: %v", err)
	}
}

func TestPutQueueSpoolBlob(t *testing.T) {
	q := newTestPutQueue(t)

	// Small content stays uncompressed on disk.
	small := []byte("hello put queue")
	diskSize, path, err := q.spoolBlob(int64(len(small)), bytes.NewReader(small))
	if err != nil {
		t.Fatal(err)
	}
	if diskSize != int64(len(small)) {
		t.Errorf("small diskSize = %d, want %d", diskSize, len(small))
	}
	if got, err := os.ReadFile(path); err != nil || !bytes.Equal(got, small) {
		t.Errorf("small spool file = %q, %v; want %q", got, err, small)
	}
	if dir := filepath.Dir(path); dir != q.dir {
		t.Errorf("spool file in %q, want %q", dir, q.dir)
	}
	if !strings.HasPrefix(filepath.Base(path), "put-") {
		t.Errorf("spool file name %q lacks put- prefix", filepath.Base(path))
	}

	// Content at or above the lz4 threshold is compressed; decompressing
	// the spool file yields the original bytes.
	big := bytes.Repeat([]byte("gocached"), 1000)
	diskSize, path, err = q.spoolBlob(int64(len(big)), bytes.NewReader(big))
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if fi, err := f.Stat(); err != nil || fi.Size() != diskSize {
		t.Errorf("big spool file size = %v, %v; want %d", fi.Size(), err, diskSize)
	}
	got, err := io.ReadAll(lz4.NewReader(f))
	if err != nil || !bytes.Equal(got, big) {
		t.Errorf("big spool file decompressed to %d bytes, %v; want %d bytes", len(got), err, len(big))
	}

	// A short body (fewer bytes than the declared size) fails and cleans up
	// its spool file.
	_, badPath, err := q.spoolBlob(100, strings.NewReader("short"))
	if err == nil {
		t.Fatal("spoolBlob with short body unexpectedly succeeded")
	}
	if badPath != "" {
		t.Errorf("failed spoolBlob returned path %q, want empty", badPath)
	}
	ents, err := os.ReadDir(q.dir)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := len(ents), 2; got != want {
		t.Errorf("spool dir has %d files, want %d (failed spool not cleaned up?)", got, want)
	}
}

// makePending builds a pendingPut for the given content the way handlePut
// will: reserving queue room, spooling big content to a queue file, and
// keeping small content in memory.
func makePending(t *testing.T, q *putQueue, ns int64, actionID string, content []byte) *pendingPut {
	t.Helper()
	reserved, err := q.reserve(context.Background(), int64(len(content)))
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(content)
	p := &pendingPut{
		key:              actionKey{NamespaceID: ns, ActionID: actionID},
		sha256hex:        hex.EncodeToString(sum[:]),
		storedSize:       int64(len(content)),
		uncompressedSize: int64(len(content)),
		createTime:       q.srv.now().Unix(),
		reservation:      reserved,
	}
	if len(content) <= smallObjectSize {
		p.smallData = content
	} else {
		diskSize, path, err := q.spoolBlob(int64(len(content)), bytes.NewReader(content))
		if err != nil {
			t.Fatal(err)
		}
		p.storedSize = diskSize
		p.queueFile = path
	}
	return p
}

func TestPutQueueDrain(t *testing.T) {
	st := newServerTester(t)
	q := st.srv.putq

	small := []byte("inline data")
	big := bytes.Repeat([]byte("gocached"), 1000)

	pSmall := makePending(t, q, 0, "aa01", small)
	if dup := q.enqueue(pSmall); dup {
		t.Fatal("small enqueue reported dup")
	}
	pBig := makePending(t, q, 0, "bb02", big)
	if dup := q.enqueue(pBig); dup {
		t.Fatal("big enqueue reported dup")
	}

	// While pending, entries are visible via lookup.
	if _, ok := q.lookup(pSmall.key); !ok {
		t.Fatal("small entry not pending before drain")
	}

	if err := st.srv.drainPendingPuts(); err != nil {
		t.Fatal(err)
	}

	if n, _ := q.pendingStats(); n != 0 {
		t.Errorf("%d entries still pending after drain", n)
	}
	st.wantMetric(&st.srv.m.PutQueueFlushes, 1)
	st.wantMetric(&st.srv.m.PutQueueFlushedItems, 2)
	st.wantMetric(&st.srv.m.PutQueueFlushDups, 0)
	st.wantMetric(&st.srv.m.PutQueueDropped, 0)

	// The big blob was installed in the main blob directory (lz4'd), and
	// with no hot tier its spool file is deleted.
	if got, want := st.diskFiles(), []string{pBig.blobName()}; !slices.Equal(got, want) {
		t.Errorf("disk files = %v, want %v", got, want)
	}
	if ents, err := os.ReadDir(q.dir); err != nil || len(ents) != 0 {
		t.Errorf("queue dir has %d entries after drain, err=%v; want empty", len(ents), err)
	}

	// Metadata is committed: both actions exist in SQLite.
	var n int
	if err := st.srv.db.QueryRow("SELECT COUNT(*) FROM Actions").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("Actions rows = %d, want 2", n)
	}

	// A second drain is a no-op.
	if err := st.srv.drainPendingPuts(); err != nil {
		t.Fatal(err)
	}
	st.wantMetric(&st.srv.m.PutQueueFlushes, 0)
}

// TestPutQueueSkipIntentWhenPresent verifies that a spooled blob already in
// the main blob directory is neither copied nor given a cleanup intent, and
// still commits, while a new blob gets its intent as usual.
func TestPutQueueSkipIntentWhenPresent(t *testing.T) {
	st := newServerTester(t)
	q := st.srv.putq
	content := bytes.Repeat([]byte("present"), 1000)

	p1 := makePending(t, q, 0, "ee01", content)
	q.enqueue(p1)
	st.drain()
	st.wantMetric(&st.srv.m.PutQueueCopySkips, 0)
	fi1, err := os.Stat(q.mainPath(p1))
	if err != nil {
		t.Fatal(err)
	}

	// A different action with the same output: its blob is already there.
	p2 := makePending(t, q, 0, "ee02", content)
	q.enqueue(p2)
	if err := q.installInMain(p2); err != nil {
		t.Fatal(err)
	}
	if p2.intentPath != "" {
		t.Errorf("intent %s written for an already-present blob", p2.intentPath)
	}
	if got := intentFiles(t, q); len(got) != 0 {
		t.Errorf("intents written: %q; want none", got)
	}
	st.wantMetric(&st.srv.m.PutQueueCopySkips, 1)
	if fi2, err := os.Stat(q.mainPath(p2)); err != nil || !os.SameFile(fi1, fi2) {
		t.Errorf("present blob was replaced (err=%v)", err)
	}

	// A new blob still gets an intent before its copy.
	p3 := makePending(t, q, 0, "ee03", bytes.Repeat([]byte("new"), 1000))
	q.enqueue(p3)
	if err := q.installInMain(p3); err != nil {
		t.Fatal(err)
	}
	if p3.intentPath == "" {
		t.Fatal("no intent written for a new blob")
	}
	if _, err := os.Stat(p3.intentPath); err != nil {
		t.Errorf("intent for new blob: %v", err)
	}
	st.wantMetric(&st.srv.m.PutQueueCopySkips, 0)

	// Both commit.
	st.drain()
	var n int
	if err := st.srv.db.QueryRow("SELECT COUNT(*) FROM Actions").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Errorf("Actions rows = %d, want 3", n)
	}
	if _, err := os.Stat(q.mainPath(p3)); err != nil {
		t.Errorf("new blob not installed: %v", err)
	}
}

func TestPutQueueDrainFlushDup(t *testing.T) {
	st := newServerTester(t)
	q := st.srv.putq

	// Commit an action, then make the same action pending again: the flush
	// discovers the dup and doesn't double-count the shard delta.
	p1 := makePending(t, q, 0, "cc03", []byte("first"))
	q.enqueue(p1)
	if err := st.srv.drainPendingPuts(); err != nil {
		t.Fatal(err)
	}
	preCount, preBytes := st.srv.sumShardDeltas()

	p2 := makePending(t, q, 0, "cc03", []byte("other content"))
	if dup := q.enqueue(p2); dup {
		t.Fatal("enqueue after flush reported pending-dup; want flush-time dup")
	}
	if err := st.srv.drainPendingPuts(); err != nil {
		t.Fatal(err)
	}
	st.wantMetric(&st.srv.m.PutQueueFlushDups, 1)
	if c, b := st.srv.sumShardDeltas(); c != preCount || b != preBytes {
		t.Errorf("shard deltas changed on dup flush: (%d, %d) -> (%d, %d)", preCount, preBytes, c, b)
	}
}

func TestPutQueueDrainHotInstall(t *testing.T) {
	st := newServerTester(t, WithHotDir(filepath.Join(t.TempDir(), "hot")), WithHotCapacity(1<<20))
	q := st.srv.putq

	big := bytes.Repeat([]byte("hot data"), 1000)
	p := makePending(t, q, 0, "dd04", big)
	q.enqueue(p)

	if err := st.srv.drainPendingPuts(); err != nil {
		t.Fatal(err)
	}

	// The spool file was renamed into the hot tier and indexed, and the
	// main blob directory got its own copy.
	if got, want := st.hotFiles(), []string{p.blobName()}; !slices.Equal(got, want) {
		t.Errorf("hot files = %v, want %v", got, want)
	}
	if got, want := st.srv.hot.usageBytes(), p.storedSize; got != want {
		t.Errorf("hot usage = %d, want %d", got, want)
	}
	if got, want := st.diskFiles(), []string{p.blobName()}; !slices.Equal(got, want) {
		t.Errorf("disk files = %v, want %v", got, want)
	}
	if ents, err := os.ReadDir(q.dir); err != nil || len(ents) != 0 {
		t.Errorf("queue dir has %d entries after drain, err=%v; want empty", len(ents), err)
	}
}

// intentFiles returns the cleanup intent files under q's cleanup directory,
// in shard subdirectories or (from older versions) directly in it, as paths
// relative to it.
func intentFiles(t *testing.T, q *putQueue) []string {
	t.Helper()
	var ret []string
	err := filepath.WalkDir(q.cleanupDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			rel, _ := filepath.Rel(q.cleanupDir, path)
			ret = append(ret, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return ret
}

func TestPutQueueCleanupIntents(t *testing.T) {
	st := newServerTester(t)
	q := st.srv.putq

	exists := func(path string) bool {
		t.Helper()
		_, err := os.Stat(path)
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		return err == nil
	}
	writeIntent := func(due int64, blobName string) string {
		t.Helper()
		path := filepath.Join(q.cleanupDir, fmt.Sprintf("%08x-%s", due, blobName))
		if err := os.WriteFile(path, nil, 0644); err != nil {
			t.Fatal(err)
		}
		return path
	}
	blobPath := func(blobName string) string {
		return filepath.Join(st.srv.dir, blobName[:2], blobName)
	}
	now := st.srv.now().Unix()

	// Common case: a drained PUT leaves a blob and no intents behind.
	committed := makePending(t, q, 0, "aa01", bytes.Repeat([]byte("committed"), 1000))
	q.enqueue(committed)
	st.drain()
	if got := intentFiles(t, q); len(got) != 0 {
		t.Fatalf("intents left after drain: %q", got)
	}
	if !exists(blobPath(committed.blobName())) {
		t.Fatal("committed blob missing after drain")
	}

	// A crash-orphaned blob (file on disk, intent due, no SQLite row) is
	// swept along with its intent.
	orphanSum := sha256.Sum256([]byte("orphan"))
	orphanName := hex.EncodeToString(orphanSum[:]) + ".lz4"
	if err := os.MkdirAll(filepath.Dir(blobPath(orphanName)), 0750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(blobPath(orphanName), []byte("orphan bytes"), 0644); err != nil {
		t.Fatal(err)
	}
	orphanIntent := writeIntent(now-1, orphanName)

	// A due intent for an accounted-for blob loses only the intent.
	staleIntent := writeIntent(now-1, committed.blobName())

	// An intent that isn't due yet is untouched, as is a malformed entry.
	futureIntent := writeIntent(now+1000, orphanName)
	if err := os.WriteFile(filepath.Join(q.cleanupDir, "0bad-name"), nil, 0644); err != nil {
		t.Fatal(err)
	}

	if err := q.sweepCleanupIntents(); err != nil {
		t.Fatal(err)
	}
	if exists(blobPath(orphanName)) {
		t.Error("orphan blob not swept")
	}
	if exists(orphanIntent) {
		t.Error("orphan intent not removed")
	}
	if exists(staleIntent) {
		t.Error("stale intent for accounted-for blob not removed")
	}
	if !exists(blobPath(committed.blobName())) {
		t.Error("accounted-for blob was swept")
	}
	if !exists(futureIntent) {
		t.Error("future intent removed early")
	}
	st.wantMetric(&st.srv.m.PutQueueOrphansSwept, 1)

	// An in-flight PUT protects its blob: metadata isn't committed yet,
	// but a due intent for its SHA must neither delete the blob nor the
	// intent (it's rechecked next sweep).
	inflight := makePending(t, q, 0, "bb02", bytes.Repeat([]byte("inflight"), 1000))
	q.enqueue(inflight)
	if err := q.writeCleanupIntent(inflight); err != nil {
		t.Fatal(err)
	}
	if err := q.copyToMain(inflight); err != nil {
		t.Fatal(err)
	}
	dueIntent := writeIntent(now-1, inflight.blobName())
	if err := q.sweepCleanupIntents(); err != nil {
		t.Fatal(err)
	}
	if !exists(blobPath(inflight.blobName())) {
		t.Fatal("in-flight blob swept out from under a pending PUT")
	}
	if !exists(dueIntent) {
		t.Error("due intent for in-flight PUT removed; want kept for recheck")
	}
	st.wantMetric(&st.srv.m.PutQueueOrphansSwept, 0)

	// After the PUT commits, the next sweep drops the now-moot intent and
	// keeps the blob.
	st.drain()
	if err := q.sweepCleanupIntents(); err != nil {
		t.Fatal(err)
	}
	if !exists(blobPath(inflight.blobName())) {
		t.Error("committed blob swept")
	}
	if exists(dueIntent) {
		t.Error("moot intent not removed after commit")
	}
}

// TestPutQueueShardedDirs checks that a copy creates its intent in the
// blob's cleanup shard subdirectory and its temp file in the blob's own
// shard directory, never in the shared top-level directories, creating
// either directory if missing, and that the sweeper handles intents in
// shard subdirectories and in the old flat layout alike.
func TestPutQueueShardedDirs(t *testing.T) {
	st := newServerTester(t)
	q := st.srv.putq
	now := st.srv.now().Unix()

	p := makePending(t, q, 0, "ff01", bytes.Repeat([]byte("sharded"), 1000))
	q.enqueue(p)
	name := p.blobName()
	shardDir := filepath.Join(st.srv.dir, name[:2])
	if err := os.RemoveAll(shardDir); err != nil {
		t.Fatal(err)
	}

	// The shard directories don't exist yet; both are created on demand.
	if err := q.writeCleanupIntent(p); err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(q.cleanupDir, name[:2], filepath.Base(p.intentPath)); p.intentPath != want {
		t.Errorf("intent path = %s; want %s", p.intentPath, want)
	}
	if _, err := os.Stat(p.intentPath); err != nil {
		t.Errorf("intent: %v", err)
	}
	if err := q.copyToMain(p); err != nil {
		t.Fatal(err)
	}
	ents, err := os.ReadDir(shardDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) != 1 || ents[0].Name() != name {
		t.Errorf("shard dir entries = %v; want just %s", ents, name)
	}

	// With the main directory's root read-only, a copy into an existing
	// shard directory still works, so the temp file isn't created in the
	// root. (Root ignores permissions, so this can't be checked as root.)
	if os.Geteuid() != 0 {
		p2 := makePending(t, q, 0, "ff02", bytes.Repeat([]byte("read-only root"), 1000))
		if err := os.MkdirAll(filepath.Dir(q.mainPath(p2)), 0750); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(st.srv.dir, 0555); err != nil {
			t.Fatal(err)
		}
		err := q.copyToMain(p2)
		os.Chmod(st.srv.dir, 0755)
		if err != nil {
			t.Errorf("copy with a read-only main directory root: %v", err)
		}
	}

	// The sweeper handles both layouts.
	orphanSum := sha256.Sum256([]byte("sharded orphan"))
	orphanName := hex.EncodeToString(orphanSum[:]) + ".lz4"
	orphanBlob := filepath.Join(st.srv.dir, orphanName[:2], orphanName)
	if err := os.MkdirAll(filepath.Dir(orphanBlob), 0750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(orphanBlob, []byte("orphan"), 0644); err != nil {
		t.Fatal(err)
	}
	writeAt := func(dir string, due int64, blob string) string {
		t.Helper()
		if err := os.MkdirAll(dir, 0750); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, fmt.Sprintf("%08x-%s", due, blob))
		if err := os.WriteFile(path, nil, 0644); err != nil {
			t.Fatal(err)
		}
		return path
	}
	sharded := writeAt(filepath.Join(q.cleanupDir, orphanName[:2]), now-1, orphanName)
	flat := writeAt(q.cleanupDir, now-1, orphanName)
	future := writeAt(filepath.Join(q.cleanupDir, orphanName[:2]), now+1000, orphanName)
	strange := filepath.Join(q.cleanupDir, "not-a-shard")
	if err := os.Mkdir(strange, 0750); err != nil {
		t.Fatal(err)
	}

	st.drain() // commits p, deleting its intent
	if err := q.sweepCleanupIntents(); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{sharded, flat, orphanBlob} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("%s not swept: %v", path, err)
		}
	}
	for _, path := range []string{future, strange, q.mainPath(p)} {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("%s: %v; want kept", path, err)
		}
	}
	if got, want := intentFiles(t, q), []string{filepath.Join(orphanName[:2], filepath.Base(future))}; !slices.Equal(got, want) {
		t.Errorf("intents left = %q; want %q", got, want)
	}
	st.wantMetric(&st.srv.m.PutQueueOrphansSwept, 1)
}

func TestIsShardName(t *testing.T) {
	for name, want := range map[string]bool{
		"00": true, "bf": true, "ff": true, "9a": true,
		"": false, "0": false, "abc": false, "BF": false, "g0": false, ".c": false,
	} {
		if got := isShardName(name); got != want {
			t.Errorf("isShardName(%q) = %v; want %v", name, got, want)
		}
	}
}

func TestParseCleanupIntent(t *testing.T) {
	sha := strings.Repeat("ab", 32)
	tests := []struct {
		name     string
		wantDue  int64
		wantBlob string
		wantOK   bool
	}{
		{"000004d1-" + sha, 1233, sha, true},
		{"000004d1-" + sha + ".lz4", 1233, sha + ".lz4", true},
		{"4d1-" + sha, 0, "", false},           // time not fixed-width
		{"000004d1-" + sha[:10], 0, "", false}, // truncated hash
		{"junk", 0, "", false},
		{"000004d1-", 0, "", false},
	}
	for _, tt := range tests {
		due, blob, ok := parseCleanupIntent(tt.name)
		if due != tt.wantDue || blob != tt.wantBlob || ok != tt.wantOK {
			t.Errorf("parseCleanupIntent(%q) = (%v, %q, %v), want (%v, %q, %v)",
				tt.name, due, blob, ok, tt.wantDue, tt.wantBlob, tt.wantOK)
		}
	}
}

func TestPutBodyReadDeadline(t *testing.T) {
	now := time.Unix(1000, 0)
	tests := []struct {
		size int64
		want time.Duration
	}{
		{0, putUploadGrace},
		{1, putUploadGrace}, // sub-second remainders ride on the grace
		{200 << 20, putUploadGrace + 200*time.Second},
		{1 << 62, putMaxBodyReadTime}, // absurd sizes are capped, not overflowed
	}
	for _, tt := range tests {
		if got := putBodyReadDeadline(now, tt.size).Sub(now); got != tt.want {
			t.Errorf("size %d: deadline = now+%v, want now+%v", tt.size, got, tt.want)
		}
	}
}

func TestPutQueueGetWhilePending(t *testing.T) {
	st := newServerTester(t)
	c := st.mkClient()
	ctx := context.Background()

	smallVal := "small val"
	bigVal := strings.Repeat("big value ", 500) // well over smallObjectSize
	if _, err := c.Put(ctx, "aa01", "9901", int64(len(smallVal)), strings.NewReader(smallVal)); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Put(ctx, "bb02", "9902", int64(len(bigVal)), strings.NewReader(bigVal)); err != nil {
		t.Fatal(err)
	}
	if n, _ := st.srv.putq.pendingStats(); n != 2 {
		t.Fatalf("pending entries = %d, want 2", n)
	}
	var n int
	if err := st.srv.db.QueryRow("SELECT COUNT(*) FROM Actions").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("Actions rows before drain = %d, want 0", n)
	}

	// Both objects are readable before their metadata reaches SQLite.
	st.wantGet(c, "aa01", "9901", smallVal)
	st.wantGet(c, "bb02", "9902", bigVal)

	// A duplicate PUT of a still-pending action is detected synchronously;
	// the first PUT wins.
	if _, err := c.Put(ctx, "aa01", "9901", int64(len(smallVal)), strings.NewReader(smallVal)); err != nil {
		t.Fatal(err)
	}
	st.wantMetric(&st.srv.m.PutsDup, 1)
	if n, _ := st.srv.putq.pendingStats(); n != 2 {
		t.Fatalf("pending entries after dup = %d, want 2", n)
	}

	// Still readable after the pipeline settles, now from SQLite and disk.
	st.drain()
	if n, _ := st.srv.putq.pendingStats(); n != 0 {
		t.Fatalf("pending entries after drain = %d, want 0", n)
	}
	st.wantGet(c, "aa01", "9901", smallVal)
	st.wantGet(c, "bb02", "9902", bigVal)
}

// benchmarkPut measures the end-to-end server cost of PUTs of the given
// size: the request path (hash + spool + enqueue) plus the amortized
// background pipeline, which the benchmark drains synchronously every 4096
// PUTs (also required for progress: the reservation count cap would
// otherwise block once 8192 PUTs are pending with background loops off).
func benchmarkPut(b *testing.B, size int) {
	st := newServerTester(b, WithVerbose(false))
	srv := st.srv
	val := bytes.Repeat([]byte("x"), size)
	b.SetBytes(int64(size))

	i := 0
	for b.Loop() {
		i++
		actionID := fmt.Sprintf("%08x", i)
		req := httptest.NewRequest("PUT", "/"+actionID+"/"+actionID, bytes.NewReader(val))
		w := httptest.NewRecorder()
		srv.ServeHTTP(w, req)
		if w.Code != http.StatusNoContent {
			b.Fatalf("PUT status = %d, want 204", w.Code)
		}
		if i%4096 == 0 {
			if err := srv.drainPendingPuts(); err != nil {
				b.Fatal(err)
			}
		}
	}
	b.StopTimer()
	if err := srv.drainPendingPuts(); err != nil {
		b.Fatal(err)
	}
}

func BenchmarkPutInline(b *testing.B) { benchmarkPut(b, 100) }
func BenchmarkPutDisk(b *testing.B)   { benchmarkPut(b, 8<<10) }

func TestPutQueueStartupCleanup(t *testing.T) {
	t.Run("hot", func(t *testing.T) {
		hotDir := t.TempDir()
		leftover := filepath.Join(hotDir, putQueueDirName, "put-1-old")
		if err := os.MkdirAll(filepath.Dir(leftover), 0750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(leftover, []byte("orphan"), 0644); err != nil {
			t.Fatal(err)
		}

		st := newServerTester(t, WithHotDir(hotDir), WithHotCapacity(1<<20))

		if _, err := os.Stat(leftover); !os.IsNotExist(err) {
			t.Errorf("leftover spool file still exists (err=%v)", err)
		}
		if got, want := st.srv.putq.dir, filepath.Join(hotDir, putQueueDirName); got != want {
			t.Errorf("queue dir = %q, want %q", got, want)
		}
		if fi, err := os.Stat(st.srv.putq.dir); err != nil || !fi.IsDir() {
			t.Errorf("queue dir missing after start: %v", err)
		}
	})

	t.Run("no-hot", func(t *testing.T) {
		st := newServerTester(t)
		if got, want := st.srv.putq.dir, filepath.Join(st.srv.dir, putQueueDirName); got != want {
			t.Errorf("queue dir = %q, want %q", got, want)
		}
		if fi, err := os.Stat(st.srv.putq.dir); err != nil || !fi.IsDir() {
			t.Errorf("queue dir missing after start: %v", err)
		}
	})
}
