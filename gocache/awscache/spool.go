// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package awscache

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"sync"
)

// spool is a temporary buffer for one object passing through, in memory if
// it fits the cache's memory budget and in a temp file otherwise. It's not
// a cache: its bytes are discarded on Close.
//
// Objects are buffered whole rather than streamed so that PUTs can hash
// content before choosing its S3 key.
type spool struct {
	buf  *bytes.Buffer // non-nil if in memory
	f    *os.File      // non-nil if on disk
	size int64         // bytes written so far

	release func()
}

// newSpool returns a spool for about want bytes. It uses memory if want fits
// the memory budget right now and disk otherwise, waiting (subject to ctx)
// for disk spool capacity if need be. One object larger than the whole disk
// spool capacity is still admitted, alone.
func (c *Cache) newSpool(ctx context.Context, want int64) (*spool, error) {
	want = max(want, 0)
	if c.memSem.TryAcquire(want) {
		c.m.spoolBytes.WithLabelValues("mem").Add(float64(want))
		buf := new(bytes.Buffer)
		buf.Grow(int(want))
		return &spool{
			buf: buf,
			release: sync.OnceFunc(func() {
				c.m.spoolBytes.WithLabelValues("mem").Sub(float64(want))
				c.memSem.Release(want)
			}),
		}, nil
	}
	reserve := min(want, c.cfg.SpoolCap)
	if !c.diskSem.TryAcquire(reserve) {
		c.m.spoolBlocked.Inc()
		if err := c.diskSem.Acquire(ctx, reserve); err != nil {
			return nil, fmt.Errorf("waiting for spool space: %w", err)
		}
	}
	f, err := os.CreateTemp(c.cfg.SpoolDir, "awscache-*")
	if err != nil {
		c.diskSem.Release(reserve)
		return nil, err
	}
	// Unlink it right away (on Unix), so a crash can't leak it.
	unlinked := os.Remove(f.Name()) == nil
	c.m.spoolBytes.WithLabelValues("disk").Add(float64(reserve))
	return &spool{
		f: f,
		release: sync.OnceFunc(func() {
			f.Close()
			if !unlinked {
				os.Remove(f.Name())
			}
			c.m.spoolBytes.WithLabelValues("disk").Sub(float64(reserve))
			c.diskSem.Release(reserve)
		}),
	}, nil
}

func (s *spool) Write(p []byte) (int, error) {
	var n int
	var err error
	if s.buf != nil {
		n, err = s.buf.Write(p)
	} else {
		n, err = s.f.Write(p)
	}
	s.size += int64(n)
	return n, err
}

// Size returns the number of bytes written.
func (s *spool) Size() int64 { return s.size }

// Reader returns a reader of everything written so far, from the start.
// It must not be used concurrently with Write or another Reader.
func (s *spool) Reader() (io.ReadSeeker, error) {
	if s.buf != nil {
		return bytes.NewReader(s.buf.Bytes()), nil
	}
	return io.NewSectionReader(s.f, 0, s.size), nil
}

// Close discards the spool's contents and releases its reservation.
func (s *spool) Close() error {
	s.release()
	return nil
}
