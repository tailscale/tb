// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package blobserver

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/tailscale/tb/gocache/awscache"
)

// TestRealAWS runs a Server and a client against a real DynamoDB table and
// S3 bucket, named by $AWSCACHE_TEST_TABLE and $AWSCACHE_TEST_BUCKET, with
// credentials and region from the environment as usual. It's skipped without
// them.
func TestRealAWS(t *testing.T) {
	table, bucket := os.Getenv("AWSCACHE_TEST_TABLE"), os.Getenv("AWSCACHE_TEST_BUCKET")
	if table == "" || bucket == "" {
		t.Skip("AWSCACHE_TEST_TABLE and AWSCACHE_TEST_BUCKET not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	const prefix = "blobserver-test/"
	newServer := func() (*Server, string) {
		s, err := New(ctx, bucket, t.TempDir(), 1<<30,
			WithLogf(t.Logf),
			WithBlobPrefix(prefix),
		)
		if err != nil {
			t.Fatal(err)
		}
		select {
		case <-s.loaded:
		case <-time.After(5 * time.Second):
			t.Fatal("timeout waiting for the server to find the blobs on disk")
		}
		ts := httptest.NewServer(s)
		t.Cleanup(ts.Close)
		return s, ts.URL
	}
	waitFor := func(what string, cond func() bool) {
		t.Helper()
		for !cond() {
			if ctx.Err() != nil {
				t.Fatalf("timeout waiting for %s", what)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	s, url := newServer()
	newCache := func(blobServers []string) *awscache.Cache {
		c, err := awscache.New(ctx, awscache.Config{
			Region:      os.Getenv("AWS_REGION"),
			Table:       table,
			Bucket:      bucket,
			BlobPrefix:  prefix,
			Logf:        t.Logf,
			Retention:   2 * 24 * time.Hour, // let test items expire soon
			BlobServers: blobServers,
		})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { c.Close(context.Background()) })
		return c
	}
	writer := newCache(nil)
	reader := newCache([]string{url})

	ns := awscache.Namespace("blobserver-test")
	g := awscache.Grant{
		CanWrite:           true,
		WriteNamespace:     ns,
		ExtraReadNamespace: ns,
	}
	data := randBytes(1 << 20)
	h := sha256.Sum256(data)
	sha := hex.EncodeToString(h[:])
	action := hex.EncodeToString(randBytes(32))
	if err := writer.Put(ctx, g, action, "abcd", int64(len(data)), bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}

	// The first GET misses, and the server then fills the blob from S3.
	get := func(sha string) int {
		res, err := http.Get(url + "/blob/" + sha + ".lz4")
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		if _, err := io.Copy(io.Discard, res.Body); err != nil {
			t.Fatal(err)
		}
		return res.StatusCode
	}
	if code := get(sha); code != http.StatusNotFound {
		t.Fatalf("first GET = %d; want 404", code)
	}
	waitFor("fill from S3", func() bool { return fills(s, "stored") == 1 })
	if code := get(sha); code != http.StatusOK {
		t.Fatalf("GET after fill = %d; want 200", code)
	}

	// A miss that isn't in S3.
	missing := strings.Repeat("0", 64)
	if code := get(missing); code != http.StatusNotFound {
		t.Fatalf("GET of missing blob = %d; want 404", code)
	}
	waitFor("notfound fill", func() bool { return fills(s, "notfound") == 1 })

	// A client with the server in its pool gets the blob from the server.
	hits := func(method string) float64 { return testutil.ToFloat64(s.m.requests.WithLabelValues(method, "hit")) }
	getFromServer := func(action string, want []byte) {
		t.Helper()
		hits0 := hits("GET")
		o, err := reader.Get(ctx, g, action)
		if err != nil || o == nil {
			t.Fatalf("Get through Server = %v, %v", o, err)
		}
		r, err := o.Reader()
		if err != nil {
			t.Fatal(err)
		}
		got, err := io.ReadAll(r)
		o.Close()
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("Get through Server: got %d bytes, err %v", len(got), err)
		}
		if got := hits("GET"); got != hits0+1 {
			t.Errorf("Server hits = %v; want %v", got, hits0+1)
		}
	}
	getFromServer(action, data)

	// A client with the server in its pool tells the server about a new
	// blob after it uploads it, so the server fills it from S3, and the
	// first GET of it is a hit.
	data = randBytes(1 << 20)
	action = hex.EncodeToString(randBytes(32))
	if err := reader.Put(ctx, g, action, "abcd", int64(len(data)), bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	waitFor("fill after POST", func() bool { return fills(s, "stored") == 2 })
	getFromServer(action, data)

	// A PUT on another host of an action with the same blob finds the blob
	// on the server with a HEAD, so it doesn't upload the blob again.
	hits0 := hits("HEAD")
	other := newCache([]string{url})
	if err := other.Put(ctx, g, hex.EncodeToString(randBytes(32)), "abcd", int64(len(data)), bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	if got := hits("HEAD"); got != hits0+1 {
		t.Errorf("Server hits for HEAD = %v; want %v", got, hits0+1)
	}
	if got := testutil.ToFloat64(s.m.warms.WithLabelValues("started")); got != 1 {
		t.Errorf("started warms = %v; want 1", got)
	}

	// Two servers in a pool each hold a share of the blobs. A client fills
	// each blob into one server, and the next client gets it from there.
	s1, url1 := newServer()
	s2, url2 := newServer()
	var actions []string
	blobs := map[string][]byte{}
	for range 20 {
		action := hex.EncodeToString(randBytes(32))
		actions = append(actions, action)
		blobs[action] = randBytes(1 << 10)
		if err := writer.Put(ctx, g, action, "abcd", 1<<10, bytes.NewReader(blobs[action])); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range []*awscache.Cache{newCache([]string{url1, url2}), newCache([]string{url1, url2})} {
		for _, action := range actions {
			o, err := c.Get(ctx, g, action)
			if err != nil || o == nil {
				t.Fatalf("Get through pool = %v, %v", o, err)
			}
			o.Close()
		}
		waitFor("fills", func() bool { return fills(s1, "stored")+fills(s2, "stored") == float64(len(actions)) })
	}
	for i, s := range []*Server{s1, s2} {
		stored, hits := fills(s, "stored"), testutil.ToFloat64(s.m.requests.WithLabelValues("GET", "hit"))
		if stored == 0 || stored == float64(len(actions)) || hits != stored {
			t.Errorf("server %d: %v fills, %v hits of %d blobs; want a share, with a hit for each fill", i, stored, hits, len(actions))
		}
	}
}
