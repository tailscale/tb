// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

// Package awscache is a Go build cache backend that stores metadata in
// DynamoDB and objects in S3, used directly by the processes that serve
// cache requests rather than through a central cache server.
//
// It has no auth and no policy: the caller decides which namespaces each
// request may read and write (a [Grant]), typically from an identity it has
// verified itself. [Handler] serves gocached's HTTP protocol, the one the
// gocache/cachers HTTP client speaks, on top of a [Cache].
//
// # Storage
//
// One DynamoDB table, partition key only (string attribute "k"), with TTL
// enabled on the "ttl" attribute. Each (namespace, action ID) is one item,
// keyed "a#<namespace>#<actionID>", holding the output ID, size, create and
// access times, its TTL, and either the object's bytes inline (objects up to
// [Config.InlineMax]) or the SHA-256 and stored size of the object's blob in
// S3. The global namespace is stored as "!global:<generation>", so bumping
// [Config.GlobalGeneration] starts it afresh while the old items expire.
//
// S3 holds lz4-compressed blobs at "<prefix><sha[:2]>/<sha>.lz4", content
// addressed by the SHA-256 of the uncompressed bytes, computed by the
// writer. Blobs are shared across namespaces and never overwritten (PUTs use
// If-None-Match: *).
//
// GETs stream blobs from S3 to the client, and check each blob's SHA-256 on
// the way: they decompress it as they send it (or decompress a copy, if the
// client accepts lz4), and hold back the last byte of the response until
// the hash matches. A blob of the wrong size is a miss. A blob with other
// content (damaged, or overwritten) makes the response fail before its end,
// so the client discards it.
//
// The bucket may be an S3 Express One Zone directory bucket (a name ending
// in "--x-s3"), whose time to first byte is much lower than S3 Standard's.
// A directory bucket can't require If-None-Match: * on PUTs, so anything
// with write access to it can overwrite a blob. The hash check on GET makes
// an overwritten blob fail rather than serve other content.
//
// A PUT writes its blob before its metadata and returns once both are
// durable, so an acknowledged PUT is readable from every host. The first PUT
// of an action in a namespace wins.
//
// # Garbage collection
//
// GET hits bump an action's access time and TTL at most daily
// (relatime-style, in the background), so DynamoDB TTL expires actions
// unused for [Config.Retention]. Blobs aren't deleted by this package; a
// separate scrubber deletes blobs no longer referenced by any action after
// a grace period. Blob deletion must allow for this package's in-process
// memory of recently seen blobs, which lives as long as a [Cache].
//
// See ci/doc/aws-native-gocached.md in Tailscale's corp repo for the design.
package awscache

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/ratelimit"
	"github.com/aws/aws-sdk-go-v2/aws/retry"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/pierrec/lz4/v4"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/tailscale/tb/gocache/logger"
	"golang.org/x/sync/semaphore"
	"tailscale.com/util/lru"
)

// Namespace is a cache namespace: a partition of the action ID space, so
// that, for example, builds of untrusted branches can't poison the cache
// that builds of main read. Every reader reads [GlobalNamespace].
//
// Namespaces must pass [ValidateNamespace].
type Namespace string

// GlobalNamespace is the namespace every [Grant] reads, and which trusted
// writers write.
const GlobalNamespace Namespace = ""

// namespaceAllowedBytes are the non-alphanumeric bytes allowed in a
// namespace, the same as gocached's. '#' and '!' aren't among them, which
// keeps storage keys unambiguous.
const namespaceAllowedBytes = "._~:/@+|=-[]"

// maxNamespaceLen bounds namespaces, as they're part of the DynamoDB
// partition key, which is limited to 2048 bytes.
const maxNamespaceLen = 1024

// ValidateNamespace reports whether ns is a valid namespace: at most 1024
// bytes of ASCII letters, digits, and the punctuation "._~:/@+|=-[]".
func ValidateNamespace(ns Namespace) error {
	if len(ns) > maxNamespaceLen {
		return fmt.Errorf("namespace is %d bytes; max %d", len(ns), maxNamespaceLen)
	}
	for i := 0; i < len(ns); i++ {
		c := ns[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		case strings.IndexByte(namespaceAllowedBytes, c) >= 0:
		default:
			return fmt.Errorf("namespace contains disallowed byte %#x at index %d", c, i)
		}
	}
	return nil
}

// Grant is what a request may do: it always reads [GlobalNamespace], and
// optionally one more namespace, and it may write one namespace. The zero
// value is read-only access to the global namespace.
type Grant struct {
	// CanWrite is whether the request may write, to WriteNamespace.
	CanWrite bool

	// WriteNamespace is the namespace written to, if CanWrite. The empty
	// string (GlobalNamespace) means the global namespace.
	WriteNamespace Namespace

	// ExtraReadNamespace, if non-empty, is read in addition to the global
	// namespace. Hits in the global namespace win.
	ExtraReadNamespace Namespace
}

func (g Grant) validate() error {
	return errors.Join(ValidateNamespace(g.WriteNamespace), ValidateNamespace(g.ExtraReadNamespace))
}

// ErrReadOnly is returned by [Cache.Put] for a read-only [Grant].
var ErrReadOnly = errors.New("awscache: read-only grant")

// Config configures a [Cache]. Zero values of optional fields mean the
// documented defaults.
type Config struct {
	// Region is the AWS region of the bucket and table. If empty, it comes
	// from the AWS SDK's default config chain (e.g. AWS_REGION), and failing
	// that, EC2 instance metadata.
	Region string

	// Bucket is the S3 bucket holding blobs. Required. It may be an S3
	// Express One Zone directory bucket (a name ending in "--x-s3"), which
	// should be in the same AZ as the clients.
	Bucket string

	// BlobPrefix is prepended to every S3 key, e.g. "v1/".
	BlobPrefix string

	// Table is the DynamoDB table holding action metadata. Required. It
	// must have a string partition key "k" and no sort key, and should have
	// TTL enabled on the "ttl" attribute.
	Table string

	// GlobalGeneration is the generation of the global namespace.
	// Incrementing it effectively wipes the global namespace; the old
	// generation's items expire through TTL. Generations 0 and 1 are the
	// same.
	GlobalGeneration int

	// ConsistentReads makes action lookups strongly consistent, at twice
	// the read cost. Eventually consistent reads are almost always fresh
	// within a second, which is plenty for a cache.
	ConsistentReads bool

	// InlineMax is the largest object (uncompressed) stored inline in its
	// DynamoDB item rather than in S3. DynamoDB bills writes per 1KB of
	// item, and the rest of an item is about 300 bytes, so the default of
	// 700 keeps inline writes at one write unit. At most 256KiB, as
	// DynamoDB items are limited to 400KB. -1 means never inline anything,
	// not even empty objects.
	InlineMax int64

	// MaxObjectSize is the largest object a PUT may store. Default 2GiB;
	// at most 5GiB (S3's single-request limit).
	MaxObjectSize int64

	// MemBudget bounds the bytes of objects buffered in memory at once
	// while passing through PUTs. Objects that don't fit are buffered on
	// disk in SpoolDir instead. Default 256MiB.
	MemBudget int64

	// SpoolDir is where objects that don't fit MemBudget are buffered.
	// Default os.TempDir().
	SpoolDir string

	// SpoolCap bounds the total bytes buffered in SpoolDir at once; PUTs
	// wait for space beyond it. Default 8GiB.
	SpoolCap int64

	// KnownBlobs is how many S3 blob keys known to exist this process
	// remembers, to skip the HeadObject on PUTs of content already stored.
	// Default 64Ki.
	KnownBlobs int

	// Retention is how long an action lives without being read, through
	// DynamoDB TTL, give or take Relatime. Default 30 days.
	Retention time.Duration

	// Relatime is how old an action's access time must be before a GET
	// bumps it (and its TTL). Default 24h.
	Relatime time.Duration

	// ATimeQueueLen, ATimeRate, and ATimeWorkers bound the background
	// access time bump queue: how many bumps may be queued (more are
	// dropped), bumps per second, and concurrent writers. Defaults 10k, 50,
	// and 4.
	ATimeQueueLen int
	ATimeRate     float64
	ATimeWorkers  int

	// Logf logs. Default log.Printf.
	Logf logger.Logf

	// AWSConfig, if non-nil, is the AWS SDK config to use instead of
	// loading the default one.
	AWSConfig *aws.Config

	// TODO(bradfitz): optional same-AZ read-through NVMe blob cache tier
	// (stateless pods on EKS, discovered via "m#cache#<az>" DynamoDB items
	// and sharded by rendezvous hashing of the blob SHA-256), per the "Cache
	// tier" section of ci/doc/aws-native-gocached.md. It's purely a latency
	// optimization; everything here works against S3 alone.

	// Test hooks, replacing the AWS clients and clock.
	meta  metaStore
	blobs blobStore
	now   func() time.Time
}

// Cache is a Go build cache stored in DynamoDB and S3. It's safe for
// concurrent use. Its only local state is buffers and caches.
type Cache struct {
	cfg      Config // with defaults filled in
	logf     logger.Logf
	now      func() time.Time
	m        *metrics
	meta     metaStore
	blobs    blobStore
	checks   []func(context.Context) error
	globalNS storageNS
	bumper   *atimeBumper
	memSem   *semaphore.Weighted
	diskSem  *semaphore.Weighted

	knownMu    sync.Mutex
	knownBlobs *lru.Cache[string, bool]

	bgCancel    context.CancelFunc
	drainCancel context.CancelFunc
	bgDone      chan struct{}
	closeOnce   sync.Once
}

// New returns a new Cache using the given config. It doesn't contact AWS
// (except perhaps for credentials and the region); see [Cache.Check].
//
// ctx is only used for loading the AWS config.
func New(ctx context.Context, cfg Config) (*Cache, error) {
	if err := cfg.fillDefaults(); err != nil {
		return nil, err
	}
	m := newMetrics()
	var checks []func(context.Context) error
	if cfg.meta == nil || cfg.blobs == nil {
		if cfg.Bucket == "" || cfg.Table == "" {
			return nil, errors.New("awscache: Bucket and Table are required")
		}
		var awsCfg aws.Config
		if cfg.AWSConfig != nil {
			awsCfg = *cfg.AWSConfig
		} else {
			var err error
			awsCfg, err = loadAWSConfig(ctx, cfg.Region)
			if err != nil {
				return nil, fmt.Errorf("awscache: loading AWS config: %w", err)
			}
		}
		ddb := &dynamoStore{c: dynamodb.NewFromConfig(awsCfg), table: cfg.Table, consistentReads: cfg.ConsistentReads, m: m}
		bs := &s3Store{c: s3.NewFromConfig(awsCfg), bucket: cfg.Bucket, m: m}
		cfg.meta, cfg.blobs = ddb, bs
		checks = append(checks, ddb.checkTable, bs.checkBucket)
	}

	c := &Cache{
		cfg:        cfg,
		logf:       cfg.Logf,
		now:        cfg.now,
		m:          m,
		meta:       cfg.meta,
		blobs:      cfg.blobs,
		checks:     checks,
		globalNS:   globalStorageNS(cfg.GlobalGeneration),
		memSem:     semaphore.NewWeighted(cfg.MemBudget),
		diskSem:    semaphore.NewWeighted(max(cfg.SpoolCap, 1)),
		knownBlobs: &lru.Cache[string, bool]{MaxEntries: cfg.KnownBlobs},
		bgDone:     make(chan struct{}),
	}
	c.bumper = newATimeBumper(c.meta, c.logf, m, c.now, cfg.Relatime, cfg.Retention, cfg.ATimeQueueLen, cfg.ATimeRate)

	bgCtx, bgCancel := context.WithCancel(context.Background())
	drainCtx, drainCancel := context.WithCancel(context.Background())
	c.bgCancel, c.drainCancel = bgCancel, drainCancel
	go func() {
		defer close(c.bgDone)
		c.bumper.run(bgCtx, drainCtx, cfg.ATimeWorkers)
	}()
	return c, nil
}

func (cfg *Config) fillDefaults() error {
	if cfg.InlineMax == 0 {
		cfg.InlineMax = 700
	}
	if cfg.MaxObjectSize == 0 {
		cfg.MaxObjectSize = 2 << 30
	}
	if cfg.MemBudget == 0 {
		cfg.MemBudget = 256 << 20
	}
	if cfg.SpoolDir == "" {
		cfg.SpoolDir = os.TempDir()
	}
	if cfg.SpoolCap == 0 {
		cfg.SpoolCap = 8 << 30
	}
	if cfg.KnownBlobs == 0 {
		cfg.KnownBlobs = 64 << 10
	}
	if cfg.Retention == 0 {
		cfg.Retention = 30 * 24 * time.Hour
	}
	if cfg.Relatime == 0 {
		cfg.Relatime = 24 * time.Hour
	}
	if cfg.ATimeQueueLen == 0 {
		cfg.ATimeQueueLen = 10_000
	}
	if cfg.ATimeRate == 0 {
		cfg.ATimeRate = 50
	}
	if cfg.ATimeWorkers == 0 {
		cfg.ATimeWorkers = 4
	}
	if cfg.Logf == nil {
		cfg.Logf = logPrintf
	}
	if cfg.now == nil {
		cfg.now = time.Now
	}
	switch {
	case cfg.InlineMax < -1 || cfg.InlineMax > 256<<10:
		return fmt.Errorf("awscache: InlineMax %d out of range [-1, 256KiB]", cfg.InlineMax)
	case cfg.MaxObjectSize <= cfg.InlineMax:
		return fmt.Errorf("awscache: MaxObjectSize %d must exceed InlineMax %d", cfg.MaxObjectSize, cfg.InlineMax)
	case cfg.MaxObjectSize > 5<<30:
		return fmt.Errorf("awscache: MaxObjectSize %d exceeds S3's 5GiB single PUT limit", cfg.MaxObjectSize)
	case cfg.MemBudget < 0, cfg.SpoolCap < 0, cfg.Retention < 0, cfg.Relatime < 0, cfg.GlobalGeneration < 0:
		return errors.New("awscache: MemBudget, SpoolCap, Retention, Relatime, and GlobalGeneration must not be negative")
	case cfg.Relatime >= cfg.Retention:
		return fmt.Errorf("awscache: Relatime %v must be less than Retention %v", cfg.Relatime, cfg.Retention)
	}
	return nil
}

// loadAWSConfig returns the AWS SDK config for region, with an HTTP
// transport and retryer tuned for many concurrent small requests.
func loadAWSConfig(ctx context.Context, region string) (aws.Config, error) {
	hc := awshttp.NewBuildableClient().WithTransportOptions(func(tr *http.Transport) {
		// The default of 2 idle conns per host would make nearly every
		// request under load dial a new TLS connection to S3/DynamoDB.
		tr.MaxIdleConns = 512
		tr.MaxIdleConnsPerHost = 256
		tr.IdleConnTimeout = 90 * time.Second
	})
	opts := []func(*config.LoadOptions) error{
		config.WithHTTPClient(hc),
		config.WithRetryer(func() aws.Retryer {
			return retry.NewStandard(func(o *retry.StandardOptions) {
				o.MaxAttempts = 3
				// The default client-side retry quota fails requests
				// outright once a burst of errors drains it, which turns a
				// brief throttling blip into a wave of cache errors. Let
				// the server-side throttling and backoff do their job.
				o.RateLimiter = ratelimit.None
			})
		}),
	}
	if region != "" {
		opts = append(opts, config.WithRegion(region))
	} else {
		opts = append(opts, config.WithEC2IMDSRegion())
	}
	return config.LoadDefaultConfig(ctx, opts...)
}

// Check verifies that the table and bucket exist and are reachable with the
// cache's credentials, to catch misconfiguration early. It needs the
// dynamodb:DescribeTable and s3:ListBucket permissions, which the cache
// itself doesn't otherwise need.
func (c *Cache) Check(ctx context.Context) error {
	var errs []error
	for _, check := range c.checks {
		errs = append(errs, check(ctx))
	}
	return errors.Join(errs...)
}

// Collectors returns the cache's Prometheus metrics, for the caller to
// register.
func (c *Cache) Collectors() []prometheus.Collector {
	return c.m.collectors()
}

// Close stops the cache's background work, first spending up to a few
// seconds (or until ctx is done) writing queued access time bumps. It
// doesn't wait for in-flight GETs and PUTs.
func (c *Cache) Close(ctx context.Context) error {
	c.closeOnce.Do(func() {
		ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		stop := context.AfterFunc(ctx, c.drainCancel)
		defer stop()
		c.bgCancel()
		<-c.bgDone
		c.drainCancel()
	})
	return nil
}

// storageNS returns the storage namespace for ns.
func (c *Cache) storageNS(ns Namespace) storageNS {
	if ns == GlobalNamespace {
		return c.globalNS
	}
	return storageNS(ns)
}

// Object is a cache hit returned by [Cache.Get]. Callers must Close it.
//
// A blob's bytes stream from S3 as they're written. Its hash is checked on
// the way, and the last byte is written only if the hash matches (see the
// package docs). Use only one of WriteTo and WriteLZ4To, once.
type Object struct {
	// OutputID is the Go output ID (hex) stored with the action.
	OutputID string

	// Size is the object's uncompressed size.
	Size int64

	inline   []byte        // if stored inline
	blob     io.ReadCloser // else the lz4 blob
	blobSize int64         // size of blob
	sha256   string        // hex SHA-256 of the blob's uncompressed content

	// storage is where the object came from: "inline" or "s3".
	storage string
}

// errBadBlob is the error, wrapped, from [Object.WriteTo] and
// [Object.WriteLZ4To] when the blob's content is bad, as opposed to an error
// writing it.
var errBadBlob = errors.New("bad blob")

// WriteTo writes the object's uncompressed bytes to w.
//
// For a blob, it writes the last byte only after the content's SHA-256
// matches. Thus a client that knows Size gets a short body if the blob is
// bad.
func (o *Object) WriteTo(w io.Writer) (int64, error) {
	if o.blob == nil {
		n, err := w.Write(o.inline)
		return int64(n), err
	}
	hw := &holdBackWriter{w: w}
	if err := o.check(o.blob, hw); err != nil {
		return hw.n, o.blobError(hw, err)
	}
	err := hw.flush()
	return hw.n, err
}

// LZ4Size returns the size of the object as an lz4 frame, if it's stored
// compressed. Otherwise it returns ok false.
func (o *Object) LZ4Size() (size int64, ok bool) {
	if o.blob == nil {
		return 0, false
	}
	return o.blobSize, true
}

// WriteLZ4To writes the object, which must be stored compressed (see
// LZ4Size), to w as an lz4 frame.
//
// It writes the last byte only after the content's SHA-256 matches. Thus a
// client that knows the size gets a short body if the blob is bad.
func (o *Object) WriteLZ4To(w io.Writer) (int64, error) {
	if o.blob == nil {
		return 0, errors.New("object isn't stored compressed")
	}
	hw := &holdBackWriter{w: w}
	if err := o.check(io.TeeReader(o.blob, hw), nil); err != nil {
		return hw.n, o.blobError(hw, err)
	}
	if got := hw.n + int64(len(hw.last)); got != o.blobSize {
		return hw.n, fmt.Errorf("%w: lz4 frame is %d bytes; want %d", errBadBlob, got, o.blobSize)
	}
	err := hw.flush()
	return hw.n, err
}

// check decompresses the lz4 frame from src, writing the content to dst if
// non-nil, and checks the content's size and SHA-256. Because the lz4
// reader looks for another frame after the first one, it reads src to its
// end.
func (o *Object) check(src io.Reader, dst io.Writer) error {
	h := sha256.New()
	var out io.Writer = h
	if dst != nil {
		out = io.MultiWriter(dst, h)
	}
	n, err := io.Copy(out, io.LimitReader(lz4.NewReader(src), o.Size+1))
	if err != nil {
		return fmt.Errorf("decompressing: %w", err)
	}
	if n != o.Size {
		return fmt.Errorf("decompressed to %d bytes; want %d", n, o.Size)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != o.sha256 {
		return fmt.Errorf("content has sha256 %s; want %s", got, o.sha256)
	}
	return nil
}

// blobError returns err, from streaming the blob through hw, wrapped in
// errBadBlob unless it came from writing to hw's writer.
func (o *Object) blobError(hw *holdBackWriter, err error) error {
	if hw.err != nil {
		return hw.err
	}
	return fmt.Errorf("%w %s: %v", errBadBlob, o.sha256, err)
}

// holdBackWriter writes to w everything written to it but the last byte,
// which it writes on flush.
type holdBackWriter struct {
	w    io.Writer
	last []byte // the last byte written, if any, not yet written to w
	n    int64  // bytes written to w
	err  error  // the first error writing to w
}

func (h *holdBackWriter) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if err := h.write(h.last); err != nil {
		return 0, err
	}
	if err := h.write(p[:len(p)-1]); err != nil {
		return 0, err
	}
	h.last = append(h.last[:0], p[len(p)-1])
	return len(p), nil
}

func (h *holdBackWriter) write(p []byte) error {
	if h.err != nil {
		return h.err
	}
	if len(p) == 0 {
		return nil
	}
	n, err := h.w.Write(p)
	h.n += int64(n)
	h.err = err
	return err
}

// flush writes the last byte.
func (h *holdBackWriter) flush() error {
	err := h.write(h.last)
	h.last = nil
	return err
}

// Close releases the object's resources.
func (o *Object) Close() error {
	if o.blob != nil {
		return o.blob.Close()
	}
	return nil
}

// validHex reports whether x is a plausible action or output ID: non-empty
// lowercase hex of even length, at most 1000 bytes. Same as gocached's.
func validHex(x string) bool {
	if len(x) < 4 || len(x) > 1000 || len(x)%2 == 1 {
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

// ErrBadID is returned for malformed action and output IDs.
var ErrBadID = errors.New("awscache: malformed action or output ID")

// Get looks up actionID in the namespaces g may read, returning (nil, nil)
// on a miss. A hit's bytes stream from storage as they're written, and
// are checked on the way.
func (c *Cache) Get(ctx context.Context, g Grant, actionID string) (*Object, error) {
	o, _, err := c.get(ctx, g, actionID)
	return o, err
}

// get is Get, also reporting whether a hit enqueued an access time bump.
func (c *Cache) get(ctx context.Context, g Grant, actionID string) (_ *Object, bumped bool, _ error) {
	if !validHex(actionID) {
		return nil, false, ErrBadID
	}
	if err := g.validate(); err != nil {
		return nil, false, err
	}
	key, e, err := c.lookupAction(ctx, g, actionID)
	if err != nil || e == nil {
		return nil, false, err
	}
	if e.Inline {
		if int64(len(e.Data)) != e.Size {
			c.logf("awscache: GET %s: corrupt inline entry: %d bytes, want %d", key, len(e.Data), e.Size)
			c.m.corruptBlobs.Inc()
			return nil, false, nil
		}
		bumped = c.bumper.maybeEnqueue(key, e.AccessTime)
		return &Object{OutputID: e.OutputID, Size: e.Size, inline: e.Data, storage: "inline"}, bumped, nil
	}
	if len(e.SHA256) != sha256.Size*2 || !validHex(e.SHA256) {
		c.logf("awscache: GET %s: corrupt entry: bad sha256 %q", key, e.SHA256)
		c.m.corruptBlobs.Inc()
		return nil, false, nil
	}
	blob, err := c.openBlob(ctx, key, e)
	if err != nil || blob == nil {
		return nil, false, err
	}
	bumped = c.bumper.maybeEnqueue(key, e.AccessTime)
	return &Object{
		OutputID: e.OutputID,
		Size:     e.Size,
		blob:     blob,
		blobSize: e.StoredSize,
		sha256:   e.SHA256,
		storage:  "s3",
	}, bumped, nil
}

// lookupAction finds actionID in the global namespace or, failing that, g's
// extra read namespace, querying both concurrently. It returns the key of
// the hit, or a nil entry on a miss.
func (c *Cache) lookupAction(ctx context.Context, g Grant, actionID string) (key string, e *actionEntry, err error) {
	globalKey := actionKey(c.globalNS, actionID)
	if g.ExtraReadNamespace == GlobalNamespace {
		e, err := c.meta.GetAction(ctx, globalKey)
		return globalKey, e, err
	}
	extraKey := actionKey(c.storageNS(g.ExtraReadNamespace), actionID)

	var extra *actionEntry
	var extraErr error
	done := make(chan struct{})
	go func() {
		defer close(done)
		extra, extraErr = c.meta.GetAction(ctx, extraKey)
	}()
	global, globalErr := c.meta.GetAction(ctx, globalKey)
	<-done

	// Prefer global, as gocached does, so the shared entry's access time is
	// the one kept fresh.
	switch {
	case global != nil:
		return globalKey, global, nil
	case extra != nil:
		return extraKey, extra, nil
	default:
		return "", nil, errors.Join(globalErr, extraErr)
	}
}

// openBlob opens the blob of the non-inline action e (at key). It returns
// (nil, nil) if the blob is missing or isn't the size e says.
func (c *Cache) openBlob(ctx context.Context, key string, e *actionEntry) (io.ReadCloser, error) {
	// TODO(bradfitz): first try the optional same-AZ read-through NVMe
	// cache pool (see Config), hedging to S3 directly if the pool doesn't
	// start answering quickly. Until then, every blob GET goes to S3.
	rc, size, err := c.blobs.Get(ctx, blobKey(c.cfg.BlobPrefix, e.SHA256))
	if errors.Is(err, errBlobNotFound) {
		// PUT writes the blob before the action, so this means the
		// scrubber deleted the blob after this action's last use. Treat it
		// as a miss; the next PUT of the action can't repair it (the first
		// PUT wins), but the item expires.
		//
		// TODO(bradfitz): delete the dangling action item, so the next PUT
		// repairs it, once the scrubber exists and its races are clear.
		c.logf("awscache: GET %s: blob %s missing", key, e.SHA256)
		c.m.danglingActions.Inc()
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("fetching blob %s: %w", e.SHA256, err)
	}
	if size != e.StoredSize {
		rc.Close()
		c.logf("awscache: GET %s: blob %s is %d bytes; want %d", key, e.SHA256, size, e.StoredSize)
		c.m.corruptBlobs.Inc()
		return nil, nil
	}
	return rc, nil
}

// putResult is the outcome of a put: whether and where it stored the object.
type putResult struct {
	storage string // "inline", "s3", or "dup" (already stored)
	created bool   // false if the action already existed
}

// Put stores the size-byte object read from body as the output outputID of
// actionID in g's write namespace, unless that namespace already has an
// entry for actionID (the first PUT wins). It returns once the object is
// durable and visible to other hosts, or with [ErrReadOnly] if g can't
// write.
//
// If the action already exists, Put returns without reading body.
func (c *Cache) Put(ctx context.Context, g Grant, actionID, outputID string, size int64, body io.Reader) error {
	_, err := c.put(ctx, g, actionID, outputID, size, body)
	return err
}

// putStoreTimeout returns how long the blob upload and metadata write for a
// PUT of n bytes may take once its body has been received.
func putStoreTimeout(n int64) time.Duration {
	return time.Minute + time.Duration(n/(8<<20))*time.Second
}

// ErrTooLarge is returned by [Cache.Put] for objects larger than
// [Config.MaxObjectSize].
var ErrTooLarge = errors.New("awscache: object too large")

func (c *Cache) put(ctx context.Context, g Grant, actionID, outputID string, size int64, body io.Reader) (putResult, error) {
	if !validHex(actionID) || !validHex(outputID) {
		return putResult{}, ErrBadID
	}
	if !g.CanWrite {
		return putResult{}, ErrReadOnly
	}
	if err := g.validate(); err != nil {
		return putResult{}, err
	}
	if size < 0 {
		return putResult{}, errors.New("awscache: negative size")
	}
	if size > c.cfg.MaxObjectSize {
		return putResult{}, ErrTooLarge
	}
	key := actionKey(c.storageNS(g.WriteNamespace), actionID)

	// Most PUTs in CI are for actions some earlier job already stored, so
	// check before doing any work.
	if exists, err := c.meta.ActionExists(ctx, key); err != nil {
		// Not fatal: the conditional write below still keeps the first.
		c.logf("awscache: PUT %s: existence check: %v", key, err)
	} else if exists {
		return putResult{storage: "dup"}, nil
	}

	now := c.now()
	e := &actionEntry{
		OutputID:   outputID,
		Size:       size,
		CreateTime: now.Unix(),
		AccessTime: now.Unix(),
		TTL:        now.Add(c.cfg.Retention).Unix(),
	}

	res := putResult{storage: "inline"}
	if size <= c.cfg.InlineMax {
		data, err := io.ReadAll(io.LimitReader(body, size+1))
		if err != nil {
			return putResult{}, fmt.Errorf("reading body: %w", err)
		}
		if int64(len(data)) != size {
			return putResult{}, fmt.Errorf("body was %d bytes; want %d", len(data), size)
		}
		e.Inline = true
		if size > 0 {
			e.Data = data
		}
	} else {
		res.storage = "s3"
		if err := c.storeBlob(ctx, key, e, body); err != nil {
			return putResult{}, err
		}
	}

	// The body has been fully received, so finish storing it even if the
	// client gives up waiting: it's still useful to the next client.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), putStoreTimeout(size))
	defer cancel()
	created, err := c.meta.PutActionIfAbsent(ctx, key, e)
	if err != nil {
		return putResult{}, fmt.Errorf("storing metadata: %w", err)
	}
	if !created {
		// Lost a race with another PUT of the same action.
		res.storage = "dup"
	}
	res.created = created
	return res, nil
}

// storeBlob reads e.Size bytes from body into a spool, hashing and
// lz4-compressing them on the way, then uploads the result to S3 under its
// content address, unless it's already there. It fills in e's SHA256 and
// StoredSize.
func (c *Cache) storeBlob(ctx context.Context, key string, e *actionEntry, body io.Reader) error {
	sp, err := c.newSpool(ctx, e.Size)
	if err != nil {
		return err
	}
	defer sp.Close()

	h := sha256.New()
	zw := lz4.NewWriter(sp)
	if err := zw.Apply(lz4.SizeOption(uint64(e.Size))); err != nil {
		return err
	}
	n, err := io.Copy(io.MultiWriter(zw, h), io.LimitReader(body, e.Size+1))
	if err != nil {
		return fmt.Errorf("reading body: %w", err)
	}
	if n != e.Size {
		return fmt.Errorf("body was %d bytes; want %d", n, e.Size)
	}
	if err := zw.Close(); err != nil {
		return fmt.Errorf("compressing: %w", err)
	}
	e.SHA256 = hex.EncodeToString(h.Sum(nil))
	e.StoredSize = sp.Size()

	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), putStoreTimeout(e.Size))
	defer cancel()
	bk := blobKey(c.cfg.BlobPrefix, e.SHA256)
	if c.blobKnown(bk) {
		c.m.blobSkips.WithLabelValues("known").Inc()
		return nil
	}
	// TODO(bradfitz): ask the optional NVMe cache pool before S3, which
	// would usually know about recently written blobs.
	exists, err := c.blobs.Exists(ctx, bk)
	if err != nil {
		// Not fatal: an upload is still correct.
		c.logf("awscache: PUT %s: checking for blob %s: %v", key, e.SHA256, err)
	}
	if exists {
		c.m.blobSkips.WithLabelValues("head").Inc()
	} else {
		r, err := sp.Reader()
		if err != nil {
			return err
		}
		if err := c.blobs.Put(ctx, bk, r, e.StoredSize); err != nil {
			return fmt.Errorf("uploading blob %s: %w", e.SHA256, err)
		}
		c.m.blobUploads.Inc()
		c.m.blobUploadBytes.Add(float64(e.StoredSize))
	}
	c.noteBlobKnown(bk)
	return nil
}

// blobKnown reports whether this process recently saw blob key bk exist.
//
// This relies on the scrubber only deleting blobs that no action has
// referenced for far longer than a Cache lives.
func (c *Cache) blobKnown(bk string) bool {
	c.knownMu.Lock()
	defer c.knownMu.Unlock()
	return c.knownBlobs.Contains(bk)
}

func (c *Cache) noteBlobKnown(bk string) {
	c.knownMu.Lock()
	defer c.knownMu.Unlock()
	c.knownBlobs.Set(bk, true)
}

func logPrintf(format string, args ...any) { log.Printf(format, args...) }
