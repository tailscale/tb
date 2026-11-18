// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package awscache

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/tailscale/tb/rendezvous"
)

// blobServerDownFor defines how long the client should wait before retrying a
// blob server that it previously failed to send a request to. This ensures slow
// connection and retry attempts don't trigger pathological performance for
// GOCACHEPROG, as it sends a high volume of small requests.
const blobServerDownFor = 10 * time.Second

// errBlobServerMiss is the error from [blobPoolClient.get] for a blob that the
// server doesn't have. The server then gets the blob from S3, so that a later
// GET is a hit.
var errBlobServerMiss = errors.New("blob server miss")

// blobPoolClient implements a client for a pre-configured set of BlobServers.
// For each blob, it picks a single server that it expects to find the blob
// from via rendezvous hashing.
type blobPoolClient struct {
	table   rendezvous.Table
	servers map[string]*blobPoolServer
	hc      *http.Client
}

type blobPoolServer struct {
	base      string
	downUntil atomic.Int64
}

func newBlobPoolClient(urls []string) (*blobPoolClient, error) {
	p := &blobPoolClient{
		servers: make(map[string]*blobPoolServer),
		hc: &http.Client{
			Transport: &http.Transport{
				DialContext:           (&net.Dialer{Timeout: time.Second}).DialContext,
				ResponseHeaderTimeout: 5 * time.Second,
				MaxIdleConnsPerHost:   64,
				IdleConnTimeout:       90 * time.Second,
			},
		},
	}
	weights := make(map[string]float64)
	for _, u := range urls {
		u = strings.TrimSuffix(u, "/")
		if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
			return nil, fmt.Errorf("awscache: blob server %q isn't an http or https URL", u)
		}
		if _, dup := p.servers[u]; dup {
			return nil, fmt.Errorf("awscache: duplicate blob server %q", u)
		}
		p.servers[u] = &blobPoolServer{base: u}
		weights[u] = 1
	}
	p.table.Set(weights)
	return p, nil
}

// pick decides which server should be queried for a specific blob's SHA256.
func (p *blobPoolClient) pick(sha string) (*blobPoolServer, bool) {
	u, ok := p.table.Pick(sha)
	if !ok {
		return nil, false
	}
	s := p.servers[u]
	if time.Now().UnixNano() < s.downUntil.Load() {
		return s, false
	}
	return s, true
}

// markDown records that the given server should not be retried until
// the [blobServerDownFor] duration has passed.
func (p *blobPoolClient) markDown(s *blobPoolServer) {
	s.downUntil.Store(time.Now().Add(blobServerDownFor).UnixNano())
}

// get fetches the blob from the given blob pool server.
func (p *blobPoolClient) get(ctx context.Context, s *blobPoolServer, sha string, size int64) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", s.base+"/blob/"+sha+".lz4", nil)
	if err != nil {
		return nil, err
	}
	res, err := p.do(ctx, s, req)
	if err != nil {
		return nil, err
	}
	if res.StatusCode == http.StatusNotFound {
		res.Body.Close()
		return nil, errBlobServerMiss
	}
	if res.StatusCode != http.StatusOK {
		res.Body.Close()
		return nil, fmt.Errorf("%s: %s", s.base, res.Status)
	}
	if res.ContentLength != size {
		res.Body.Close()
		return nil, fmt.Errorf("%s: blob is %d bytes; want %d", s.base, res.ContentLength, size)
	}
	return res.Body, nil
}

// has reports whether the given blob pool server has the blob on disk, with
// the given size.
func (p *blobPoolClient) has(ctx context.Context, s *blobPoolServer, sha string, size int64) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, "HEAD", s.base+"/blob/"+sha+".lz4", nil)
	if err != nil {
		return false, err
	}
	res, err := p.do(ctx, s, req)
	if err != nil {
		return false, err
	}
	res.Body.Close()
	switch res.StatusCode {
	case http.StatusOK:
		return res.ContentLength == size, nil
	case http.StatusNotFound:
		return false, nil
	}
	return false, fmt.Errorf("%s: %s", s.base, res.Status)
}

// warm tells the given blob pool server that the blob is in S3, so that the
// server gets it from S3.
func (p *blobPoolClient) warm(ctx context.Context, s *blobPoolServer, sha string) error {
	req, err := http.NewRequestWithContext(ctx, "POST", s.base+"/blob/"+sha+".lz4", nil)
	if err != nil {
		return err
	}
	res, err := p.do(ctx, s, req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusAccepted {
		return fmt.Errorf("%s: %s", s.base, res.Status)
	}
	return nil
}

func (p *blobPoolClient) do(ctx context.Context, s *blobPoolServer, req *http.Request) (*http.Response, error) {
	res, err := p.hc.Do(req)
	if err != nil && ctx.Err() == nil {
		p.markDown(s)
	}
	return res, err
}
