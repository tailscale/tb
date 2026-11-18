// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package awscache

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/prometheus/client_golang/prometheus"
)

// S3Blobs is the store of the lz4 blobs of a [Cache] in S3. A Cache reads
// and writes blobs through it, and a blob server reads blobs through it. Use
// [NewS3Blobs] to make one for a blob server.
type S3Blobs struct {
	store  blobStore
	prefix string
	m      *metrics
}

// NewS3Blobs makes an [S3Blobs] for the blobs in bucket under prefix. They
// must be the same as the [Config.Bucket] and [Config.BlobPrefix] of the
// Caches. If awsCfg is nil, NewS3Blobs loads the default AWS config, with
// the region from EC2 instance metadata.
func NewS3Blobs(ctx context.Context, bucket, prefix string, awsCfg *aws.Config) (*S3Blobs, error) {
	if bucket == "" {
		return nil, errors.New("awscache: S3Blobs needs a bucket")
	}
	if awsCfg == nil {
		cfg, err := loadAWSConfig(ctx, "")
		if err != nil {
			return nil, fmt.Errorf("awscache: loading AWS config: %w", err)
		}
		awsCfg = &cfg
	}
	m := newMetrics()
	store := &s3Store{
		c:      s3.NewFromConfig(*awsCfg),
		bucket: bucket,
		m:      m,
	}
	return newS3Blobs(store, prefix, m), nil
}

// newS3Blobs makes an [S3Blobs] for the blobs in store under prefix. m must
// be the metrics that store records to.
func newS3Blobs(store blobStore, prefix string, m *metrics) *S3Blobs {
	return &S3Blobs{
		store:  store,
		prefix: prefix,
		m:      m,
	}
}

// Get opens the lz4 blob whose uncompressed content has the lowercase SHA-256
// hex sha, and returns the size of the blob. It returns [ErrBlobNotFound] if
// the blob isn't in S3.
func (b *S3Blobs) Get(ctx context.Context, sha string) (io.ReadCloser, int64, error) {
	return b.store.Get(ctx, blobKey(b.prefix, sha))
}

// exists reports whether the blob sha is in S3.
func (b *S3Blobs) exists(ctx context.Context, sha string) (bool, error) {
	return b.store.Exists(ctx, blobKey(b.prefix, sha))
}

// put stores the size-byte blob sha from r in S3. It isn't an error if the
// blob is already in S3.
func (b *S3Blobs) put(ctx context.Context, sha string, r io.ReadSeeker, size int64) error {
	return b.store.Put(ctx, blobKey(b.prefix, sha), r, size)
}

// Collectors returns the Prometheus metrics of the S3Blobs, for the caller
// to register. They have the same names as metrics of a [Cache], so the
// caller can't register both in one registry. A Cache includes them in its
// own [Cache.Collectors].
func (b *S3Blobs) Collectors() []prometheus.Collector {
	return []prometheus.Collector{
		b.m.opDuration,
	}
}
