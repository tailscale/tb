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
// names match gocached's session stats, served at /session/stats.
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
	return st
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
	storage, typ := "none", "error"
	defer func() {
		d := c.now().Sub(start)
		h.st.lastUsed.Store(c.now().UnixNano())
		h.st.getNanos.Add(d.Nanoseconds())
		c.m.getDuration.WithLabelValues(storage, typ).Observe(d.Seconds())
		if typ == "error" {
			h.st.getErrs.Add(1)
		}
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
	o, bumped, err := c.get(r.Context(), g, actionID)
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
	var body io.Reader
	if zr, zsize, ok := o.LZ4(); ok && requestAcceptsEncoding(r, "lz4") {
		hdr.Set("Content-Encoding", "lz4")
		hdr.Set("Content-Length", strconv.FormatInt(zsize, 10))
		hdr.Set("X-Uncompressed-Length", strconv.FormatInt(o.Size, 10))
		body = zr
	} else {
		hdr.Set("Content-Length", strconv.FormatInt(o.Size, 10))
		body, err = o.Reader()
		if err != nil {
			typ = "error"
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
	}
	if r.Method == "HEAD" {
		return
	}
	n, err := io.Copy(w, body)
	h.st.getBytes.Add(n)
	if err != nil && r.Context().Err() == nil {
		// Headers are already sent, so all we can do is cut the response
		// short; the client sees a short body and errors.
		c.logf("awscache: GET %s: writing response: %v", actionID, err)
		typ = "error"
	}
}

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
	storage, typ := "none", "error"
	defer func() {
		d := c.now().Sub(start)
		h.st.lastUsed.Store(c.now().UnixNano())
		h.st.putsNanos.Add(d.Nanoseconds())
		c.m.putDuration.WithLabelValues(storage, typ).Observe(d.Seconds())
		if typ == "error" {
			h.st.putErrs.Add(1)
		}
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
	res, err := c.put(r.Context(), g, actionID, outputID, size, r.Body)
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
