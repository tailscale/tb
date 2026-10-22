// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package awscache

import (
	"context"
	"sync"
	"time"

	"github.com/tailscale/tb/gocache/logger"
	"golang.org/x/time/rate"
)

// atimeBumper is a best-effort background queue of DynamoDB access time
// updates, so that a GET never waits on a write.
//
// Like Linux's relatime, a GET only enqueues a bump when the action's access
// time is older than the relatime threshold (a day by default), so each
// action costs at most about one write per day, and the conditional update
// makes duplicates racing from other hosts cheap no-ops. Each bump also
// pushes the item's DynamoDB TTL out to the access time plus the retention
// period, which is how unused actions expire.
type atimeBumper struct {
	meta      metaStore
	logf      logger.Logf
	m         *metrics
	now       func() time.Time
	relatime  time.Duration
	retention time.Duration
	limiter   *rate.Limiter
	ch        chan string

	mu      sync.Mutex
	pending map[string]bool // keys queued or in flight
}

func newATimeBumper(meta metaStore, logf logger.Logf, m *metrics, now func() time.Time, relatime, retention time.Duration, queueLen int, perSec float64) *atimeBumper {
	return &atimeBumper{
		meta:      meta,
		logf:      logf,
		m:         m,
		now:       now,
		relatime:  relatime,
		retention: retention,
		limiter:   rate.NewLimiter(rate.Limit(perSec), max(1, int(perSec))),
		ch:        make(chan string, queueLen),
		pending:   make(map[string]bool),
	}
}

// maybeEnqueue enqueues a bump of the action at key if its access time
// atime (unix seconds) is older than the relatime threshold. It reports
// whether it enqueued one. It never blocks: when the queue is full, the bump
// is dropped, to be retried by a later GET.
func (b *atimeBumper) maybeEnqueue(key string, atime int64) bool {
	if atime > b.now().Add(-b.relatime).Unix() {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.pending[key] {
		return false
	}
	select {
	case b.ch <- key:
		b.pending[key] = true
		b.m.atimeQueued.Inc()
		return true
	default:
		b.m.atimeDropped.Inc()
		return false
	}
}

// run processes bumps with n workers until ctx is done. Then, until
// drainCtx is done, it keeps going (still rate limited) until the queue is
// empty, so a process exiting with its VM gets its last bumps in.
func (b *atimeBumper) run(ctx, drainCtx context.Context, n int) {
	var wg sync.WaitGroup
	for range n {
		wg.Go(func() {
			for {
				select {
				case <-ctx.Done():
					b.drain(drainCtx)
					return
				case key := <-b.ch:
					// drainCtx outlives ctx, so a bump already dequeued
					// when ctx ends still gets written.
					if err := b.limiter.Wait(drainCtx); err != nil {
						b.m.atimeDropped.Inc()
						b.done(key)
						continue
					}
					b.bump(drainCtx, key)
				}
			}
		})
	}
	wg.Wait()
}

func (b *atimeBumper) drain(ctx context.Context) {
	for {
		select {
		case key := <-b.ch:
			if ctx.Err() != nil || b.limiter.Wait(ctx) != nil {
				b.m.atimeDropped.Inc()
				b.done(key)
				continue
			}
			b.bump(ctx, key)
		default:
			return
		}
	}
}

func (b *atimeBumper) done(key string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.pending, key)
}

func (b *atimeBumper) bump(ctx context.Context, key string) {
	defer b.done(key)
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	now := b.now()
	err := b.meta.BumpAccessTime(ctx, key, now.Unix(), now.Add(b.retention).Unix(), now.Add(-b.relatime).Unix())
	if err != nil {
		b.m.atimeErrs.Inc()
		if ctx.Err() == nil {
			b.logf("awscache: bumping access time of %s: %v", key, err)
		}
		return
	}
	b.m.atimeBumps.Inc()
}
