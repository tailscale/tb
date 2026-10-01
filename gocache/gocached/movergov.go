package gocached

import (
	"context"
	"sync"
	"time"
)

// Defaults and tuning parameters for [moverGovernor].
const (
	// defaultMinPutMovers and defaultMaxPutMovers bound the adaptive number
	// of concurrent spooled-blob copies into the main blob directory. The
	// minimum is the starting point and the floor the governor never trims
	// below; the maximum bounds goroutines, open files, and the concurrency
	// inflicted on the main directory's filesystem.
	defaultMinPutMovers = 8
	defaultMaxPutMovers = 256

	// moverStepFraction is how far a probe moves the limit from the
	// committed operating point, as a fraction of it (at least one slot).
	moverStepFraction = 0.25

	// moverEvalInterval is the shortest measurement window. A window also
	// needs moverMinCompletions copies before the governor acts on it, and
	// stretches up to moverMaxEvalInterval to collect them; a window that
	// ends without them is discarded. 512 completions put the Poisson
	// standard error of the window's rate near 4.4%, and of a comparison
	// between two windows near 6.3%, under the 12.5% gain a probe must
	// show, so a single noisy window rarely flips a decision.
	moverEvalInterval    = 2 * time.Second
	moverMaxEvalInterval = 30 * time.Second
	moverMinCompletions  = 512

	// moverMinUtilization is the fraction of limit*window for which copy
	// slots must have been held for the window to say anything about
	// capacity. Below it, throughput was bounded by demand (an emptying
	// backlog), not by the limit, and the window is discarded.
	moverMinUtilization = 0.9

	// moverGainFraction is the share of the ideal (linear) throughput gain
	// an upward probe must deliver to be kept: a +25% step must yield at
	// least +12.5% copies/s. Latency rising with concurrency is expected
	// and irrelevant; what matters is whether the extra slots did work.
	moverGainFraction = 0.5

	// moverLossTolerance is how much throughput a downward probe may lose
	// and still be kept, meaning the slots given up were buying nothing
	// beyond measurement noise.
	moverLossTolerance = 0.1

	// moverDropFraction is the fall in throughput at the committed limit
	// that triggers an immediate downward probe: the directory got slower,
	// so the committed concurrency may now be excess.
	moverDropFraction = 0.25

	// moverHoldAfterReject is how long the governor sits at the committed
	// limit after a probe is rejected before probing again.
	moverHoldAfterReject = 30 * time.Second

	// moverDownProbeEvery makes every Nth probe from steady state a
	// downward one, so a limit that drifted above the knee (through a
	// noisy accepted probe) gets trimmed even if throughput never drops.
	moverDownProbeEvery = 4
)

// moverGovernor adaptively sets how many spooled-blob copies into the main
// blob directory may run concurrently, by hill-climbing on measured copy
// throughput.
//
// The main directory may be a network filesystem whose throughput for the
// small, round-trip-bound copies that dominate the put queue scales with
// concurrency until the filesystem saturates. Per-copy latency grows with
// concurrency well before that point, so latency is not the signal;
// throughput is. The governor holds a committed operating point (a limit
// and the copies/s measured there) and probes: it raises the limit by a
// step and keeps the raise only if copies/s rose by a meaningful fraction
// of the ideal linear gain, and it lowers the limit when a trial at fewer
// slots delivers the same throughput. Windows in which the slots weren't
// kept busy (demand-limited) teach it nothing and are discarded.
type moverGovernor struct {
	minLimit int
	maxLimit int
	clock    func() time.Time

	mu     sync.Mutex
	cond   *sync.Cond // signaled when a slot frees or the limit grows
	limit  int        // current maximum concurrent copies
	active int        // copies currently running

	// Current measurement window.
	windowStart time.Time
	lastAccrue  time.Time
	busy        time.Duration // integral of active over the window
	completions int           // successful copies finished in the window

	// Committed operating point and probe state.
	committedLimit int
	committedRate  float64 // copies/s measured at committedLimit; 0 until known
	holdUntil      time.Time
	probes         int // steady-state probes started, for alternating direction

	increases, decreases, rejected int // for metrics
}

func newMoverGovernor(minLimit, maxLimit int) *moverGovernor {
	if minLimit <= 0 {
		minLimit = defaultMinPutMovers
	}
	if maxLimit < minLimit {
		maxLimit = max(minLimit, defaultMaxPutMovers)
	}
	g := &moverGovernor{
		minLimit:       minLimit,
		maxLimit:       maxLimit,
		clock:          time.Now,
		limit:          minLimit,
		committedLimit: minLimit,
	}
	g.cond = sync.NewCond(&g.mu)
	now := g.clock()
	g.windowStart, g.lastAccrue = now, now
	return g
}

// accrueLocked advances the busy integral to now. It must be called before
// any change to active or limit, and the caller must hold g.mu.
func (g *moverGovernor) accrueLocked(now time.Time) {
	if now.After(g.lastAccrue) {
		g.busy += time.Duration(g.active) * now.Sub(g.lastAccrue)
		g.lastAccrue = now
	}
}

// acquire blocks until a copy slot is available under the current limit or
// ctx is done. Each successful acquire must be paired with a release.
func (g *moverGovernor) acquire(ctx context.Context) error {
	// Wake waiters on shutdown, since cond.Wait can't select on ctx.
	stop := context.AfterFunc(ctx, func() {
		g.mu.Lock()
		g.cond.Broadcast()
		g.mu.Unlock()
	})
	defer stop()

	g.mu.Lock()
	defer g.mu.Unlock()
	for g.active >= g.limit {
		if err := ctx.Err(); err != nil {
			return err
		}
		g.cond.Wait()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	g.accrueLocked(g.clock())
	g.active++
	return nil
}

// release returns a slot taken by acquire. completed reports whether the
// copy succeeded; only successful copies count toward throughput.
func (g *moverGovernor) release(completed bool) {
	g.mu.Lock()
	g.accrueLocked(g.clock())
	g.active--
	if completed {
		g.completions++
	}
	g.mu.Unlock()
	g.cond.Signal()
}

// evaluate closes the measurement window if it is ready and moves the
// limit according to what it showed. It is called periodically by run, and
// directly by tests.
func (g *moverGovernor) evaluate(now time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.accrueLocked(now)

	elapsed := now.Sub(g.windowStart)
	if elapsed < moverEvalInterval {
		return
	}
	sparse := g.completions < moverMinCompletions
	if sparse && elapsed < moverMaxEvalInterval {
		return
	}
	rate := float64(g.completions) / elapsed.Seconds()
	util := g.busy.Seconds() / (float64(g.limit) * elapsed.Seconds())
	g.windowStart, g.busy, g.completions = now, 0, 0

	if sparse || util < moverMinUtilization {
		// Demand-limited: the limit wasn't what bounded throughput, so
		// the window says nothing about capacity. Any probe in flight
		// stays in flight until a busy window judges it.
		return
	}

	switch {
	case g.committedRate == 0:
		// First busy window: this is the starting operating point.
		g.commitLocked(rate)
		g.probeUpLocked()

	case g.limit > g.committedLimit:
		ideal := float64(g.limit)/float64(g.committedLimit) - 1
		if rate/g.committedRate-1 >= moverGainFraction*ideal {
			g.commitLocked(rate)
			g.probeUpLocked()
		} else {
			g.rejectLocked(now)
		}

	case g.limit < g.committedLimit:
		if rate >= g.committedRate*(1-moverLossTolerance) {
			g.commitLocked(rate)
			g.probeDownLocked()
		} else {
			g.rejectLocked(now)
		}

	default:
		if rate < g.committedRate*(1-moverDropFraction) {
			// Throughput fell at a limit that used to deliver more. Take
			// the new rate as the truth and test whether the concurrency
			// is now excess.
			g.committedRate = rate
			g.probeDownLocked()
			return
		}
		g.committedRate = (g.committedRate + rate) / 2
		if now.Before(g.holdUntil) {
			return
		}
		g.probes++
		if g.probes%moverDownProbeEvery == 0 {
			g.probeDownLocked()
		} else {
			g.probeUpLocked()
		}
	}
}

// commitLocked records the current limit and the rate measured at it as the
// operating point to compare future probes against.
func (g *moverGovernor) commitLocked(rate float64) {
	g.committedLimit = g.limit
	g.committedRate = rate
}

func (g *moverGovernor) stepLocked() int {
	return max(1, int(float64(g.committedLimit)*moverStepFraction))
}

// probeUpLocked raises the limit one step above the committed point, if the
// maximum allows.
func (g *moverGovernor) probeUpLocked() {
	n := min(g.maxLimit, g.committedLimit+g.stepLocked())
	if n > g.limit {
		g.limit = n
		g.increases++
		g.cond.Broadcast()
	}
}

// probeDownLocked lowers the limit one step below the committed point, if
// the minimum allows.
func (g *moverGovernor) probeDownLocked() {
	n := max(g.minLimit, g.committedLimit-g.stepLocked())
	if n < g.limit {
		g.limit = n
		g.decreases++
	}
}

// rejectLocked abandons the probe in flight, returning to the committed
// limit, and holds there for a while before probing again.
func (g *moverGovernor) rejectLocked(now time.Time) {
	g.rejected++
	g.holdUntil = now.Add(moverHoldAfterReject)
	switch {
	case g.limit > g.committedLimit:
		g.limit = g.committedLimit
		g.decreases++
	case g.limit < g.committedLimit:
		g.limit = g.committedLimit
		g.increases++
		g.cond.Broadcast()
	}
}

// run calls evaluate periodically until ctx is done.
func (g *moverGovernor) run(ctx context.Context) {
	t := time.NewTicker(moverEvalInterval / 2)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			g.evaluate(now)
		}
	}
}

// moverStats is a snapshot of the governor's state for metrics.
type moverStats struct {
	limit, active        int
	committedLimit       int
	committedRate        float64
	increases, decreases int
	rejected             int
}

func (g *moverGovernor) stats() moverStats {
	g.mu.Lock()
	defer g.mu.Unlock()
	return moverStats{
		limit:          g.limit,
		active:         g.active,
		committedLimit: g.committedLimit,
		committedRate:  g.committedRate,
		increases:      g.increases,
		decreases:      g.decreases,
		rejected:       g.rejected,
	}
}
