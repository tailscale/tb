// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

// Package blobserver has a read-through disk cache of the S3 blobs of an
// [awscache.Cache].
//
// A [Server] is a latency optimization only. Clients verify every blob that
// they get from a Server, and they use S3 when a Server fails, is down, or
// doesn't have a blob.
//
// It has no access control and uses its own AWS credentials, so it bypasses
// read access control of the S3 bucket under its prefix for any client that can
// connect and provide a SHA256.
//
// A pool of Servers shares the cached blobs between them. Each client is
// configured with the same set of Server URLs ([awscache.Config.BlobServers]),
// and it uses a rendezvous hash of a blob's SHA256 to pick the one server
// that should hold that blob. The Servers don't know about each other or
// about the hash. A server caches the blobs that clients ask it for, so the
// clients' hash defines the content set of each server. If a server is added
// to or removed from the pool, only the blobs that the hash moves to a
// different server miss until the pool refills.
//
// Clients use three requests, all for /blob/<sha>.lz4:
//
//   - GET reads a blob. On a miss, the server sends 404 at once, so that
//     the client gets the blob from S3 itself. The server then gets the
//     blob from S3 in the background and stores it on disk, so that the
//     next GET of it is a hit.
//   - HEAD tells whether the server has a blob on disk. The server doesn't
//     go to S3. Before a client uploads a new blob to S3, it sends a HEAD,
//     so it doesn't upload a blob that already exists.
//   - POST tells the server that a client has just uploaded the blob to S3.
//     The server sends 202 at once, and it then fills the blob from S3 in
//     the background, so that the next GET of it is a hit.
//
// The server evicts the least recently used blobs to stay below its size
// limit.
package blobserver

import (
	"container/list"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/tailscale/tb/gocache/awscache"
	"github.com/tailscale/tb/gocache/logger"
	tslogger "tailscale.com/types/logger"
)

const (
	// fillTimeout is the maximum time of a fill from S3.
	fillTimeout = time.Minute

	// maxFills is the maximum number of fills in progress.
	//
	// The estimates below use blob sizes and request rates from production (as
	// of 2026-11-24), measured S3 latencies, and i7i.xlarge nodes:
	//
	//   - About half of the blobs that clients read are 64 KiB or less, 95%
	//     are 1 MiB or less, and the largest are 64 MiB. The mean is about
	//     370 KiB of lz4.
	//   - The mean S3 first byte is about 31 ms (p95 76 ms), and the mean
	//     transfer is about 4 ms. Thus the mean fill takes about 35 ms.
	//   - So each fill slot does about 29 fills/s, or about 11 MB/s.
	//
	// Thus the peak rate of fills is approximately:
	//
	//	maxFills  S3 GETs/s  throughput
	//	      64      1,800  0.7 GB/s (5.5 Gbps)
	//	     128      3,700  1.4 GB/s (11 Gbps)
	//	     256      7,300  2.8 GB/s (22 Gbps)
	//
	// If all fills take as long as the p95 first byte, the rates are a
	// little less than half.
	//
	// At peak, cigocached serves about 2,200 blob GETs/s and about 550 blob
	// writes/s. Each write causes a fill, so there are at most about 2,700
	// fills/s even if all GETs miss, and 128 is sufficient. It also:
	//
	//   - Is about the number of fills that the network of the node can
	//     keep busy. An i7i.xlarge bursts to 10 Gbps, or about 3,300
	//     fills/s, and its baseline is 2.3 Gbps, or about 800 fills/s. More
	//     fills only share the network, so each fill is slower, which makes
	//     GETs that wait for a fill slower, and makes large blobs exceed
	//     fillTimeout.
	//   - Keeps the connections to S3 below the MaxIdleConnsPerHost (256)
	//     that awscache sets on the AWS HTTP transport, so the fills use
	//     the same connections again and don't open new ones.
	//   - Keeps the rate of S3 GETs below the 5,500 GETs/s that one S3
	//     prefix partition supports, which decreases the risk of 503
	//     SlowDown errors. Clients also get blobs from S3 directly when
	//     they miss, so if all GETs miss, the total is about 4,900 GETs/s.
	maxFills = 128

	// defaultMaxQueued is the maximum number of queued blob fills after
	// maxFills. The cache is very bursty, so the queue is always expected to
	// drain in quiet periods, but this limit ensures it can't grow unbounded.
	defaultMaxQueued = 1_000_000

	// relatime is how old the modification time of a blob's file can be
	// before a GET of the blob updates it. A new Server uses these times to
	// find the least recently used blobs.
	relatime = time.Hour
)

var (
	errCached        = errors.New("blob is already cached")
	errFilling       = errors.New("a fill of the blob is in progress")
	errQueued        = errors.New("a fill of the blob is in the queue")
	errServerLoading = errors.New("server is finding the blobs on disk")
	errBusy          = errors.New("the queue of fills is full")
	errBlobTooLarge  = errors.New("blob is larger than the cache")
)

// Option is an optional setting for [New].
type Option func(*options)

type options struct {
	logf       logger.Logf
	awsConfig  *aws.Config
	blobPrefix string
	get        getFunc
	maxQueued  int
}

// getFunc opens the blob sha in S3 and returns its size. It returns
// [awscache.ErrBlobNotFound] if the blob isn't in S3.
type getFunc func(ctx context.Context, sha string) (io.ReadCloser, int64, error)

// WithLogf sets the logger of the Server. The default is log.Printf.
func WithLogf(logf logger.Logf) Option {
	return func(o *options) {
		o.logf = logf
	}
}

// WithAWSConfig sets the AWS config that the Server uses to get blobs from
// S3. The default is the AWS SDK's default config, with the region from EC2
// instance metadata and an HTTP transport tuned for many concurrent requests.
func WithAWSConfig(cfg *aws.Config) Option {
	return func(o *options) {
		o.awsConfig = cfg
	}
}

// WithBlobPrefix sets the prefix of the blob keys in the S3 bucket, e.g.
// "v1/". It must be the same as the [awscache.Config.BlobPrefix] of the
// clients.
//
// [awscache.Config.BlobPrefix]: https://pkg.go.dev/github.com/tailscale/tb/gocache/awscache#Config.BlobPrefix
func WithBlobPrefix(prefix string) Option {
	return func(o *options) {
		o.blobPrefix = prefix
	}
}

// withGetter replaces S3, for tests.
func withGetter(get getFunc) Option {
	return func(o *options) {
		o.get = get
	}
}

// withMaxQueued sets the maximum number of blobs in the queue of fills, for
// tests.
func withMaxQueued(n int) Option {
	return func(o *options) {
		o.maxQueued = n
	}
}

// Server is a caching HTTP server for blobs that are durably stored in S3.
// It is a latency optimization, so that blobs too big to be inlined in
// DynamoDB (> 700 bytes) can still be served at low latencies. Use [New] to
// make one.
type Server struct {
	dir       string
	maxBytes  int64
	maxQueued int
	logf      logger.Logf
	get       getFunc
	s3blobs   *awscache.S3Blobs // nil in tests
	m         *serverMetrics
	loaded    chan struct{} // closed when the blobs on disk are in ll

	mu       sync.Mutex
	ll       *list.List
	ents     map[string]*list.Element
	filling  map[string]*fill // blobs with a fill in progress
	queue    *list.List       // SHAs of blobs that wait for a fill, oldest at the front
	queued   map[string]bool  // SHAs in queue
	usage    int64
	reserved int64
}

// fill is a fill in progress.
type fill struct {
	size int64         // size of the blob after the fill reserves space for it; -1 before
	done chan struct{} // closed when the fill ends
}

type blobFile struct {
	sha  string
	size int64
	used time.Time // last GET or fill
}

// New makes a [Server] that gets blobs from bucket and keeps at most
// maxBytes of them on disk in dir. It uses the blobs that an earlier Server
// left in dir.
func New(ctx context.Context, bucket, dir string, maxBytes int64, opts ...Option) (*Server, error) {
	if dir == "" || maxBytes <= 0 {
		return nil, errors.New("blobserver: Server needs a dir and a positive maxBytes")
	}
	o := options{
		logf:      log.Printf,
		maxQueued: defaultMaxQueued,
	}
	for _, opt := range opts {
		opt(&o)
	}
	s := &Server{
		dir:       dir,
		maxBytes:  maxBytes,
		maxQueued: o.maxQueued,
		logf:      logger.Logf(tslogger.RateLimitedFn(tslogger.Logf(o.logf), time.Minute, 5, 100)),
		get:       o.get,
		loaded:    make(chan struct{}),
		ll:        list.New(),
		ents:      make(map[string]*list.Element),
		filling:   make(map[string]*fill),
		queue:     list.New(),
		queued:    make(map[string]bool),
	}
	s.m = newServerMetrics(s)
	if s.get == nil {
		r, err := awscache.NewS3Blobs(ctx, bucket, o.blobPrefix, o.awsConfig)
		if err != nil {
			return nil, err
		}
		s.s3blobs = r
		s.get = r.Get
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	go s.load()
	return s, nil
}

// load adds the blobs on disk to the LRU list, with the least recently
// modified blob at the back. It removes the temporary files of fills that
// didn't end.
func (s *Server) load() {
	defer close(s.loaded)
	var blobs []*blobFile
	filepath.WalkDir(s.dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			s.logf("blobserver: finding blobs on disk: %v", err)
			return nil
		}
		if d.IsDir() {
			return nil
		}
		name := d.Name()
		if strings.HasSuffix(name, ".tmp") {
			os.Remove(path)
			return nil
		}
		sha, ok := strings.CutSuffix(name, ".lz4")
		if !ok || !validSHA256Hex(sha) || path != s.path(sha) {
			return nil
		}
		if fi, err := d.Info(); err == nil {
			blobs = append(blobs, &blobFile{
				sha:  sha,
				size: fi.Size(),
				used: fi.ModTime(),
			})
		}
		return nil
	})
	slices.SortFunc(blobs, func(a, b *blobFile) int { return a.used.Compare(b.used) })
	s.mu.Lock()
	for _, bf := range blobs {
		s.ents[bf.sha] = s.ll.PushFront(bf)
		s.usage += bf.size
	}
	s.mu.Unlock()
	s.evict()
}

func (s *Server) path(sha string) string {
	return filepath.Join(s.dir, sha[:2], sha+".lz4")
}

// touch records a GET of the blob sha, whose file has the modification time
// mtime.
func (s *Server) touch(sha string, mtime time.Time) {
	now := time.Now()
	s.mu.Lock()
	if e, ok := s.ents[sha]; ok {
		s.ll.MoveToFront(e)
		e.Value.(*blobFile).used = now
	}
	s.mu.Unlock()
	if now.Sub(mtime) > relatime {
		// Best-effort only; an error only affects eviction order on restart.
		os.Chtimes(s.path(sha), now, now)
	}
}

// beginFill records the start of a fill of the blob sha. It returns
// errCached if the blob is on disk, errFilling if a fill of the blob is in
// progress, and errServerLoading if the server can't start a fill now. If the
// maximum number of fills are in progress, beginFill puts the blob in the
// queue of fills and returns errQueued, or it returns errBusy if the queue is
// full. If it returns a fill, the caller must call endFill.
func (s *Server) beginFill(sha string) (*fill, error) {
	select {
	case <-s.loaded:
	default:
		return nil, errServerLoading
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.canFillLocked(sha); err != nil {
		return nil, err
	}
	if s.queued[sha] {
		return nil, errQueued
	}
	if len(s.filling) >= maxFills {
		if s.queue.Len() >= s.maxQueued {
			return nil, errBusy
		}
		s.queue.PushBack(sha)
		s.queued[sha] = true
		return nil, errQueued
	}
	return s.addFillLocked(sha), nil
}

// canFillLocked returns errCached if the blob sha is on disk, and errFilling
// if a fill of the blob is in progress. If the blob is in the LRU list but
// its file isn't on disk, canFillLocked removes the blob from the list.
// s.mu must be held.
func (s *Server) canFillLocked(sha string) error {
	if e, ok := s.ents[sha]; ok {
		if _, err := os.Stat(s.path(sha)); !os.IsNotExist(err) {
			return errCached
		}
		// The blob is in the list, but its file isn't on disk. This occurs
		// if an eviction of an earlier copy of the blob removes the file
		// after a new fill writes it. Remove the blob from the list, so
		// that this fill can store it again.
		s.ll.Remove(e)
		delete(s.ents, sha)
		s.usage -= e.Value.(*blobFile).size
	}
	if s.filling[sha] != nil {
		return errFilling
	}
	return nil
}

// addFillLocked records the start of a fill of the blob sha. s.mu must be
// held.
func (s *Server) addFillLocked(sha string) *fill {
	f := &fill{
		size: -1,
		done: make(chan struct{}),
	}
	s.filling[sha] = f
	return f
}

// endFill records the end of the fill f of the blob sha. Then it removes
// blobs from the front of the queue of fills until it finds one that it can
// fill. It records the start of the fill of that blob, and returns its SHA
// and fill. The caller must call endFill for that fill. If the queue has no
// blob to fill, endFill returns a nil fill.
func (s *Server) endFill(sha string, f *fill) (nextSHA string, next *fill) {
	s.mu.Lock()
	delete(s.filling, sha)
	for next == nil && s.queue.Len() > 0 {
		nextSHA = s.queue.Remove(s.queue.Front()).(string)
		delete(s.queued, nextSHA)
		if s.canFillLocked(nextSHA) == nil {
			next = s.addFillLocked(nextSHA)
		}
	}
	s.mu.Unlock()
	close(f.done)
	if next == nil {
		return "", nil
	}
	return nextSHA, next
}

// fillSize returns the size of the blob sha that a fill in progress is
// writing, and a channel that is closed when the fill ends. It returns a nil
// channel if there is no fill of the blob in progress, or if the fill hasn't
// reserved space for the blob.
func (s *Server) fillSize(sha string) (int64, <-chan struct{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f := s.filling[sha]
	if f == nil || f.size < 0 {
		return 0, nil
	}
	return f.size, f.done
}

// reserve adds the size of the blob sha that a fill is about to write to
// s.reserved, and it evicts blobs to make space for it.
func (s *Server) reserve(sha string, size int64) error {
	if size > s.maxBytes {
		return errBlobTooLarge
	}
	s.mu.Lock()
	s.reserved += size
	// A GET of the blob can now wait for the fill.
	s.filling[sha].size = size
	s.mu.Unlock()
	s.evict()
	return nil
}

// commit releases the reservation of a fill. If ok, the fill wrote the blob,
// and commit adds it to the cache.
func (s *Server) commit(sha string, size int64, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reserved -= size
	if !ok {
		return
	}
	s.ents[sha] = s.ll.PushFront(&blobFile{
		sha:  sha,
		size: size,
		used: time.Now(),
	})
	s.usage += size
}

func (s *Server) evict() {
	var victims []string
	now := time.Now()
	s.mu.Lock()
	for s.usage+s.reserved > s.maxBytes {
		e := s.ll.Back()
		if e == nil {
			break
		}
		bf := e.Value.(*blobFile)
		s.ll.Remove(e)
		delete(s.ents, bf.sha)
		s.usage -= bf.size
		victims = append(victims, bf.sha)
		s.m.evictions.Observe(now.Sub(bf.used).Seconds())
	}
	s.mu.Unlock()
	for _, sha := range victims {
		if err := os.Remove(s.path(sha)); err != nil && !os.IsNotExist(err) {
			s.logf("blobserver: evicting %s: %v", sha, err)
		}
	}
}

// validSHA256Hex reports whether x is a lowercase hex SHA-256.
func validSHA256Hex(x string) bool {
	if len(x) != sha256.Size*2 {
		return false
	}
	for i := range len(x) {
		b := x[i]
		if b >= '0' && b <= '9' || b >= 'a' && b <= 'f' {
			continue
		}
		return false
	}
	return true
}

// ServeHTTP implements the blob-serving HTTP API as an [http.Handler]:
//
//   - GET  /blob/<sha>.lz4
//   - HEAD /blob/<sha>.lz4
//   - POST /blob/<sha>.lz4
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	sha, ok := strings.CutPrefix(r.URL.Path, "/blob/")
	sha, ok2 := strings.CutSuffix(sha, ".lz4")
	if !ok || !ok2 || !validSHA256Hex(sha) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	switch r.Method {
	case "GET", "HEAD":
		s.serveGet(w, r, sha)
	case "POST":
		s.serveWarm(w, sha)
	default:
		http.Error(w, "bad method", http.StatusMethodNotAllowed)
	}
}

func (s *Server) serveGet(w http.ResponseWriter, r *http.Request, sha string) {
	result := "hit"
	defer func() { s.m.requests.WithLabelValues(r.Method, result).Inc() }()
	f, err := os.Open(s.path(sha))
	if os.IsNotExist(err) && r.Method == "GET" {
		if size, done := s.fillSize(sha); done != nil {
			result = "filling"
			s.serveFilling(w, r, sha, size, done)
			return
		}
	}
	if os.IsNotExist(err) {
		result = "miss"
		if r.Method == "GET" {
			s.startFill(sha)
		}
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if err != nil {
		result = "error"
		s.logf("blobserver: opening %s: %v", sha, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		result = "error"
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	s.touch(sha, fi.ModTime())
	w.Header().Set("Content-Type", "application/x-lz4")
	w.Header().Set("Content-Length", strconv.FormatInt(fi.Size(), 10))
	if r.Method == "HEAD" {
		return
	}
	n, _ := io.Copy(w, f)
	s.m.servedBytes.Add(float64(n))
}

// serveFilling waits for the fill of the size-byte blob sha to end, and then
// serves the blob. It sends the response header at once, so that the client
// doesn't time out while it waits. If the fill fails, serveFilling aborts the
// response, so that the client gets an error. If the client goes away first,
// serveFilling also aborts the response.
func (s *Server) serveFilling(w http.ResponseWriter, r *http.Request, sha string, size int64, done <-chan struct{}) {
	w.Header().Set("Content-Type", "application/x-lz4")
	w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	w.WriteHeader(http.StatusOK)
	http.NewResponseController(w).Flush()
	select {
	case <-done:
	case <-r.Context().Done():
		panic(http.ErrAbortHandler)
	}
	f, err := os.Open(s.path(sha))
	if err != nil {
		panic(http.ErrAbortHandler)
	}
	defer f.Close()
	n, _ := io.Copy(w, f)
	s.m.servedBytes.Add(float64(n))
}

// startFill starts a fill of the blob sha from S3 in a new goroutine. It
// returns the error from [Server.beginFill] if it doesn't start a fill.
func (s *Server) startFill(sha string) error {
	f, err := s.beginFill(sha)
	if err != nil {
		return err
	}
	go s.runFills(sha, f)
	return nil
}

// runFills does the fill f of the blob sha. Then it does the fills from the
// queue of fills that [Server.endFill] returns, until the queue is empty.
// When busy, there may be up to [maxFills] goroutines running this function.
func (s *Server) runFills(sha string, f *fill) {
	for f != nil {
		start := time.Now()
		result := s.fillFromS3(sha)
		s.m.fills.WithLabelValues(result).Observe(time.Since(start).Seconds())
		sha, f = s.endFill(sha, f)
	}
}

// fillFromS3 gets the blob sha from S3 and stores it. It returns the result
// for the fills metric.
func (s *Server) fillFromS3(sha string) string {
	ctx, cancel := context.WithTimeout(context.Background(), fillTimeout)
	defer cancel()
	rc, size, err := s.get(ctx, sha)
	if err == nil {
		defer rc.Close()
		err = s.store(sha, rc, size)
	}
	switch {
	case err == nil:
		return "stored"
	case errors.Is(err, awscache.ErrBlobNotFound):
		return "notfound"
	case errors.Is(err, errBlobTooLarge):
		return "toolarge"
	}
	s.logf("blobserver: filling %s from S3: %v", sha, err)
	if ctx.Err() != nil {
		return "timeout"
	}
	return "error"
}

// serveWarm starts a fill of the blob sha from S3, so that the next GET of it
// is a hit.
func (s *Server) serveWarm(w http.ResponseWriter, sha string) {
	result := "started"
	switch err := s.startFill(sha); {
	case errors.Is(err, errCached):
		result = "cached"
	case errors.Is(err, errFilling):
		result = "filling"
	case errors.Is(err, errQueued):
		result = "queued"
	case errors.Is(err, errServerLoading):
		result = "loading"
	case errors.Is(err, errBusy):
		result = "busy"
	}
	s.m.warms.WithLabelValues(result).Inc()
	w.WriteHeader(http.StatusAccepted)
}

// store reserves space for the size-byte blob sha, reads it from r, and
// stores it.
func (s *Server) store(sha string, r io.Reader, size int64) error {
	if err := s.reserve(sha, size); err != nil {
		return err
	}
	err := s.write(sha, r, size)
	s.commit(sha, size, err == nil)
	// If the fills in progress needed more space than the limit, the
	// server is above the limit. Evict blobs to go below it again.
	s.evict()
	if err != nil {
		return err
	}
	s.m.fillBytes.Add(float64(size))
	return nil
}

// write writes the blob sha from r to disk. It doesn't verify the blob,
// because the S3 SDK verifies it against the checksum in S3, and the client
// verifies it again.
func (s *Server) write(sha string, r io.Reader, size int64) error {
	path := s.path(sha)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), sha+".*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	// Read past size to EOF, because the SDK only verifies the checksum at EOF.
	n, err := io.Copy(tmp, io.LimitReader(r, size+1))
	if err != nil {
		return fmt.Errorf("reading blob: %w", err)
	}
	if n != size {
		return fmt.Errorf("read %d bytes; want %d", n, size)
	}
	// Sync before the rename, so that after a crash, the blob's file isn't
	// empty or short.
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// Collectors returns the Prometheus metrics of the Server, for the caller to
// register. See [newServerMetrics] for the full documentation.
func (s *Server) Collectors() []prometheus.Collector {
	cs := []prometheus.Collector{
		s.m.requests,
		s.m.fills,
		s.m.warms,
		s.m.servedBytes,
		s.m.fillBytes,
		s.m.evictions,
		s.m.diskBytes,
		s.m.reservedBytes,
		s.m.maxBytes,
		s.m.blobs,
		s.m.fillsInProgress,
		s.m.fillsQueued,
	}
	if s.s3blobs != nil {
		cs = append(cs, s.s3blobs.Collectors()...)
	}
	return cs
}

type serverMetrics struct {
	requests        *prometheus.CounterVec
	fills           *prometheus.HistogramVec
	warms           *prometheus.CounterVec
	servedBytes     prometheus.Counter
	fillBytes       prometheus.Counter
	evictions       prometheus.Histogram
	diskBytes       prometheus.GaugeFunc
	reservedBytes   prometheus.GaugeFunc
	maxBytes        prometheus.GaugeFunc
	blobs           prometheus.GaugeFunc
	fillsInProgress prometheus.GaugeFunc
	fillsQueued     prometheus.GaugeFunc
}

func newServerMetrics(s *Server) *serverMetrics {
	gauge := func(name, help string, f func() int64) prometheus.GaugeFunc {
		return prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Name: name,
			Help: help,
		}, func() float64 {
			s.mu.Lock()
			defer s.mu.Unlock()
			return float64(f())
		})
	}
	return &serverMetrics{
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gocache_blobserver_requests_total",
			Help: "blob GET and HEAD requests, labeled by method and by result: hit, filling (waited for a fill in progress), miss (not on disk), or error",
		}, []string{"method", "result"}),
		fills: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "gocache_blobserver_fill_duration_seconds",
			Help:    "wall time of each fill from S3 after a GET miss or a POST, labeled by result: stored, notfound (not in S3), toolarge (larger than the cache), timeout, or error",
			Buckets: prometheus.ExponentialBuckets(0.01, 2, 14),
		}, []string{"result"}),
		warms: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gocache_blobserver_warms_total",
			Help: "blob POST requests, labeled by result: started (a fill started), cached (already on disk), filling (a fill of the blob in progress), queued (the blob is in the queue of fills), loading (finding the blobs on disk), or busy (the queue of fills is full)",
		}, []string{"result"}),
		servedBytes: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "gocache_blobserver_served_bytes_total",
			Help: "lz4 bytes served",
		}),
		fillBytes: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "gocache_blobserver_fill_bytes_total",
			Help: "lz4 bytes stored on disk from S3",
		}),
		evictions: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "gocache_blobserver_eviction_idle_seconds",
			Help:    "time since the last GET or fill of each blob evicted from disk",
			Buckets: prometheus.ExponentialBuckets(60, 4, 10),
		}),
		diskBytes: gauge("gocache_blobserver_disk_bytes", "bytes of blobs on disk", func() int64 {
			return s.usage
		}),
		reservedBytes: gauge("gocache_blobserver_reserved_bytes", "bytes reserved for fills in progress", func() int64 {
			return s.reserved
		}),
		maxBytes: gauge("gocache_blobserver_max_bytes", "maximum bytes of blobs on disk", func() int64 {
			return s.maxBytes
		}),
		blobs: gauge("gocache_blobserver_blobs", "blobs on disk", func() int64 {
			return int64(len(s.ents))
		}),
		fillsInProgress: gauge("gocache_blobserver_fills_in_progress", "fills from S3 in progress", func() int64 {
			return int64(len(s.filling))
		}),
		fillsQueued: gauge("gocache_blobserver_fills_queued", "blobs in the queue of fills that wait for a fill in progress to end", func() int64 {
			return int64(s.queue.Len())
		}),
	}
}
