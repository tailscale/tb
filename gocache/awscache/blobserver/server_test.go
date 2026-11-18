// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package blobserver

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/pierrec/lz4/v4"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"
	"github.com/tailscale/tb/gocache/awscache"
)

// blob is a test blob: lz4-compressed content and the SHA-256 hex of the
// content.
type blob struct {
	sha string
	lz4 []byte
}

func (b blob) size() int64 { return int64(len(b.lz4)) }

func randBytes(n int) []byte {
	b := make([]byte, n)
	rand.Read(b)
	return b
}

func newBlob(t testing.TB, content []byte) blob {
	t.Helper()
	var buf bytes.Buffer
	w := lz4.NewWriter(&buf)
	if _, err := w.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	h := sha256.Sum256(content)
	return blob{
		sha: hex.EncodeToString(h[:]),
		lz4: buf.Bytes(),
	}
}

// fakeS3 is an in-memory S3 for a Server.
type fakeS3 struct {
	mu    sync.Mutex
	blobs map[string][]byte
	gets  int

	// gate, if non-nil, stops reads of the blob gateSHA, or of all blobs if
	// gateSHA is empty, until the test closes it. Then the reads return
	// gateErr, if it isn't nil.
	gate    chan struct{}
	gateSHA string
	gateErr error
}

func newFakeS3(blobs ...blob) *fakeS3 {
	f := &fakeS3{
		blobs: make(map[string][]byte),
	}
	for _, b := range blobs {
		f.blobs[b.sha] = b.lz4
	}
	return f
}

func (f *fakeS3) Get(ctx context.Context, sha string) (io.ReadCloser, int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gets++
	b, ok := f.blobs[sha]
	if !ok {
		return nil, 0, awscache.ErrBlobNotFound
	}
	var r io.Reader = bytes.NewReader(b)
	if f.gate != nil && (f.gateSHA == "" || sha == f.gateSHA) {
		r = &gatedReader{r: r, gate: f.gate, err: f.gateErr}
	}
	return io.NopCloser(r), int64(len(b)), nil
}

func (f *fakeS3) getCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.gets
}

type gatedReader struct {
	r    io.Reader
	gate chan struct{}
	err  error
}

func (r *gatedReader) Read(p []byte) (int, error) {
	<-r.gate
	if r.err != nil {
		return 0, r.err
	}
	return r.r.Read(p)
}

// newTestServer makes a Server that gets blobs from s3 and keeps them in a new
// temporary dir. It must be called in a synctest bubble.
func newTestServer(t *testing.T, s3 *fakeS3, maxBytes int64, opts ...Option) *Server {
	t.Helper()
	opts = append([]Option{WithLogf(t.Logf), withGetter(s3.Get)}, opts...)
	s, err := New(context.Background(), "", t.TempDir(), maxBytes, opts...)
	if err != nil {
		t.Fatal(err)
	}
	checkLoaded(t, s)
	return s
}

// checkLoaded waits for s to find the blobs on disk. It must be called in a
// synctest bubble.
func checkLoaded(t *testing.T, s *Server) {
	t.Helper()
	synctest.Wait()
	select {
	case <-s.loaded:
	default:
		t.Fatal("server didn't find the blobs on disk")
	}
}

func getBlob(s *Server, sha string) (int, []byte) {
	code, _, body := serveBlob(s, "GET", sha)
	return code, body
}

// fillBlob sends a GET of the blob sha, which s doesn't have, and waits for
// the fill that it starts to end. It must be called in a synctest bubble.
func fillBlob(t *testing.T, s *Server, sha string) {
	t.Helper()
	if code, _ := getBlob(s, sha); code != http.StatusNotFound {
		t.Fatalf("GET of uncached blob = %d; want 404", code)
	}
	synctest.Wait()
}

func serveBlob(s *Server, method, sha string) (int, http.Header, []byte) {
	return serveRequest(s, httptest.NewRequest(method, "/blob/"+sha+".lz4", nil))
}

// getBlobAsync starts a GET of the blob sha in a new goroutine. It returns a
// channel that receives the status code and body of the response.
func getBlobAsync(s *Server, sha string) <-chan []byte {
	ch := make(chan []byte, 1)
	go func() {
		code, body := getBlob(s, sha)
		if code != 200 {
			body = nil
		}
		ch <- body
	}()
	return ch
}

// serveRequest sends req to s. It returns the status code 0 if s aborts the
// response.
func serveRequest(s *Server, req *http.Request) (code int, _ http.Header, _ []byte) {
	defer func() {
		if e := recover(); e != nil && e != http.ErrAbortHandler {
			panic(e)
		}
	}()
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	res := rec.Result()
	body, _ := io.ReadAll(res.Body)
	return res.StatusCode, res.Header, body
}

func fills(s *Server, result string) float64 {
	return sampleCount(s.m.fills.WithLabelValues(result))
}

// sampleCount returns the number of observations of the histogram h.
func sampleCount(h prometheus.Observer) float64 {
	var m dto.Metric
	h.(prometheus.Metric).Write(&m)
	return float64(m.GetHistogram().GetSampleCount())
}

func TestServer(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		b1 := newBlob(t, randBytes(1000))
		b2 := newBlob(t, randBytes(1000))
		b3 := newBlob(t, randBytes(1000))
		s3 := newFakeS3(b1, b2, b3)
		s := newTestServer(t, s3, b1.size()+b2.size())

		// A miss gets a 404 at once, and the server fills the blob in
		// the background.
		fillBlob(t, s, b1.sha)
		for range 2 {
			code, body := getBlob(s, b1.sha)
			if code != 200 || !bytes.Equal(body, b1.lz4) {
				t.Fatalf("GET = %d, %d bytes; want 200, %d bytes", code, len(body), b1.size())
			}
		}
		if gets := s3.getCount(); gets != 1 {
			t.Errorf("S3 gets = %d; want 1", gets)
		}

		fillBlob(t, s, b2.sha)
		getBlob(s, b1.sha)
		fillBlob(t, s, b3.sha)
		if _, err := os.Stat(s.path(b2.sha)); !os.IsNotExist(err) {
			t.Errorf("least recently used blob not evicted: %v", err)
		}
		if got := sampleCount(s.m.evictions); got != 1 {
			t.Errorf("evictions = %v; want 1", got)
		}
		if got := fills(s, "stored"); got != 3 {
			t.Errorf("stored fills = %v; want 3", got)
		}

		// If the file of a cached blob is missing, the next GET is a miss,
		// and the fill stores the blob again. A POST also stores it again.
		if err := os.Remove(s.path(b1.sha)); err != nil {
			t.Fatal(err)
		}
		fillBlob(t, s, b1.sha)
		if code, _ := getBlob(s, b1.sha); code != 200 {
			t.Errorf("GET after a fill of a missing file = %d; want 200", code)
		}
		if err := os.Remove(s.path(b1.sha)); err != nil {
			t.Fatal(err)
		}
		if code, _, _ := serveBlob(s, "POST", b1.sha); code != http.StatusAccepted {
			t.Errorf("POST of a missing file = %d; want 202", code)
		}
		synctest.Wait()
		if _, err := os.Stat(s.path(b1.sha)); err != nil {
			t.Errorf("POST didn't store the blob again: %v", err)
		}
		if got, want := int64(testutil.ToFloat64(s.m.diskBytes)), b1.size()+b3.size(); got != want {
			t.Errorf("disk bytes = %d; want %d", got, want)
		}

		fillBlob(t, s, strings.Repeat("0", 64))
		if got := fills(s, "notfound"); got != 1 {
			t.Errorf("notfound fills = %v; want 1", got)
		}
		gets0 := s3.getCount()
		for _, path := range []string{
			"/blob/nothex.lz4",
			"/blob/" + b1.sha,
			"/blob/" + b1.sha + ".gz",
			"/blob/" + strings.ToUpper(b1.sha) + ".lz4",
			"/blob/" + b1.sha[2:] + ".lz4",
			"/blob/" + b1.sha[:2] + "/" + b1.sha + ".lz4",
			"/blob/pfx/" + b1.sha[:2] + "/" + b1.sha + ".lz4",
			"/" + b1.sha + ".lz4",
			"/blobs/" + b1.sha + ".lz4",
		} {
			if code, _, _ := serveRequest(s, httptest.NewRequest("GET", path, nil)); code != http.StatusNotFound {
				t.Errorf("GET %s = %d; want 404", path, code)
			}
		}
		synctest.Wait()
		if gets := s3.getCount(); gets != gets0 {
			t.Errorf("S3 gets for bad paths = %d; want 0", gets-gets0)
		}
	})
}

func TestServerHEAD(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		b := newBlob(t, randBytes(1000))
		s3 := newFakeS3(b)
		s := newTestServer(t, s3, 1<<20)
		result := func(r string) float64 { return testutil.ToFloat64(s.m.requests.WithLabelValues("HEAD", r)) }

		if code, _, _ := serveBlob(s, "HEAD", b.sha); code != http.StatusNotFound {
			t.Fatalf("HEAD of uncached blob = %d; want 404", code)
		}
		synctest.Wait()
		if gets := s3.getCount(); gets != 0 {
			t.Errorf("HEAD of uncached blob made %d S3 gets; want 0", gets)
		}
		if result("miss") != 1 {
			t.Errorf("misses = %v; want 1", result("miss"))
		}

		fillBlob(t, s, b.sha)
		code, h, body := serveBlob(s, "HEAD", b.sha)
		if code != 200 || len(body) != 0 {
			t.Fatalf("HEAD of cached blob = %d, %d bytes; want 200, 0 bytes", code, len(body))
		}
		if got, want := h.Get("Content-Length"), strconv.FormatInt(b.size(), 10); got != want {
			t.Errorf("HEAD Content-Length = %q; want %q", got, want)
		}
		if result("hit") != 1 {
			t.Errorf("hits = %v; want 1", result("hit"))
		}

		if code, _, _ := serveBlob(s, "PUT", b.sha); code != http.StatusMethodNotAllowed {
			t.Errorf("PUT = %d; want 405", code)
		}
	})
}

func TestServerTooLarge(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		b := newBlob(t, randBytes(1000))
		s3 := newFakeS3(b)
		s := newTestServer(t, s3, b.size()-1)
		fillBlob(t, s, b.sha)
		if got := fills(s, "toolarge"); got != 1 {
			t.Errorf("toolarge fills = %v; want 1", got)
		}
		if got := testutil.ToFloat64(s.m.diskBytes); got != 0 {
			t.Errorf("disk bytes = %v; want 0", got)
		}
		if _, err := os.Stat(s.path(b.sha)); !os.IsNotExist(err) {
			t.Errorf("blob stored: %v", err)
		}

		// A blob that is exactly maxBytes fits.
		s = newTestServer(t, s3, b.size())
		fillBlob(t, s, b.sha)
		if code, _ := getBlob(s, b.sha); code != 200 {
			t.Fatalf("GET of blob of maxBytes = %d; want 200", code)
		}
	})
}

func TestServerFillReservesSpace(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		b1 := newBlob(t, randBytes(1000))
		b2 := newBlob(t, randBytes(1000))
		b3 := newBlob(t, randBytes(1000))
		b4 := newBlob(t, randBytes(1100))
		s3 := newFakeS3(b1, b2, b3, b4)
		s := newTestServer(t, s3, b1.size()+b2.size())
		diskBytes := func() int64 { return int64(testutil.ToFloat64(s.m.diskBytes)) }
		reserved := func() int64 { return int64(testutil.ToFloat64(s.m.reservedBytes)) }

		fillBlob(t, s, b1.sha)
		fillBlob(t, s, b2.sha)
		s3.gate, s3.gateSHA = make(chan struct{}), b3.sha
		fillBlob(t, s, b3.sha)

		// The fill of b3 reserves its space before it reads the blob.
		// This evicts b1, which is the least recently used blob.
		if _, err := os.Stat(s.path(b1.sha)); !os.IsNotExist(err) {
			t.Errorf("b1 not evicted for the fill of b3: %v", err)
		}
		if diskBytes() != b2.size() || reserved() != b3.size() {
			t.Errorf("disk, reserved bytes during fill = %d, %d; want %d, %d", diskBytes(), reserved(), b2.size(), b3.size())
		}

		// b3 holds its reservation, so the fills of b3 and b4 need more
		// space than the limit. The fill of b4 evicts b2, and it stores
		// b4. The server is then above the limit, so it evicts b4, which
		// is the only cached blob.
		fillBlob(t, s, b4.sha)
		if got := fills(s, "stored"); got != 3 {
			t.Errorf("stored fills = %v; want 3", got)
		}
		for _, b := range []blob{b2, b4} {
			if _, err := os.Stat(s.path(b.sha)); !os.IsNotExist(err) {
				t.Errorf("blob not evicted: %v", err)
			}
		}

		close(s3.gate)
		synctest.Wait()
		if code, _ := getBlob(s, b3.sha); code != 200 {
			t.Fatalf("GET of b3 = %d; want 200", code)
		}
		if diskBytes() != b3.size() || reserved() != 0 {
			t.Errorf("disk, reserved bytes after fill = %d, %d; want %d, 0", diskBytes(), reserved(), b3.size())
		}
	})
}

func TestNew(t *testing.T) {
	s3 := newFakeS3()
	for _, maxBytes := range []int64{0, -1} {
		if _, err := New(context.Background(), "", t.TempDir(), maxBytes, withGetter(s3.Get)); err == nil {
			t.Errorf("New with maxBytes %d: no error", maxBytes)
		}
	}
	if _, err := New(context.Background(), "", t.TempDir(), 1<<20); err == nil {
		t.Error("New without a bucket: no error")
	}
}

// TestServerLoad checks that a new Server uses the blobs that an earlier
// Server left on disk.
func TestServerLoad(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		dir := t.TempDir()
		writeFile := func(path string, data []byte, age time.Duration) {
			t.Helper()
			mtime := time.Now().Add(-age)
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, data, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Chtimes(path, mtime, mtime); err != nil {
				t.Fatal(err)
			}
		}
		// blobs[0] is the least recently used.
		var blobs []blob
		for i := range 3 {
			b := newBlob(t, randBytes(1000))
			blobs = append(blobs, b)
			writeFile(filepath.Join(dir, b.sha[:2], b.sha+".lz4"), b.lz4, time.Duration(3-i)*2*relatime)
		}
		tmp := filepath.Join(dir, blobs[0].sha[:2], blobs[0].sha+".123.tmp")
		other := filepath.Join(dir, "other.lz4")
		writeFile(tmp, nil, 0)
		writeFile(other, nil, 0)

		s, err := New(context.Background(), "", dir, blobs[1].size()+blobs[2].size(), WithLogf(t.Logf), withGetter(newFakeS3().Get))
		if err != nil {
			t.Fatal(err)
		}
		checkLoaded(t, s)

		// The Server evicts the least recently used blob to go below its
		// limit, and it removes the temporary file of a fill.
		for _, p := range []string{s.path(blobs[0].sha), tmp} {
			if _, err := os.Stat(p); !os.IsNotExist(err) {
				t.Errorf("%s not removed: %v", p, err)
			}
		}
		if _, err := os.Stat(other); err != nil {
			t.Errorf("file that isn't a blob: %v", err)
		}
		if code, body := getBlob(s, blobs[1].sha); code != 200 || !bytes.Equal(body, blobs[1].lz4) {
			t.Fatalf("GET of blob on disk = %d, %d bytes; want 200, %d bytes", code, len(body), blobs[1].size())
		}
		// The GET makes blobs[2] the least recently used blob, and it updates
		// the old modification time of the file of blobs[1].
		if got := s.ll.Back().Value.(*blobFile).sha; got != blobs[2].sha {
			t.Errorf("least recently used blob = %s; want %s", got, blobs[2].sha)
		}
		if fi, err := os.Stat(s.path(blobs[1].sha)); err != nil || time.Since(fi.ModTime()) > relatime {
			t.Errorf("modification time of blob after GET: %v, %v; want now", fi.ModTime(), err)
		}
	})
}

func TestServerPOST(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		b1 := newBlob(t, randBytes(1000))
		b2 := newBlob(t, randBytes(1000))
		s3 := newFakeS3(b1, b2)
		s := newTestServer(t, s3, 1<<20)
		warms := func(r string) float64 { return testutil.ToFloat64(s.m.warms.WithLabelValues(r)) }
		post := func(sha string) {
			t.Helper()
			if code, _, _ := serveBlob(s, "POST", sha); code != http.StatusAccepted {
				t.Fatalf("POST = %d; want 202", code)
			}
		}

		// A POST fills the blob from S3, so the next GET is a hit.
		post(b1.sha)
		synctest.Wait()
		if code, body := getBlob(s, b1.sha); code != 200 || !bytes.Equal(body, b1.lz4) {
			t.Fatalf("GET after POST = %d, %d bytes; want 200, %d bytes", code, len(body), b1.size())
		}
		post(b1.sha)
		post(strings.Repeat("0", 64))
		synctest.Wait()
		if warms("started") != 2 || warms("cached") != 1 || fills(s, "notfound") != 1 {
			t.Errorf("started, cached, notfound = %v, %v, %v; want 2, 1, 1", warms("started"), warms("cached"), fills(s, "notfound"))
		}

		// A POST doesn't start a second fill of a blob.
		s3.gate, s3.gateSHA = make(chan struct{}), b2.sha
		post(b2.sha)
		post(b2.sha)
		if warms("filling") != 1 {
			t.Errorf("filling = %v; want 1", warms("filling"))
		}
		close(s3.gate)
		synctest.Wait()

		// A POST doesn't start a fill if maxFills fills are in progress.
		var blobs []blob
		for range maxFills + 1 {
			b := newBlob(t, randBytes(10))
			blobs = append(blobs, b)
			s3.blobs[b.sha] = b.lz4
		}
		s3.gate, s3.gateSHA = make(chan struct{}), ""
		for _, b := range blobs {
			post(b.sha)
		}
		if got := testutil.ToFloat64(s.m.fillsInProgress); got != maxFills || warms("busy") != 1 {
			t.Errorf("fills in progress, busy = %v, %v; want %d, 1", got, warms("busy"), maxFills)
		}
		close(s3.gate)
		synctest.Wait()
	})
}

func TestServerFillWaiters(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		b := newBlob(t, randBytes(100<<10))
		s3 := newFakeS3(b)
		s3.gate, s3.gateSHA = make(chan struct{}), b.sha
		s := newTestServer(t, s3, 1<<20)

		// A GET of a blob that a fill is writing waits for the fill, and
		// gets the blob without a second S3 get.
		fillBlob(t, s, b.sha)
		got := getBlobAsync(s, b.sha)
		synctest.Wait()
		close(s3.gate)
		if body := <-got; !bytes.Equal(body, b.lz4) {
			t.Errorf("GET during a fill: %d bytes; want %d", len(body), b.size())
		}
		if gets := s3.getCount(); gets != 1 {
			t.Errorf("S3 gets = %d; want 1", gets)
		}
		if got := testutil.ToFloat64(s.m.requests.WithLabelValues("GET", "filling")); got != 1 {
			t.Errorf("filling GETs = %v; want 1", got)
		}

		// If the fill fails, the server aborts the response.
		b2 := newBlob(t, randBytes(100<<10))
		s3.blobs[b2.sha] = b2.lz4
		s3.gate, s3.gateSHA, s3.gateErr = make(chan struct{}), b2.sha, errors.New("S3 failed")
		fillBlob(t, s, b2.sha)
		got = getBlobAsync(s, b2.sha)
		synctest.Wait()
		close(s3.gate)
		if body := <-got; body != nil {
			t.Errorf("GET during a failed fill: %d bytes; want an aborted response", len(body))
		}
		if got := testutil.ToFloat64(s.m.requests.WithLabelValues("GET", "filling")); got != 2 {
			t.Errorf("filling GETs = %v; want 2", got)
		}
		synctest.Wait()
		if _, err := os.Stat(s.path(b2.sha)); !os.IsNotExist(err) {
			t.Errorf("failed fill stored the blob: %v", err)
		}

		// Before the fill knows the size of the blob, a GET is a miss.
		b3 := newBlob(t, randBytes(1000))
		s3 = newFakeS3(b3)
		gate := make(chan struct{})
		s = newTestServer(t, s3, 1<<20, withGetter(func(ctx context.Context, sha string) (io.ReadCloser, int64, error) {
			<-gate
			return s3.Get(ctx, sha)
		}))
		fillBlob(t, s, b3.sha)
		if code, _ := getBlob(s, b3.sha); code != http.StatusNotFound {
			t.Errorf("GET before the fill knows the size = %d; want 404", code)
		}
		close(gate)
		synctest.Wait()
		if code, _ := getBlob(s, b3.sha); code != 200 {
			t.Errorf("GET after the fill = %d; want 200", code)
		}
	})
}
