// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package awscache

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// metrics are the Prometheus metrics of one [Cache]. They're per Cache, not
// package globals, so a process (or test) can have several; callers register
// them with [Cache.Collectors].
type metrics struct {
	opDuration      *prometheus.HistogramVec // service, op, result
	getDuration     *prometheus.HistogramVec // storage, type
	putDuration     *prometheus.HistogramVec // storage, type
	getPhase        *prometheus.HistogramVec // tier, phase
	putPhase        *prometheus.HistogramVec // tier, phase
	blobBytes       *prometheus.CounterVec   // op, tier
	objectBytes     *prometheus.HistogramVec // op
	activeGets      prometheus.Gauge
	activePuts      prometheus.Gauge
	danglingActions prometheus.Counter
	corruptBlobs    prometheus.Counter
	blobUploads     prometheus.Counter
	blobUploadBytes prometheus.Counter
	blobSkips       *prometheus.CounterVec // how
	spoolBytes      *prometheus.GaugeVec   // where
	spoolBlocked    prometheus.Counter
	atimeQueued     prometheus.Counter
	atimeDropped    prometheus.Counter
	atimeBumps      prometheus.Counter
	atimeErrs       prometheus.Counter
}

var latencyBuckets = []float64{0.001, 0.002, 0.005, 0.01, 0.02, 0.05, 0.1, 0.2, 0.5, 1, 2, 5, 10, 30}

// phaseBuckets are finer than latencyBuckets, to tell apart phases that take
// a few milliseconds, like S3 Express and S3 Standard first bytes. They go
// from 0.2ms to 30s, each about 1.5 times the last.
var phaseBuckets = prometheus.ExponentialBucketsRange(0.0002, 30, 30)

func newMetrics() *metrics {
	return &metrics{
		opDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "gocache_aws_op_duration_seconds",
			Help:    "latency of S3 and DynamoDB calls (to first byte, for S3 GETs), labeled by service (dynamodb, s3, or s3express for a directory bucket), op, and result (ok, miss, cond for a failed condition, or error)",
			Buckets: latencyBuckets,
		}, []string{"service", "op", "result"}),
		getDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "gocache_aws_get_duration_seconds",
			Help:    "wall time of each cache get, labeled by storage (inline, s3, or none) and type (hit, miss, or error)",
			Buckets: latencyBuckets,
		}, []string{"storage", "type"}),
		putDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "gocache_aws_put_duration_seconds",
			Help:    "wall time of each cache put, labeled by storage (inline, s3, dup, or none) and type (put, dup, or error)",
			Buckets: latencyBuckets,
		}, []string{"storage", "type"}),
		getPhase: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "gocache_aws_get_phase_duration_seconds",
			Help:    "time spent in each phase of a cache get served by the HTTP handler, labeled by tier (inline, s3, miss, or error) and phase (lookup: the DynamoDB action lookup; first_byte: the blob GET to its response headers; transfer: sending the response body, which for a blob streams it from S3). Phases that didn't happen aren't observed",
			Buckets: phaseBuckets,
		}, []string{"tier", "phase"}),
		putPhase: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "gocache_aws_put_phase_duration_seconds",
			Help:    "time spent in each phase of a cache put served by the HTTP handler, labeled by tier (inline, s3, dup, or error) and phase (exists: the DynamoDB check for an existing action; spool_wait: waiting for buffer space; receive: reading the body, and hashing and compressing blobs; blob_check: checking whether the blob is already stored; upload: uploading the blob; meta: writing the action item). Phases that didn't happen aren't observed",
			Buckets: phaseBuckets,
		}, []string{"tier", "phase"}),
		blobBytes: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gocache_aws_blob_bytes_total",
			Help: "blob (lz4) bytes read from S3 by gets and uploaded to S3 by puts served by the HTTP handler, labeled by op (get or put) and tier (s3, or error for gets that failed while streaming a blob). With the transfer and upload phases, gives throughput",
		}, []string{"op", "tier"}),
		objectBytes: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "gocache_aws_object_bytes",
			Help: "uncompressed sizes of objects returned by get hits and stored by puts, labeled by op",
			// 64B to 2GiB, doubling.
			Buckets: prometheus.ExponentialBuckets(64, 2, 26),
		}, []string{"op"}),
		activeGets: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "gocache_aws_active_gets",
			Help: "currently pending gets",
		}),
		activePuts: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "gocache_aws_active_puts",
			Help: "currently pending puts",
		}),
		danglingActions: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "gocache_aws_dangling_actions_total",
			Help: "gets whose action referenced a blob missing from S3 (e.g. deleted by the scrubber), served as misses",
		}),
		corruptBlobs: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "gocache_aws_corrupt_blobs_total",
			Help: "gets whose blob in S3 wasn't the size its action said (served as misses), or whose content failed its SHA-256 or lz4 checks while streaming (responses aborted before their last byte); should be zero",
		}),
		blobUploads: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "gocache_aws_blob_uploads_total",
			Help: "blobs uploaded to S3",
		}),
		blobUploadBytes: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "gocache_aws_blob_upload_bytes_total",
			Help: "lz4-compressed bytes uploaded to S3",
		}),
		blobSkips: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gocache_aws_blob_upload_skips_total",
			Help: "puts of blob-sized objects whose blob was already in S3, so not uploaded; labeled by how that was known: known (this process saw it recently) or head (S3 HeadObject)",
		}, []string{"how"}),
		spoolBytes: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "gocache_aws_spool_reserved_bytes",
			Help: "bytes reserved by in-flight puts buffering objects, labeled by where (mem or disk)",
		}, []string{"where"}),
		spoolBlocked: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "gocache_aws_spool_blocked_total",
			Help: "puts that had to wait for disk spool capacity",
		}),
		atimeQueued: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "gocache_aws_atime_bumps_queued_total",
			Help: "access time bumps enqueued",
		}),
		atimeDropped: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "gocache_aws_atime_bumps_dropped_total",
			Help: "access time bumps dropped because the queue was full or the cache was closing",
		}),
		atimeBumps: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "gocache_aws_atime_bumps_total",
			Help: "access time bumps written (including conditional no-ops)",
		}),
		atimeErrs: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "gocache_aws_atime_bump_errors_total",
			Help: "access time bumps that failed",
		}),
	}
}

func (m *metrics) collectors() []prometheus.Collector {
	return []prometheus.Collector{
		m.opDuration, m.getDuration, m.putDuration, m.getPhase, m.putPhase,
		m.blobBytes, m.objectBytes,
		m.activeGets, m.activePuts, m.danglingActions, m.corruptBlobs,
		m.blobUploads, m.blobUploadBytes, m.blobSkips, m.spoolBytes,
		m.spoolBlocked, m.atimeQueued, m.atimeDropped, m.atimeBumps,
		m.atimeErrs,
	}
}

// observeOp records the latency of one AWS call that started at start.
func (m *metrics) observeOp(service, op, result string, start time.Time) {
	m.opDuration.WithLabelValues(service, op, result).Observe(time.Since(start).Seconds())
}
