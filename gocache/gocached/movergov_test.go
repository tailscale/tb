package gocached

import (
	"context"
	"errors"
	"math/rand/v2"
	"testing"
	"time"
)

// govSim drives a moverGovernor through synthetic measurement windows
// against a model of the main blob directory's throughput as a function of
// concurrency, without goroutines or real time.
type govSim struct {
	t     *testing.T
	g     *moverGovernor
	now   time.Time
	rng   *rand.Rand
	noise float64 // relative jitter applied to each window's copy count

	// rateAt returns the copies/s the directory delivers at the given
	// concurrency when the slots are kept busy.
	rateAt func(limit int) float64
}

func newGovSim(t *testing.T, minLimit, maxLimit int, rateAt func(int) float64) *govSim {
	t.Helper()
	g := newMoverGovernor(minLimit, maxLimit)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	g.clock = func() time.Time { return now }
	g.windowStart, g.lastAccrue = now, now
	return &govSim{t: t, g: g, now: now, rng: rand.New(rand.NewPCG(1, 2)), rateAt: rateAt}
}

// knee returns a rateAt where each slot delivers perSlot copies/s until the
// directory saturates at ceiling copies/s, i.e. the knee is at
// ceiling/perSlot slots.
func knee(perSlot, ceiling float64) func(int) float64 {
	return func(limit int) float64 {
		return min(float64(limit)*perSlot, ceiling)
	}
}

// step advances one evaluation interval in which the slots were busy for
// the given fraction of the limit's capacity and copies completed at the
// model's rate for that utilization.
func (s *govSim) step(utilization float64) {
	g := s.g
	g.mu.Lock()
	limit := g.limit
	rate := s.rateAt(limit) * utilization
	if s.noise > 0 {
		rate *= 1 + s.noise*(2*s.rng.Float64()-1)
	}
	g.completions += int(rate*moverEvalInterval.Seconds() + 0.5)
	g.busy += time.Duration(float64(limit) * utilization * float64(moverEvalInterval))
	g.mu.Unlock()
	s.now = s.now.Add(moverEvalInterval)
	g.evaluate(s.now)
}

// run advances d of fully busy simulated time.
func (s *govSim) run(d time.Duration) {
	for range int(d / moverEvalInterval) {
		s.step(1)
	}
}

func (s *govSim) limit() int {
	st := s.g.stats()
	return st.limit
}

func (s *govSim) committed() int {
	return s.g.stats().committedLimit
}

func TestMoverGovernorClimbsToKnee(t *testing.T) {
	// 10 copies/s per slot until 400 copies/s: the knee is 40 slots.
	s := newGovSim(t, 8, 256, knee(10, 400))
	s.run(5 * time.Minute)
	st := s.g.stats()
	if st.committedLimit < 32 || st.committedLimit > 52 {
		t.Fatalf("committed limit = %d, want near the knee at 40; stats %+v", st.committedLimit, st)
	}
	if st.committedRate < 350 || st.committedRate > 420 {
		t.Fatalf("committed rate = %.0f, want near the 400 copies/s ceiling", st.committedRate)
	}
	if st.rejected == 0 {
		t.Fatal("never rejected a probe; should be bouncing off the knee")
	}
	// It must have stopped well short of the maximum: extra slots past
	// the knee buy nothing.
	if s.limit() > 64 {
		t.Fatalf("limit = %d, want it held near the knee", s.limit())
	}
}

func TestMoverGovernorReachesMaxWithHeadroom(t *testing.T) {
	s := newGovSim(t, 8, 256, knee(10, 1e9))
	s.run(5 * time.Minute)
	if got := s.committed(); got != 256 {
		t.Fatalf("committed limit = %d, want the max 256 when throughput keeps scaling", got)
	}
}

func TestMoverGovernorStaysAtFloorWhenSaturated(t *testing.T) {
	// Already saturated at the floor: no probe up should stick.
	s := newGovSim(t, 8, 256, knee(10, 60))
	s.run(5 * time.Minute)
	if got := s.committed(); got != 8 {
		t.Fatalf("committed limit = %d, want the floor 8 when more slots add nothing", got)
	}
	if st := s.g.stats(); st.rejected == 0 || st.increases == 0 {
		t.Fatalf("expected repeated rejected upward probes, got %+v", st)
	}
}

func TestMoverGovernorIgnoresDemandLimitedWindows(t *testing.T) {
	s := newGovSim(t, 8, 256, knee(10, 1e9))
	// Slots half idle: throughput is bounded by demand, not the limit.
	for range 300 {
		s.step(0.5)
	}
	if st := s.g.stats(); st.limit != 8 || st.committedRate != 0 {
		t.Fatalf("demand-limited windows moved the governor: %+v", st)
	}
	// Once busy, it climbs.
	s.run(time.Minute)
	if s.committed() <= 8 {
		t.Fatalf("did not climb once busy: %+v", s.g.stats())
	}
}

func TestMoverGovernorIgnoresSparseWindows(t *testing.T) {
	// 1 copy/s is far below moverMinCompletions per window: windows stretch
	// to moverMaxEvalInterval and are then discarded as sparse.
	s := newGovSim(t, 8, 256, func(int) float64 { return 1 })
	s.run(5 * time.Minute)
	if st := s.g.stats(); st.limit != 8 || st.committedRate != 0 || st.increases != 0 {
		t.Fatalf("sparse windows moved the governor: %+v", st)
	}
}

func TestMoverGovernorTrimsWhenDirectorySlows(t *testing.T) {
	ceiling := 2000.0
	s := newGovSim(t, 8, 256, func(limit int) float64 {
		return min(float64(limit)*10, ceiling)
	})
	s.run(5 * time.Minute)
	if got := s.committed(); got < 160 {
		t.Fatalf("committed limit = %d, want near the knee at 200 before the slowdown", got)
	}

	// The directory gets much slower: the knee moves to 30 slots.
	ceiling = 300
	s.run(5 * time.Minute)
	st := s.g.stats()
	if st.committedLimit > 45 {
		t.Fatalf("committed limit = %d after slowdown, want trimmed to near 30; %+v", st.committedLimit, st)
	}
	if st.committedRate > 330 {
		t.Fatalf("committed rate = %.0f after slowdown, want near 300", st.committedRate)
	}
}

func TestMoverGovernorRecoversWhenDirectorySpeedsUp(t *testing.T) {
	ceiling := 300.0
	s := newGovSim(t, 8, 256, func(limit int) float64 {
		return min(float64(limit)*10, ceiling)
	})
	s.run(5 * time.Minute)
	if got := s.committed(); got > 45 {
		t.Fatalf("committed limit = %d, want near 30 before speedup", got)
	}
	ceiling = 2000
	s.run(5 * time.Minute)
	if got := s.committed(); got < 160 {
		t.Fatalf("committed limit = %d after speedup, want climbed back toward 200; %+v", got, s.g.stats())
	}
}

func TestMoverGovernorTracksKneeUnderNoise(t *testing.T) {
	s := newGovSim(t, 8, 256, knee(10, 400))
	s.noise = 0.15
	s.run(10 * time.Minute)
	// Sample the committed limit over a further stretch: it should hover
	// around the knee rather than wander off to either bound.
	lo, hi := 1<<30, 0
	for range 150 {
		s.run(moverEvalInterval * 5)
		c := s.committed()
		lo, hi = min(lo, c), max(hi, c)
	}
	if lo < 20 || hi > 80 {
		t.Fatalf("committed limit wandered to [%d, %d] under noise; want it near the knee at 40", lo, hi)
	}
}

func TestMoverGovernorAcquireRelease(t *testing.T) {
	g := newMoverGovernor(2, 4)
	ctx := context.Background()
	if err := g.acquire(ctx); err != nil {
		t.Fatal(err)
	}
	if err := g.acquire(ctx); err != nil {
		t.Fatal(err)
	}
	if st := g.stats(); st.active != 2 {
		t.Fatalf("active = %d, want 2", st.active)
	}

	// A third acquire blocks at the limit.
	done := make(chan error, 1)
	go func() { done <- g.acquire(ctx) }()
	select {
	case err := <-done:
		t.Fatalf("acquire over limit returned early: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	g.release(true)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("acquire didn't proceed after release")
	}

	// Only successful copies count as completions.
	g.release(false)
	g.release(true)
	g.mu.Lock()
	completions := g.completions
	g.mu.Unlock()
	if completions != 2 {
		t.Fatalf("completions = %d, want 2", completions)
	}

	// Raising the limit admits a waiter without a release.
	for range 2 {
		if err := g.acquire(ctx); err != nil {
			t.Fatal(err)
		}
	}
	go func() { done <- g.acquire(ctx) }()
	select {
	case err := <-done:
		t.Fatalf("acquire over limit returned early: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	g.mu.Lock()
	g.limit = 3
	g.cond.Broadcast()
	g.mu.Unlock()
	if err := <-done; err != nil {
		t.Fatal(err)
	}

	// A blocked acquire aborts when its context is canceled.
	cctx, cancel := context.WithCancel(ctx)
	go func() { done <- g.acquire(cctx) }()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled acquire = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("canceled acquire didn't return")
	}
}

func TestMoverGovernorBusyAccounting(t *testing.T) {
	g := newMoverGovernor(4, 8)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	g.clock = func() time.Time { return now }
	g.windowStart, g.lastAccrue = now, now
	ctx := context.Background()

	// Two slots held for one second, then one of them for another second:
	// three slot-seconds of busy time out of a possible eight.
	g.acquire(ctx)
	g.acquire(ctx)
	now = now.Add(time.Second)
	g.release(true)
	now = now.Add(time.Second)
	g.release(true)
	g.mu.Lock()
	g.accrueLocked(now)
	busy := g.busy
	g.mu.Unlock()
	if busy != 3*time.Second {
		t.Fatalf("busy = %v, want 3s", busy)
	}
}
