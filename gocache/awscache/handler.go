// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package awscache

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Handler serves gocached's HTTP cache protocol (as spoken by the
// gocache/cachers HTTP client, e.g. in cigocacher) from a [Cache]:
//
//   - GET and HEAD /action/<actionID>, with "Want-Object: 1", returning the
//     object with its output ID in the Go-Output-Id header, lz4-compressed
//     if the request accepts it;
//   - PUT /<actionID>/<outputID>, with a Content-Length, storing it;
//   - GET /session/stats, returning this Handler's [Stats] as JSON.
//
// It does no authentication: the Grant func decides what each request may
// do. There's no token exchange; requests carrying Authorization headers
// have them ignored.
type Handler struct {
	c     *Cache
	grant func(*http.Request) Grant

	st stats
}

// NewHandler returns a Handler serving c, with grant deciding what each
// request may read and write. grant may block (for instance, until the
// caller has learned who it's serving), subject to the request context.
func (c *Cache) NewHandler(grant func(*http.Request) Grant) *Handler {
	return &Handler{c: c, grant: grant}
}

// Stats are counters of a [Handler]'s requests. The fields and their JSON
// names match gocached's session stats, served at /session/stats, except for
// GetTiers and PutTiers, which gocached doesn't have.
//
// The durations are sums over requests, which run concurrently, so they can
// add up to more than the wall time.
type Stats struct {
	LastUsed       time.Time
	Gets           int64
	GetBytes       int64
	GetHits        int64
	GetAccessBumps int64
	GetHitsInline  int64
	GetNanos       int64
	GetErrs        int64
	Puts           int64
	PutErrs        int64
	PutsDup        int64
	PutsBytes      int64
	PutsInline     int64
	PutsNanos      int64

	// GetTiers breaks gets down by where they were served from: "inline"
	// (from DynamoDB), "s3", "miss", or "error".
	GetTiers map[string]GetTierStats `json:",omitempty"`

	// PutTiers breaks puts down by where they stored the object: "inline",
	// "s3", "dup" (already stored, so not stored again), or "error".
	PutTiers map[string]PutTierStats `json:",omitempty"`
}

// GetTierStats are the counters of the gets served from one tier, with the
// total time spent in each phase.
type GetTierStats struct {
	Gets  int64
	Nanos int64 // whole gets

	LookupNanos    int64 // DynamoDB action lookups
	FirstByteNanos int64 // blob GETs, to their response headers
	TransferNanos  int64 // sending response bodies, streaming blobs from S3

	BlobBytes int64 // blob (lz4) bytes streamed from S3
	SentBytes int64 // response body bytes written to the client
}

// PutTierStats are the counters of the puts that stored objects in one tier,
// with the total time spent in each phase.
type PutTierStats struct {
	Puts  int64
	Nanos int64 // whole puts

	ExistsNanos    int64 // DynamoDB checks for an existing action
	SpoolWaitNanos int64 // waiting for buffer space for blobs
	ReceiveNanos   int64 // reading bodies (and hashing and compressing blobs)
	BlobCheckNanos int64 // checking whether blobs are already stored
	UploadNanos    int64 // uploading blobs
	MetaNanos      int64 // writing action items

	BlobUploads int64 // blobs uploaded, rather than found already stored
	BlobBytes   int64 // blob (lz4) bytes uploaded
}

type stats struct {
	lastUsed       atomic.Int64 // unix nanos
	gets           atomic.Int64
	getBytes       atomic.Int64
	getHits        atomic.Int64
	getAccessBumps atomic.Int64
	getHitsInline  atomic.Int64
	getNanos       atomic.Int64
	getErrs        atomic.Int64
	puts           atomic.Int64
	putErrs        atomic.Int64
	putsDup        atomic.Int64
	putsBytes      atomic.Int64
	putsInline     atomic.Int64
	putsNanos      atomic.Int64

	tierMu   sync.Mutex
	getTiers map[string]*GetTierStats // or nil
	putTiers map[string]*PutTierStats // or nil
}

// Stats returns a snapshot of h's counters.
func (h *Handler) Stats() Stats {
	s := &h.st
	st := Stats{
		Gets:           s.gets.Load(),
		GetBytes:       s.getBytes.Load(),
		GetHits:        s.getHits.Load(),
		GetAccessBumps: s.getAccessBumps.Load(),
		GetHitsInline:  s.getHitsInline.Load(),
		GetNanos:       s.getNanos.Load(),
		GetErrs:        s.getErrs.Load(),
		Puts:           s.puts.Load(),
		PutErrs:        s.putErrs.Load(),
		PutsDup:        s.putsDup.Load(),
		PutsBytes:      s.putsBytes.Load(),
		PutsInline:     s.putsInline.Load(),
		PutsNanos:      s.putsNanos.Load(),
	}
	if n := s.lastUsed.Load(); n != 0 {
		st.LastUsed = time.Unix(0, n).UTC()
	}
	s.tierMu.Lock()
	defer s.tierMu.Unlock()
	if len(s.getTiers) > 0 {
		st.GetTiers = map[string]GetTierStats{}
		for tier, ts := range s.getTiers {
			st.GetTiers[tier] = *ts
		}
	}
	if len(s.putTiers) > 0 {
		st.PutTiers = map[string]PutTierStats{}
		for tier, ts := range s.putTiers {
			st.PutTiers[tier] = *ts
		}
	}
	return st
}

// recordGet records the timing of one get, which took d in all, in h's
// stats and the cache's metrics. transfer is how long it took to send the
// response body, sent bytes of it, which for a blob streamed blobBytes from
// S3.
func (h *Handler) recordGet(tier string, d time.Duration, tm *getTiming, transfer time.Duration, sent, blobBytes int64) {
	m := h.c.m
	phases := []struct {
		name string
		d    time.Duration
		sum  func(*GetTierStats) *int64
	}{
		{"lookup", tm.lookup, func(ts *GetTierStats) *int64 { return &ts.LookupNanos }},
		{"first_byte", tm.firstByte, func(ts *GetTierStats) *int64 { return &ts.FirstByteNanos }},
		{"transfer", transfer, func(ts *GetTierStats) *int64 { return &ts.TransferNanos }},
	}
	if blobBytes > 0 {
		m.blobBytes.WithLabelValues("get", tier).Add(float64(blobBytes))
	}

	s := &h.st
	s.tierMu.Lock()
	defer s.tierMu.Unlock()
	if s.getTiers == nil {
		s.getTiers = map[string]*GetTierStats{}
	}
	ts := s.getTiers[tier]
	if ts == nil {
		ts = new(GetTierStats)
		s.getTiers[tier] = ts
	}
	ts.Gets++
	ts.Nanos += d.Nanoseconds()
	ts.BlobBytes += blobBytes
	ts.SentBytes += sent
	for _, p := range phases {
		if p.d > 0 {
			m.getPhase.WithLabelValues(tier, p.name).Observe(p.d.Seconds())
			*p.sum(ts) += p.d.Nanoseconds()
		}
	}
}

// recordPut records the timing of one put, which took d in all, in h's
// stats and the cache's metrics.
func (h *Handler) recordPut(tier string, d time.Duration, tm *putTiming) {
	m := h.c.m
	phases := []struct {
		name string
		d    time.Duration
		sum  func(*PutTierStats) *int64
	}{
		{"exists", tm.exists, func(ts *PutTierStats) *int64 { return &ts.ExistsNanos }},
		{"spool_wait", tm.spoolWait, func(ts *PutTierStats) *int64 { return &ts.SpoolWaitNanos }},
		{"receive", tm.receive, func(ts *PutTierStats) *int64 { return &ts.ReceiveNanos }},
		{"blob_check", tm.blobCheck, func(ts *PutTierStats) *int64 { return &ts.BlobCheckNanos }},
		{"upload", tm.upload, func(ts *PutTierStats) *int64 { return &ts.UploadNanos }},
		{"meta", tm.meta, func(ts *PutTierStats) *int64 { return &ts.MetaNanos }},
	}
	if tm.blobBytes > 0 {
		m.blobBytes.WithLabelValues("put", tier).Add(float64(tm.blobBytes))
	}

	s := &h.st
	s.tierMu.Lock()
	defer s.tierMu.Unlock()
	if s.putTiers == nil {
		s.putTiers = map[string]*PutTierStats{}
	}
	ts := s.putTiers[tier]
	if ts == nil {
		ts = new(PutTierStats)
		s.putTiers[tier] = ts
	}
	ts.Puts++
	ts.Nanos += d.Nanoseconds()
	if tm.upload > 0 {
		ts.BlobUploads++
		ts.BlobBytes += tm.blobBytes
	}
	for _, p := range phases {
		if p.d > 0 {
			m.putPhase.WithLabelValues(tier, p.name).Observe(p.d.Seconds())
			*p.sum(ts) += p.d.Nanoseconds()
		}
	}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == "PUT":
		h.servePut(w, r)
	case (r.Method == "GET" || r.Method == "HEAD") && strings.HasPrefix(r.URL.Path, "/action/"):
		h.serveGet(w, r)
	case r.Method == "GET" && r.URL.Path == "/session/stats":
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(h.Stats())
	case r.Method != "GET" && r.Method != "HEAD":
		http.Error(w, "bad method", http.StatusBadRequest)
	default:
		http.Error(w, "not found", http.StatusNotFound)
	}
}

// requestAcceptsEncoding reports whether r's Accept-Encoding lists encoding.
func requestAcceptsEncoding(r *http.Request, encoding string) bool {
	for part := range strings.SplitSeq(r.Header.Get("Accept-Encoding"), ",") {
		name, _, _ := strings.Cut(strings.TrimSpace(part), ";")
		if strings.EqualFold(strings.TrimSpace(name), encoding) {
			return true
		}
	}
	return false
}

func (h *Handler) serveGet(w http.ResponseWriter, r *http.Request) {
	c := h.c
	c.m.activeGets.Inc()
	defer c.m.activeGets.Dec()
	start := c.now()
	wallStart := time.Now()
	storage, typ := "none", "error"
	var tm getTiming
	var transfer time.Duration
	var sent, blobBytes int64
	defer func() {
		d := c.now().Sub(start)
		h.st.lastUsed.Store(c.now().UnixNano())
		h.st.getNanos.Add(d.Nanoseconds())
		c.m.getDuration.WithLabelValues(storage, typ).Observe(d.Seconds())
		if typ == "error" {
			h.st.getErrs.Add(1)
		}
		tier := typ
		if typ == "hit" {
			tier = storage
		}
		h.recordGet(tier, time.Since(wallStart), &tm, transfer, sent, blobBytes)
	}()
	h.st.gets.Add(1)

	actionID := strings.TrimPrefix(r.URL.Path, "/action/")
	if !validHex(actionID) {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if r.Header.Get("Want-Object") != "1" {
		// The old two-request protocol, which nothing we serve uses.
		http.Error(w, "bad request: missing Want-Object header", http.StatusBadRequest)
		return
	}

	g := h.grant(r)
	o, bumped, err := c.get(r.Context(), g, actionID, &tm)
	if err != nil {
		if r.Context().Err() == nil {
			c.logf("awscache: GET %s: %v", actionID, err)
		}
		http.Error(w, "cache lookup error", http.StatusInternalServerError)
		return
	}
	if o == nil {
		typ = "miss"
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	defer o.Close()
	storage, typ = o.storage, "hit"
	h.st.getHits.Add(1)
	if bumped {
		h.st.getAccessBumps.Add(1)
	}
	if o.storage == "inline" {
		h.st.getHitsInline.Add(1)
	}
	c.m.objectBytes.WithLabelValues("get").Observe(float64(o.Size))

	hdr := w.Header()
	hdr.Set("Content-Type", "application/octet-stream")
	hdr.Set("Go-Output-Id", o.OutputID)
	var body io.WriterTo = o
	if zsize, ok := o.LZ4Size(); ok && requestAcceptsEncoding(r, "lz4") {
		hdr.Set("Content-Encoding", "lz4")
		hdr.Set("Content-Length", strconv.FormatInt(zsize, 10))
		hdr.Set("X-Uncompressed-Length", strconv.FormatInt(o.Size, 10))
		body = writerToFunc(o.WriteLZ4To)
	} else {
		hdr.Set("Content-Length", strconv.FormatInt(o.Size, 10))
	}
	if r.Method == "HEAD" {
		return
	}
	transferStart := time.Now()
	n, err := body.WriteTo(w)
	transfer, sent = time.Since(transferStart), n
	if o.blob != nil {
		blobBytes = o.blobSize
	}
	h.st.getBytes.Add(n)
	if err != nil {
		// The headers are already sent. Abort the response, so that the
		// client sees a broken body and discards it.
		if errors.Is(err, errBadBlob) {
			c.m.corruptBlobs.Inc()
			c.logf("awscache: GET %s: %v", actionID, err)
		} else if r.Context().Err() == nil {
			c.logf("awscache: GET %s: writing response: %v", actionID, err)
		}
		typ = "error"
		panic(http.ErrAbortHandler)
	}
}

// writerToFunc is an [io.WriterTo] that calls itself.
type writerToFunc func(io.Writer) (int64, error)

func (f writerToFunc) WriteTo(w io.Writer) (int64, error) { return f(w) }

// PUT body read deadlines, as in gocached: a minimum upload rate plus a
// flat grace, capped.
const (
	putMinUploadRate   = 1 << 20 // bytes/second
	putUploadGrace     = 30 * time.Second
	putMaxBodyReadTime = 10 * time.Minute
)

func putBodyReadDeadline(now time.Time, contentLength int64) time.Time {
	secs := contentLength / putMinUploadRate
	if secs > int64(putMaxBodyReadTime/time.Second) {
		return now.Add(putMaxBodyReadTime)
	}
	return now.Add(min(putUploadGrace+time.Duration(secs)*time.Second, putMaxBodyReadTime))
}

func (h *Handler) servePut(w http.ResponseWriter, r *http.Request) {
	c := h.c
	c.m.activePuts.Inc()
	defer c.m.activePuts.Dec()
	start := c.now()
	wallStart := time.Now()
	storage, typ := "none", "error"
	var tm putTiming
	defer func() {
		d := c.now().Sub(start)
		h.st.lastUsed.Store(c.now().UnixNano())
		h.st.putsNanos.Add(d.Nanoseconds())
		c.m.putDuration.WithLabelValues(storage, typ).Observe(d.Seconds())
		if typ == "error" {
			h.st.putErrs.Add(1)
		}
		tier := storage
		if typ == "error" {
			tier = "error"
		}
		h.recordPut(tier, time.Since(wallStart), &tm)
	}()

	actionID, outputID, ok := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
	if !ok || !validHex(actionID) || !validHex(outputID) {
		http.Error(w, "bad URI", http.StatusBadRequest)
		return
	}
	size := r.ContentLength
	if size < 0 {
		http.Error(w, "missing Content-Length", http.StatusBadRequest)
		return
	}
	h.st.puts.Add(1)

	rc := http.NewResponseController(w)
	rc.SetReadDeadline(putBodyReadDeadline(time.Now(), size))
	defer rc.SetReadDeadline(time.Time{})

	g := h.grant(r)
	res, err := c.put(r.Context(), g, actionID, outputID, size, r.Body, &tm)
	switch {
	case errors.Is(err, ErrReadOnly):
		http.Error(w, "forbidden: read-only", http.StatusForbidden)
		return
	case errors.Is(err, ErrTooLarge):
		http.Error(w, "object too large", http.StatusRequestEntityTooLarge)
		return
	case err != nil:
		if r.Context().Err() == nil {
			c.logf("awscache: PUT %s: %v", actionID, err)
		}
		http.Error(w, "error storing object", http.StatusInternalServerError)
		return
	}
	storage = res.storage
	if res.created {
		typ = "put"
		h.st.putsBytes.Add(size)
		if res.storage == "inline" {
			h.st.putsInline.Add(1)
		}
		c.m.objectBytes.WithLabelValues("put").Observe(float64(size))
	} else {
		typ = "dup"
		h.st.putsDup.Add(1)
		// Consume the body we didn't need, so the connection can be reused.
		io.Copy(io.Discard, io.LimitReader(r.Body, size))
	}
	w.WriteHeader(http.StatusNoContent)
}
