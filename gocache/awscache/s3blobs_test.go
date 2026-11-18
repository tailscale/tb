// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package awscache

import (
	"context"
	"errors"
	"testing"
)

func TestS3BlobsPrefix(t *testing.T) {
	for _, tt := range []struct {
		name        string
		cachePrefix string
		blobsPrefix string
		wantFound   bool
	}{
		{"empty", "", "", true},
		{"slash", "v1/", "v1/", true},
		{"no_slash", "v1", "v1", true},
		{"nested", "a/b/c/", "a/b/c/", true},
		{"blobs_other", "v1/", "v2/", false},
		{"blobs_empty", "v1/", "", false},
		{"cache_empty", "", "v1/", false},
		{"blobs_shorter", "v1/x/", "v1/", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			env := newTestEnv(t)
			c := env.newCache(func(cfg *Config) { cfg.BlobPrefix = tt.cachePrefix })
			e := env.putBlob(c, "eeee01", randBytes(1000))
			r := newS3Blobs(env.blobs, tt.blobsPrefix, newMetrics())
			rc, size, err := r.Get(context.Background(), e.SHA256)
			if !tt.wantFound {
				if !errors.Is(err, ErrBlobNotFound) {
					t.Fatalf("Get = %v; want ErrBlobNotFound", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			rc.Close()
			if size != e.StoredSize {
				t.Errorf("size = %d; want %d", size, e.StoredSize)
			}
		})
	}
}
