// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package awscache

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestBlobPoolFallback(t *testing.T) {
	env := newTestEnv(t)
	writer := env.newCache(nil)
	var ids []string
	objs := map[string][]byte{}
	for i := range 20 {
		id := fmt.Sprintf("dddd%02d", i)
		ids = append(ids, id)
		objs[id] = bytes.Repeat([]byte(id), 200)
		env.putBlob(writer, id, objs[id])
	}

	corrupt := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sha := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/blob/"), ".lz4")
		b := env.blobs.blobs[blobKey("pfx/", sha)]
		w.Header().Set("Content-Length", strconv.Itoa(len(b)))
		w.Write(make([]byte, len(b)))
	}))
	t.Cleanup(corrupt.Close)
	gone := httptest.NewServer(http.NotFoundHandler())
	gone.Close()

	c := env.newCache(func(cfg *Config) { cfg.BlobServers = []string{corrupt.URL, gone.URL} })
	cl := env.newClient(c, Grant{})
	for _, id := range ids {
		cl.wantHit(id, "0b"+id, objs[id])
	}
	for _, id := range ids {
		cl.wantHit(id, "0b"+id, objs[id])
	}

	fb := func(reason string) float64 { return testutil.ToFloat64(c.m.poolFallbacks.WithLabelValues(reason)) }
	if fb("corrupt") == 0 {
		t.Error("no corrupt fallbacks")
	}
	if fb("error") != 1 {
		t.Errorf("error fallbacks = %v; want 1, then the server is down", fb("error"))
	}
	if fb("down") == 0 {
		t.Error("no down fallbacks")
	}
}

func TestBlobPoolDown(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p, err := newBlobPoolClient([]string{"http://a"})
		if err != nil {
			t.Fatal(err)
		}
		const sha = "ab"
		s, ok := p.pick(sha)
		if !ok {
			t.Fatal("server down at start")
		}
		p.markDown(s)
		time.Sleep(blobServerDownFor - time.Nanosecond)
		if _, ok := p.pick(sha); ok {
			t.Fatal("server up before the down period ends")
		}
		time.Sleep(time.Nanosecond)
		if _, ok := p.pick(sha); !ok {
			t.Fatal("server down after the down period ends")
		}
	})
}

func TestBlobPoolBadConfig(t *testing.T) {
	for _, urls := range [][]string{
		{"10.0.0.1:8080"},
		{"http://a", "http://a/"},
	} {
		if _, err := newBlobPoolClient(urls); err == nil {
			t.Errorf("newBlobPool(%q) succeeded", urls)
		}
	}
}

func TestBlobPoolHEADBeforeUpload(t *testing.T) {
	env := newTestEnv(t)
	skips := func(c *Cache, how string) float64 {
		return testutil.ToFloat64(c.m.blobSkips.WithLabelValues(how))
	}
	data := randBytes(1000)
	env.putBlob(env.newCache(nil), "abab01", data)

	// If the server can't say that it has the blob, the PUT uses S3 as
	// usual. The blob is already in S3, so the PUT doesn't upload it.
	newServer := func(h http.HandlerFunc) string {
		ts := httptest.NewServer(h)
		t.Cleanup(ts.Close)
		return ts.URL
	}
	gone := httptest.NewServer(http.NotFoundHandler())
	gone.Close()
	for i, tt := range []struct {
		name string
		url  string
	}{
		{"down", gone.URL},
		{"error", newServer(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "error", http.StatusInternalServerError)
		})},
		{"wrong_size", newServer(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Length", "1")
		})},
	} {
		c := env.newCache(func(cfg *Config) { cfg.BlobServers = []string{tt.url} })
		puts0, heads0, _ := env.blobs.counts()
		env.putBlob(c, fmt.Sprintf("abab1%d", i), data)
		if puts, heads, _ := env.blobs.counts(); puts != puts0 || heads != heads0+1 {
			t.Errorf("%s: S3 puts, heads = %d, %d; want 0, 1", tt.name, puts-puts0, heads-heads0)
		}
		if skips(c, "head") != 1 {
			t.Errorf("%s: head skips = %v; want 1", tt.name, skips(c, "head"))
		}
	}
}
