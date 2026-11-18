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

	"github.com/tailscale/tb/gocache/awscache"
)

// TestRealAWS runs a Server against a real DynamoDB table and S3 bucket,
// named by $AWSCACHE_TEST_TABLE and $AWSCACHE_TEST_BUCKET, with credentials
// and region from the environment as usual. It's skipped without them.
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
	writer, err := awscache.New(ctx, awscache.Config{
		Region:     os.Getenv("AWS_REGION"),
		Table:      table,
		Bucket:     bucket,
		BlobPrefix: prefix,
		Logf:       t.Logf,
		Retention:  2 * 24 * time.Hour, // let test items expire soon
	})
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close(context.Background())

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
}
