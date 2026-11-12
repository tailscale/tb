// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package awscache

// This file has TestBuildBench's support for gocached: a transport that
// gives each benchmark run its own empty cache in a gocached server, and a
// blobStore that keeps blobs in a gocached server, to simulate the NVMe
// cache pod design.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/tailscale/tb/gocache/gocached"
)

// saltTransport is an [http.RoundTripper] that replaces the action ID in
// each gocached request (GET or HEAD /action/<actionID>, and PUT
// /<actionID>/<outputID>) with a hash of the salt and the action ID. Thus
// each salt sees its own empty cache, in a gocached server that has only
// one namespace (without auth).
type saltTransport struct {
	salt string
	rt   http.RoundTripper
}

func (st saltTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	path := req.URL.Path
	switch {
	case strings.HasPrefix(path, "/action/"):
		path = "/action/" + st.saltID(strings.TrimPrefix(path, "/action/"))
	case req.Method == "PUT":
		actionID, outputID, ok := strings.Cut(strings.TrimPrefix(path, "/"), "/")
		if ok {
			path = "/" + st.saltID(actionID) + "/" + outputID
		}
	}
	req = req.Clone(req.Context())
	req.URL.Path, req.URL.RawPath = path, ""
	return st.rt.RoundTrip(req)
}

func (st saltTransport) saltID(actionID string) string {
	h := sha256.Sum256([]byte(st.salt + ":" + actionID))
	return hex.EncodeToString(h[:])
}

// podBlobStore is a [blobStore] that keeps blobs in a gocached server, in
// front of an S3 bucket, to simulate the NVMe cache pod design (see "Blob
// storage" in Tailscale's corp ci/doc/aws-native-gocached.md) with one pod:
//
//   - Put writes the blob to S3 and then to the pod, as a client that warms
//     the pod with the blobs it writes would.
//   - Exists asks the pod, and then S3.
//   - Get reads from the pod, and falls back to S3 if the pod doesn't have
//     the blob. (A real pod would fetch from S3 itself, and keep the blob.)
//
// It uses gocached's action protocol: a blob is stored as the object of an
// action whose ID (and output ID) is the SHA-256 of the blob's key. The key
// includes the Cache's BlobPrefix, so each benchmark run has its own blobs.
// gocached may store blobs lz4-compressed again; it decompresses them for
// clients that don't accept lz4, as podBlobStore doesn't.
type podBlobStore struct {
	base string // gocached's base URL, without a trailing slash
	hc   *http.Client
	s3   blobStore
}

func newPodBlobStore(baseURL string, s3 blobStore) *podBlobStore {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.MaxIdleConns = 512
	tr.MaxIdleConnsPerHost = 256
	tr.IdleConnTimeout = 90 * time.Second
	return &podBlobStore{
		base: strings.TrimSuffix(baseURL, "/"),
		hc:   &http.Client{Transport: tr},
		s3:   s3,
	}
}

var _ blobStore = (*podBlobStore)(nil)

// podID returns the gocached action and output ID of the blob at key.
func podID(key string) string {
	h := sha256.Sum256([]byte(key))
	return hex.EncodeToString(h[:])
}

// do sends a request for the blob at key to the pod.
func (p *podBlobStore) do(ctx context.Context, method, key string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, p.base+"/action/"+podID(key), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Want-Object", "1")
	// Not lz4 (blobs are lz4 frames already), nor gzip.
	req.Header.Set("Accept-Encoding", "identity")
	return p.hc.Do(req)
}

func (p *podBlobStore) Exists(ctx context.Context, key string) (bool, error) {
	res, err := p.do(ctx, "HEAD", key)
	if err != nil {
		return false, err
	}
	res.Body.Close()
	switch res.StatusCode {
	case http.StatusOK:
		return true, nil
	case http.StatusNotFound:
		return p.s3.Exists(ctx, key)
	}
	return false, fmt.Errorf("pod HEAD: %s", res.Status)
}

func (p *podBlobStore) Put(ctx context.Context, key string, r io.ReadSeeker, size int64) error {
	if err := p.s3.Put(ctx, key, r, size); err != nil {
		return err
	}
	if _, err := r.Seek(0, io.SeekStart); err != nil {
		return err
	}
	id := podID(key)
	req, err := http.NewRequestWithContext(ctx, "PUT", p.base+"/"+id+"/"+id, io.NopCloser(r))
	if err != nil {
		return err
	}
	req.ContentLength = size
	req.Header.Set("Content-Length", strconv.FormatInt(size, 10))
	res, err := p.hc.Do(req)
	if err != nil {
		return fmt.Errorf("pod PUT: %w", err)
	}
	io.Copy(io.Discard, res.Body)
	res.Body.Close()
	if res.StatusCode/100 != 2 {
		return fmt.Errorf("pod PUT: %s", res.Status)
	}
	return nil
}

func (p *podBlobStore) Get(ctx context.Context, key string) (io.ReadCloser, int64, error) {
	res, err := p.do(ctx, "GET", key)
	if err != nil {
		return nil, 0, err
	}
	switch res.StatusCode {
	case http.StatusOK:
		if res.ContentLength < 0 {
			res.Body.Close()
			return nil, 0, fmt.Errorf("pod GET: no Content-Length")
		}
		return res.Body, res.ContentLength, nil
	case http.StatusNotFound:
		res.Body.Close()
		return p.s3.Get(ctx, key)
	}
	res.Body.Close()
	return nil, 0, fmt.Errorf("pod GET: %s", res.Status)
}

// TestBuildBenchFakeGocached runs TestBuildBench's gocached and ddbgocached
// configs against a gocached server in this process, with the in-memory
// metadata and S3 stores, building a tiny program.
func TestBuildBenchFakeGocached(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping builds in short mode")
	}
	dir := t.TempDir()
	for name, data := range map[string]string{
		"go.mod":  "module example.com/hello\n",
		"main.go": "package main\n\nfunc main() { println(\"hello\") }\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0644); err != nil {
			t.Fatal(err)
		}
	}
	srv, err := gocached.NewServer(gocached.WithDir(t.TempDir()), gocached.WithLogf(t.Logf))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Close() })
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)

	env := newTestEnv(t)
	b := newBuildBencher(t, func(ctx context.Context, cfg benchConfig, gen int) (*Cache, error) {
		if !cfg.pod {
			t.Fatalf("unexpected config %s", cfg.name)
		}
		return New(ctx, Config{
			BlobPrefix:       fmt.Sprintf("buildbench/%d/", gen),
			GlobalGeneration: gen,
			SpoolDir:         t.TempDir(),
			Logf:             t.Logf,
			meta:             env.meta,
			blobs:            newPodBlobStore(ts.URL, env.blobs),
		})
	})
	b.gocachedURL = ts.URL
	var gocachedCfg, podCfg benchConfig
	for _, cfg := range benchConfigs {
		switch {
		case cfg.gocached:
			gocachedCfg = cfg
		case cfg.pod:
			podCfg = cfg
		}
	}
	bc := benchCase{name: "hello", dir: dir, args: []string{"build", "-o", os.DevNull, "."}}
	b.runCase(bc, []benchConfig{gocachedCfg, podCfg}, 1) // with the default flags
	b.printSummary()

	byRun := map[string]benchResult{}
	for _, r := range b.results {
		byRun[r.Config+"/"+r.Run] = r
	}
	for _, name := range []string{"gocached/cold", "gocached/warm1", "gocached/diskwarm", "ddbgocached/cold", "ddbgocached/warm1"} {
		if _, ok := byRun[name]; !ok {
			t.Fatalf("no %s build; got %v", name, b.results)
		}
	}
	cold, warm := byRun["gocached/cold"], byRun["gocached/warm1"]
	if cold.RemoteHits != 0 || warm.RemoteHits == 0 || warm.misses()*10 > cold.misses() {
		t.Errorf("gocached: cold %d hits, %d misses; warm %d hits, %d misses", cold.RemoteHits, cold.misses(), warm.RemoteHits, warm.misses())
	}

	// A new generation sees an empty cache in the same gocached server.
	if r := b.run(bc, gocachedCfg, 2, "cold2", runDirs{}); true {
		r.remove()
		if got := b.results[len(b.results)-1]; got.RemoteHits != 0 {
			t.Errorf("gocached cold build in a new generation got %d remote hits; want 0", got.RemoteHits)
		}
	}

	podWarm := byRun["ddbgocached/warm1"]
	if podWarm.getTier("s3").Gets == 0 || podWarm.getPhase("s3", "first_byte").N == 0 {
		t.Errorf("ddbgocached warm build: tiers %v; want blob hits", podWarm.Stats.GetTiers)
	}
	puts, _, gets := env.blobs.counts()
	if puts == 0 || gets != 0 {
		t.Errorf("S3 behind the pod: %d puts, %d gets; want puts (write-through) and no gets (all from the pod)", puts, gets)
	}
}
