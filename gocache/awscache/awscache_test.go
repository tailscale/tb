// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package awscache

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pierrec/lz4/v4"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/tailscale/tb/gocache/cachers"
)

// fakeMeta is an in-memory metaStore with DynamoDB's conditional semantics.
type fakeMeta struct {
	mu      sync.Mutex
	items   map[string]actionEntry
	gets    int
	exists  int
	bumps   int
	failGet error // if non-nil, GetAction fails with it
}

func newFakeMeta() *fakeMeta { return &fakeMeta{items: map[string]actionEntry{}} }

func (f *fakeMeta) GetAction(ctx context.Context, key string) (*actionEntry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gets++
	if f.failGet != nil {
		return nil, f.failGet
	}
	e, ok := f.items[key]
	if !ok {
		return nil, nil
	}
	e.Data = bytes.Clone(e.Data)
	return &e, nil
}

func (f *fakeMeta) ActionExists(ctx context.Context, key string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.exists++
	_, ok := f.items[key]
	return ok, nil
}

func (f *fakeMeta) PutActionIfAbsent(ctx context.Context, key string, e *actionEntry) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.items[key]; ok {
		return false, nil
	}
	f.items[key] = *e
	return true, nil
}

func (f *fakeMeta) BumpAccessTime(ctx context.Context, key string, now, ttl, olderThan int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	e, ok := f.items[key]
	if !ok || e.AccessTime >= olderThan {
		return nil
	}
	f.bumps++
	e.AccessTime, e.TTL = now, ttl
	f.items[key] = e
	return nil
}

func (f *fakeMeta) get(key string) (actionEntry, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	e, ok := f.items[key]
	return e, ok
}

func (f *fakeMeta) set(key string, e actionEntry) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.items[key] = e
}

// fakeBlobs is an in-memory blobStore.
type fakeBlobs struct {
	mu    sync.Mutex
	blobs map[string][]byte
	puts  int
	heads int
	gets  int
}

func newFakeBlobs() *fakeBlobs { return &fakeBlobs{blobs: map[string][]byte{}} }

func (f *fakeBlobs) Exists(ctx context.Context, key string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.heads++
	_, ok := f.blobs[key]
	return ok, nil
}

func (f *fakeBlobs) Put(ctx context.Context, key string, r io.ReadSeeker, size int64) error {
	b, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	if int64(len(b)) != size {
		return fmt.Errorf("got %d bytes; want %d", len(b), size)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.puts++
	if _, ok := f.blobs[key]; !ok {
		f.blobs[key] = b
	}
	return nil
}

func (f *fakeBlobs) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gets++
	b, ok := f.blobs[key]
	if !ok {
		return nil, errBlobNotFound
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}

func (f *fakeBlobs) counts() (puts, heads, gets int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.puts, f.heads, f.gets
}

// testClock is a settable clock.
type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *testClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

type testEnv struct {
	t     *testing.T
	meta  *fakeMeta
	blobs *fakeBlobs
	clock *testClock
}

func newTestEnv(t *testing.T) *testEnv {
	return &testEnv{
		t:     t,
		meta:  newFakeMeta(),
		blobs: newFakeBlobs(),
		clock: &testClock{t: time.Unix(1_800_000_000, 0)},
	}
}

// newCache returns a Cache using env's stores, as if it were another host
// sharing the same table and bucket.
func (env *testEnv) newCache(modify func(*Config)) *Cache {
	t := env.t
	cfg := Config{
		BlobPrefix:    "pfx/",
		InlineMax:     100,
		MaxObjectSize: 10 << 20,
		SpoolDir:      t.TempDir(),
		ATimeRate:     1e6,
		Logf:          t.Logf,
		meta:          env.meta,
		blobs:         env.blobs,
		now:           env.clock.Now,
	}
	if modify != nil {
		modify(&cfg)
	}
	c, err := New(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close(context.Background()) })
	return c
}

// client is a cigocacher-like client of a Handler.
type client struct {
	t  *testing.T
	h  *Handler
	hc *cachers.HTTPClient
}

func (env *testEnv) newClient(c *Cache, g Grant) *client {
	h := c.NewHandler(func(*http.Request) Grant { return g })
	ts := httptest.NewServer(h)
	env.t.Cleanup(ts.Close)
	return &client{
		t: env.t,
		h: h,
		hc: &cachers.HTTPClient{
			BaseURL: ts.URL,
			Disk:    &cachers.DiskCache{Dir: env.t.TempDir()},
		},
	}
}

// put stores data as actionID's output via HTTP.
func (cl *client) put(actionID, outputID string, data []byte) {
	cl.t.Helper()
	if _, err := cl.hc.Put(context.Background(), actionID, outputID, int64(len(data)), bytes.NewReader(data)); err != nil {
		cl.t.Fatalf("Put(%s): %v", actionID, err)
	}
}

// get fetches actionID via HTTP, bypassing the client's disk cache, and
// returns its output ID and data, or empty strings on a miss.
func (cl *client) get(actionID string) (outputID string, data []byte) {
	cl.t.Helper()
	cl.hc.Disk = &cachers.DiskCache{Dir: cl.t.TempDir()}
	outputID, diskPath, err := cl.hc.Get(context.Background(), actionID)
	if err != nil {
		cl.t.Fatalf("Get(%s): %v", actionID, err)
	}
	if outputID == "" {
		return "", nil
	}
	data, err = os.ReadFile(diskPath)
	if err != nil {
		cl.t.Fatal(err)
	}
	return outputID, data
}

func (cl *client) wantHit(actionID, wantOutputID string, want []byte) {
	cl.t.Helper()
	gotOut, got := cl.get(actionID)
	if gotOut != wantOutputID || !bytes.Equal(got, want) {
		cl.t.Fatalf("Get(%s) = %q, %d bytes; want %q, %d bytes", actionID, gotOut, len(got), wantOutputID, len(want))
	}
}

func (cl *client) wantMiss(actionID string) {
	cl.t.Helper()
	if gotOut, _ := cl.get(actionID); gotOut != "" {
		cl.t.Fatalf("Get(%s) = hit with output %q; want miss", actionID, gotOut)
	}
}

func randBytes(n int) []byte {
	b := make([]byte, n)
	rand.Read(b)
	return b
}

var writeGlobal = Grant{CanWrite: true}

func TestRoundTrip(t *testing.T) {
	env := newTestEnv(t)
	writer := env.newClient(env.newCache(nil), writeGlobal)
	reader := env.newClient(env.newCache(nil), Grant{}) // another host

	objs := map[string][]byte{
		"aaaa01": nil,                                       // empty, inline
		"aaaa02": []byte("small"),                           // inline
		"aaaa03": bytes.Repeat([]byte("compressible"), 1e4), // blob
		"aaaa04": randBytes(3 << 20),                        // incompressible blob
	}
	for id, data := range objs {
		writer.put(id, "0b"+id, data)
	}
	for id, data := range objs {
		reader.wantHit(id, "0b"+id, data)
	}
	reader.wantMiss("bbbb01")

	if e, _ := env.meta.get(actionKey(globalStorageNS(0), "aaaa02")); !e.Inline || string(e.Data) != "small" {
		t.Errorf("small object entry = %+v; want inline", e)
	}
	e, _ := env.meta.get(actionKey(globalStorageNS(0), "aaaa03"))
	if e.Inline || e.StoredSize >= e.Size || len(e.SHA256) != 64 {
		t.Errorf("compressible object entry = %+v; want compressed blob", e)
	}
	if want := env.clock.Now().Add(30 * 24 * time.Hour).Unix(); e.TTL != want {
		t.Errorf("TTL = %v; want %v", e.TTL, want)
	}
	if puts, _, _ := env.blobs.counts(); puts != 2 {
		t.Errorf("blob puts = %d; want 2", puts)
	}

	st := reader.h.Stats()
	if st.Gets != 5 || st.GetHits != 4 || st.GetHitsInline != 2 || st.GetErrs != 0 {
		t.Errorf("reader stats = %+v", st)
	}
	st = writer.h.Stats()
	if st.Puts != 4 || st.PutsInline != 2 || st.PutErrs != 0 || st.PutsDup != 0 {
		t.Errorf("writer stats = %+v", st)
	}
}

func TestNamespaces(t *testing.T) {
	env := newTestEnv(t)
	c := env.newCache(nil)
	global := env.newClient(c, writeGlobal)
	alice := env.newClient(c, Grant{CanWrite: true, WriteNamespace: "alice", ExtraReadNamespace: "alice"})
	bob := env.newClient(c, Grant{CanWrite: true, WriteNamespace: "bob", ExtraReadNamespace: "bob"})
	anon := env.newClient(c, Grant{})

	big := randBytes(1000)
	alice.put("cccc01", "0a01", big)
	alice.wantHit("cccc01", "0a01", big)
	bob.wantMiss("cccc01")
	anon.wantMiss("cccc01")
	global.wantMiss("cccc01")

	// Global wins over the extra read namespace.
	global.put("cccc01", "0b01", []byte("global"))
	alice.wantHit("cccc01", "0b01", []byte("global"))
	bob.wantHit("cccc01", "0b01", []byte("global"))

	// Same content under two namespaces shares one blob.
	bob.put("cccc02", "0c02", big)
	bob.wantHit("cccc02", "0c02", big)
	if puts, _, _ := env.blobs.counts(); puts != 1 {
		t.Errorf("blob puts = %d; want 1", puts)
	}

	// Bumping the global generation starts global afresh.
	c2 := env.newCache(func(cfg *Config) { cfg.GlobalGeneration = 2 })
	env.newClient(c2, Grant{}).wantMiss("cccc01")
	env.newClient(c2, Grant{ExtraReadNamespace: "alice"}).wantHit("cccc01", "0a01", big)
}

func TestReadOnly(t *testing.T) {
	env := newTestEnv(t)
	c := env.newCache(nil)
	cl := env.newClient(c, Grant{ExtraReadNamespace: "alice"})
	cl.hc.Put(context.Background(), "dddd01", "0d01", 3, strings.NewReader("abc"))
	cl.wantMiss("dddd01")
	if len(env.meta.items) != 0 {
		t.Errorf("read-only PUT stored %d items", len(env.meta.items))
	}
	if st := cl.h.Stats(); st.PutErrs != 1 {
		t.Errorf("PutErrs = %d; want 1", st.PutErrs)
	}
	if err := c.Put(context.Background(), Grant{}, "dddd01", "0d01", 3, strings.NewReader("abc")); err != ErrReadOnly {
		t.Errorf("Put = %v; want ErrReadOnly", err)
	}
	if err := c.Put(context.Background(), Grant{CanWrite: true, WriteNamespace: "no#hash"}, "dddd01", "0d01", 3, strings.NewReader("abc")); err == nil {
		t.Error("Put with bad namespace succeeded")
	}
}

func TestFirstPutWinsAndDedup(t *testing.T) {
	env := newTestEnv(t)
	c := env.newCache(nil)
	cl := env.newClient(c, writeGlobal)

	data := randBytes(5000)
	cl.put("eeee01", "0e01", data)
	cl.put("eeee01", "0e02", randBytes(5000))
	cl.wantHit("eeee01", "0e01", data)
	if st := cl.h.Stats(); st.PutsDup != 1 {
		t.Errorf("PutsDup = %d; want 1", st.PutsDup)
	}

	// The same content under another action needs no HEAD or upload: this
	// process knows it's there.
	puts0, heads0, _ := env.blobs.counts()
	cl.put("eeee02", "0e01", data)
	puts1, heads1, _ := env.blobs.counts()
	if puts1 != puts0 || heads1 != heads0 {
		t.Errorf("known blob: puts %d->%d, heads %d->%d; want no change", puts0, puts1, heads0, heads1)
	}

	// Another host HEADs and finds it.
	cl2 := env.newClient(env.newCache(nil), writeGlobal)
	cl2.put("eeee03", "0e01", data)
	puts2, heads2, _ := env.blobs.counts()
	if puts2 != puts1 || heads2 != heads1+1 {
		t.Errorf("other host: puts %d->%d, heads %d->%d; want one HEAD, no PUT", puts1, puts2, heads1, heads2)
	}
	cl2.wantHit("eeee03", "0e01", data)
}

func TestBadBlobs(t *testing.T) {
	env := newTestEnv(t)
	c := env.newCache(nil)
	cl := env.newClient(c, writeGlobal)
	data := bytes.Repeat([]byte("x"), 1000)
	cl.put("ffff01", "0f01", data)
	cl.put("ffff02", "0f02", randBytes(1000))
	cl.put("ffff03", "0f03", randBytes(1000))

	// Replace ffff01's blob with a valid lz4 frame of other content.
	e1, _ := env.meta.get(actionKey(c.globalNS, "ffff01"))
	var zb bytes.Buffer
	zw := lz4.NewWriter(&zb)
	zw.Write(bytes.Repeat([]byte("y"), 1000))
	zw.Close()
	env.blobs.mu.Lock()
	env.blobs.blobs[blobKey("pfx/", e1.SHA256)] = zb.Bytes()
	env.blobs.mu.Unlock()
	e1.StoredSize = int64(zb.Len())
	env.meta.set(actionKey(c.globalNS, "ffff01"), e1)

	// Truncate ffff02's blob.
	e2, _ := env.meta.get(actionKey(c.globalNS, "ffff02"))
	env.blobs.mu.Lock()
	bk := blobKey("pfx/", e2.SHA256)
	env.blobs.blobs[bk] = env.blobs.blobs[bk][:10]

	// Delete ffff03's blob, as the scrubber might.
	e3, _ := env.meta.get(actionKey(c.globalNS, "ffff03"))
	delete(env.blobs.blobs, blobKey("pfx/", e3.SHA256))
	env.blobs.mu.Unlock()

	cl.wantMiss("ffff01")
	cl.wantMiss("ffff02")
	cl.wantMiss("ffff03")
	if got := testutil.ToFloat64(c.m.corruptBlobs); got != 2 {
		t.Errorf("corrupt blobs = %v; want 2", got)
	}
	if got := testutil.ToFloat64(c.m.danglingActions); got != 1 {
		t.Errorf("dangling actions = %v; want 1", got)
	}
}

func TestMetadataError(t *testing.T) {
	env := newTestEnv(t)
	c := env.newCache(nil)
	h := c.NewHandler(func(*http.Request) Grant { return Grant{} })
	env.meta.failGet = fmt.Errorf("throttled")
	req := httptest.NewRequest("GET", "/action/abcd", nil)
	req.Header.Set("Want-Object", "1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 500 {
		t.Errorf("status = %d; want 500", rec.Code)
	}
	if st := h.Stats(); st.GetErrs != 1 {
		t.Errorf("GetErrs = %d; want 1", st.GetErrs)
	}
}

func TestDiskSpool(t *testing.T) {
	env := newTestEnv(t)
	c := env.newCache(func(cfg *Config) { cfg.MemBudget = 1 })
	cl := env.newClient(c, writeGlobal)
	data := randBytes(1 << 20)
	cl.put("abab01", "0a01", data)
	cl.wantHit("abab01", "0a01", data)
	ents, err := os.ReadDir(c.cfg.SpoolDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) != 0 {
		t.Errorf("spool dir has %d leftover files", len(ents))
	}
	if got := testutil.ToFloat64(c.m.spoolBytes.WithLabelValues("disk")); got != 0 {
		t.Errorf("disk spool reserved = %v after requests; want 0", got)
	}
}

func TestNoLZ4(t *testing.T) {
	env := newTestEnv(t)
	c := env.newCache(nil)
	cl := env.newClient(c, writeGlobal)
	data := bytes.Repeat([]byte("hello"), 1000)
	cl.put("acac01", "0a01", data)

	h := cl.h
	for _, method := range []string{"GET", "HEAD"} {
		req := httptest.NewRequest(method, "/action/acac01", nil)
		req.Header.Set("Want-Object", "1")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != 200 || rec.Header().Get("Content-Encoding") != "" || rec.Header().Get("Go-Output-Id") != "0a01" ||
			rec.Header().Get("Content-Length") != fmt.Sprint(len(data)) {
			t.Fatalf("%s: %d %v", method, rec.Code, rec.Header())
		}
		if method == "GET" && !bytes.Equal(rec.Body.Bytes(), data) {
			t.Errorf("GET body mismatch")
		}
	}
}

func TestAccessTimeBump(t *testing.T) {
	env := newTestEnv(t)
	c := env.newCache(nil)
	cl := env.newClient(c, writeGlobal)
	cl.put("adad01", "0a01", []byte("x"))
	key := actionKey(c.globalNS, "adad01")

	// A fresh entry isn't bumped.
	cl.wantHit("adad01", "0a01", []byte("x"))
	if st := cl.h.Stats(); st.GetAccessBumps != 0 {
		t.Fatalf("GetAccessBumps = %d; want 0", st.GetAccessBumps)
	}

	env.clock.Advance(48 * time.Hour)
	cl.wantHit("adad01", "0a01", []byte("x"))
	if st := cl.h.Stats(); st.GetAccessBumps != 1 {
		t.Fatalf("GetAccessBumps = %d; want 1", st.GetAccessBumps)
	}
	c.Close(context.Background()) // waits for queued bumps
	e, _ := env.meta.get(key)
	now := env.clock.Now()
	if e.AccessTime != now.Unix() || e.TTL != now.Add(30*24*time.Hour).Unix() {
		t.Errorf("after bump: at=%v ttl=%v; want %v, %v", e.AccessTime, e.TTL, now.Unix(), now.Add(30*24*time.Hour).Unix())
	}
}

func TestSessionStats(t *testing.T) {
	env := newTestEnv(t)
	cl := env.newClient(env.newCache(nil), writeGlobal)
	cl.put("aeae01", "0a01", []byte("x"))
	cl.wantHit("aeae01", "0a01", []byte("x"))

	res, err := http.Get(cl.hc.BaseURL + "/session/stats")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var got map[string]any
	if err := json.NewDecoder(res.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got["Gets"] != 1.0 || got["GetHits"] != 1.0 || got["Puts"] != 1.0 {
		t.Errorf("stats = %v", got)
	}
}

func TestTooLarge(t *testing.T) {
	env := newTestEnv(t)
	c := env.newCache(func(cfg *Config) { cfg.MaxObjectSize = 1000 })
	if err := c.Put(context.Background(), writeGlobal, "afaf01", "0a01", 1001, bytes.NewReader(make([]byte, 1001))); err != ErrTooLarge {
		t.Errorf("Put = %v; want ErrTooLarge", err)
	}
}

func TestShortBody(t *testing.T) {
	env := newTestEnv(t)
	c := env.newCache(nil)
	for _, size := range []int64{50, 5000} { // inline and blob
		if err := c.Put(context.Background(), writeGlobal, "baba01", "0a01", size, bytes.NewReader(make([]byte, size-1))); err == nil {
			t.Errorf("Put of short %d-byte body succeeded", size)
		}
	}
	if len(env.meta.items) != 0 {
		t.Errorf("short PUTs stored %d items", len(env.meta.items))
	}
}

func TestValidateNamespace(t *testing.T) {
	for _, ns := range []Namespace{"", "alice", "corp.ts.net/user:alice@tailscale.com", "corp.ts.net/tag:binbucket", "dependabot[bot]"} {
		if err := ValidateNamespace(ns); err != nil {
			t.Errorf("ValidateNamespace(%q) = %v", ns, err)
		}
	}
	for _, ns := range []Namespace{"a#b", "!global:1", "a b", Namespace(strings.Repeat("a", 1025))} {
		if ValidateNamespace(ns) == nil {
			t.Errorf("ValidateNamespace(%q) = nil; want error", ns)
		}
	}
}
