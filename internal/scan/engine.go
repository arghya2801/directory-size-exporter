// Package scan walks target directories and produces measurements for internal/state.
//
// The engine is built around three constraints that a naive recursive walk does not satisfy:
//
//   - It must not disturb the application sharing the host. Concurrency defaults low, work is
//     rate-limitable, and the walk reads only metadata, never file contents.
//   - It must survive a hung mount. Filesystem syscalls on a dead NFS server are uninterruptible,
//     so no amount of context plumbing can cancel them; the engine gives up on the target instead
//     and keeps running.
//   - It must never report a partial measurement as a complete one. Every path that ends a scan
//     early produces an outcome that internal/state refuses to publish.
package scan

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/time/rate"

	"github.com/local/directory-size-exporter/internal/fsstat"
	"github.com/local/directory-size-exporter/internal/state"
)

// Config tunes the engine. The zero value is usable; withDefaults fills in gentle defaults chosen
// to stay out of a latency-sensitive application's way rather than to finish quickly.
type Config struct {
	// Concurrency caps directories being read simultaneously ACROSS ALL TARGETS. A single global
	// pool, rather than one per target, is what makes this an honest bound on filesystem pressure:
	// per-target pools would multiply load by the number of configured targets, so a host with
	// twelve targets and a limit of four would issue forty-eight concurrent walks.
	Concurrency int
	// BatchSize is how many entries are read per directory read.
	BatchSize int
	// ShareThreshold is the local stack depth past which a worker donates work to the shared queue.
	ShareThreshold int
	// QueueLimit caps the shared queue. Reaching it is harmless: the worker simply keeps the work
	// locally, which is what makes the design deadlock-free.
	QueueLimit int

	// RateLimit caps filesystem operations per second across all workers. Zero is unlimited.
	RateLimit float64
	// RateLimitBurst must be at least BatchSize+1, since a whole batch is acquired at once.
	RateLimitBurst int

	// ScanTimeout bounds one target's walk. Zero disables it.
	ScanTimeout time.Duration
	// HardTimeout is the point at which a target is abandoned without waiting for its workers,
	// because they are presumed stuck in an uninterruptible syscall. Zero disables it.
	HardTimeout time.Duration
	// WorkerDrainTimeout is how long to wait for workers to exit before declaring them leaked.
	WorkerDrainTimeout time.Duration

	OneFilesystem    bool
	DedupHardlinks   bool
	MaxTrackedInodes int

	// HeartbeatAfter is how long a target's walk must run before progress logging starts, and
	// HeartbeatEvery is the interval between those lines. Without them a multi-minute walk is
	// indistinguishable from a hang.
	HeartbeatAfter time.Duration
	HeartbeatEvery time.Duration

	// WorkerInit runs once on each worker goroutine before it takes any work, on the goroutine
	// itself. It exists so scheduling priority can be applied per thread: the setting is a
	// property of the OS thread, so it cannot be applied from the goroutine that starts the pool.
	WorkerInit func() error
}

func (c Config) withDefaults() Config {
	if c.Concurrency <= 0 {
		c.Concurrency = 2
	}
	if c.BatchSize <= 0 {
		c.BatchSize = 1024
	}
	if c.ShareThreshold <= 0 {
		c.ShareThreshold = 32
	}
	if c.QueueLimit <= 0 {
		c.QueueLimit = 65536
	}
	if c.MaxTrackedInodes <= 0 {
		c.MaxTrackedInodes = 2_000_000
	}
	if c.WorkerDrainTimeout <= 0 {
		c.WorkerDrainTimeout = 5 * time.Second
	}
	if c.HeartbeatEvery <= 0 {
		c.HeartbeatEvery = 30 * time.Second
	}
	// A batch is acquired with a single WaitN, and WaitN fails outright when n exceeds the burst.
	// Silently clamping the batch instead would make the rate limit quietly change the read size.
	if c.RateLimit > 0 && c.RateLimitBurst < c.BatchSize+1 {
		c.RateLimitBurst = c.BatchSize + 1
	}
	return c
}

// errUnresolvedSymlink marks a configured target that is still a symbolic link by the time the
// engine sees it. Target resolution follows links before handing paths over, so this is a
// programming or configuration fault rather than a filesystem condition.
var errUnresolvedSymlink = errors.New("target root is an unresolved symbolic link")

// Sink receives scan lifecycle events. *state.Store satisfies it.
type Sink interface {
	MarkScanStarted(target string)
	Apply(state.Result)
}

// Engine walks targets. It is safe for concurrent use; overlapping calls to ScanAll are rejected
// rather than queued.
type Engine struct {
	fs      fsstat.FS
	cfg     Config
	logger  *slog.Logger
	limiter *rate.Limiter

	scanMu sync.Mutex

	scansSkipped  atomic.Uint64
	rateWaitNanos atomic.Int64
	// liveWorkers counts worker goroutines that have not returned, including ones leaked by
	// earlier cycles. During a normal scan it equals the concurrency limit, so it is a measure of
	// activity rather than of trouble.
	liveWorkers atomic.Int64
	// abandonedWorkers is the leaked count, sampled after each cycle has closed its pool and given
	// workers a chance to exit. Reporting liveWorkers here instead would flag every running scan
	// as stuck.
	abandonedWorkers atomic.Int64
}

// NewEngine returns an engine reading through fs.
func NewEngine(fs fsstat.FS, cfg Config, logger *slog.Logger) *Engine {
	cfg = cfg.withDefaults()
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	engine := &Engine{fs: fs, cfg: cfg, logger: logger}
	if cfg.RateLimit > 0 {
		engine.limiter = rate.NewLimiter(rate.Limit(cfg.RateLimit), cfg.RateLimitBurst)
	}
	return engine
}

// Config returns the effective configuration, defaults applied.
func (e *Engine) Config() Config { return e.cfg }

// Stats reports engine-wide counters that are not attributable to any single target.
type Stats struct {
	ScansSkipped     uint64
	RateLimitWait    float64
	AbandonedWorkers int64
}

func (e *Engine) Stats() Stats {
	return Stats{
		ScansSkipped:     e.scansSkipped.Load(),
		RateLimitWait:    time.Duration(e.rateWaitNanos.Load()).Seconds(),
		AbandonedWorkers: e.abandonedWorkers.Load(),
	}
}

// ScanAll walks every target once and reports each result to sink as it finishes.
//
// It is single-flight: if a cycle is already running, this returns false immediately rather than
// piling a second walk onto the same filesystem. That rejection is counted, because silently
// skipping scans is exactly the kind of thing that makes a stale metric hard to explain.
func (e *Engine) ScanAll(ctx context.Context, targets []string, sink Sink) bool {
	if !e.scanMu.TryLock() {
		e.scansSkipped.Add(1)
		return false
	}
	defer e.scanMu.Unlock()

	work := newPool(e.cfg.QueueLimit, e.cfg.ShareThreshold)
	var workers sync.WaitGroup
	for i := 0; i < e.cfg.Concurrency; i++ {
		workers.Add(1)
		e.liveWorkers.Add(1)
		go func() {
			defer workers.Done()
			defer e.liveWorkers.Add(-1)
			e.worker(ctx, work)
		}()
	}

	var supervisors sync.WaitGroup
	for _, path := range targets {
		target := newTargetScan(path, time.Now(), e.cfg.MaxTrackedInodes)
		sink.MarkScanStarted(path)

		outcome, ready := e.prepareRoot(target)
		if !ready {
			// The root could not be used, so there is nothing to queue. Reporting immediately
			// keeps the failure fast rather than waiting out a timeout.
			sink.Apply(e.buildResult(target, outcome))
			e.logSummary(target, outcome)
			continue
		}

		target.addPending(1)
		work.push([]*dirRef{{path: path, target: target}}, nil)

		supervisors.Add(1)
		go func() {
			defer supervisors.Done()
			e.supervise(ctx, target, sink)
		}()
	}
	supervisors.Wait()

	// Closing releases workers that are parked waiting for work, and makes them drop anything left
	// on their local stacks belonging to abandoned targets.
	work.close()
	e.drainWorkers(&workers)
	return true
}

// drainWorkers waits for workers to exit, but only for a bounded time.
//
// A worker stuck in an uninterruptible syscall on a hung mount will never return, and blocking on
// it would wedge every future scan cycle. Each cycle starts fresh workers, so throughput recovers
// on its own; the leaked goroutines are surfaced through Stats instead of being waited on.
func (e *Engine) drainWorkers(workers *sync.WaitGroup) {
	finished := make(chan struct{})
	go func() {
		workers.Wait()
		close(finished)
	}()
	timer := time.NewTimer(e.cfg.WorkerDrainTimeout)
	defer timer.Stop()
	select {
	case <-finished:
	case <-timer.C:
		e.logger.Warn("Scan workers did not exit; presumed stuck in an uninterruptible filesystem call",
			"live_workers", e.liveWorkers.Load(), "drain_timeout", e.cfg.WorkerDrainTimeout)
	}

	// Sampled after the pool has closed and workers have had their chance to exit, so anything
	// still running is genuinely leaked. This is deliberately read in both branches: a clean drain
	// still leaves behind any workers leaked by earlier cycles, and those remain leaked.
	e.abandonedWorkers.Store(e.liveWorkers.Load())
}

// prepareRoot validates the target root and captures its device for the one-filesystem check.
func (e *Engine) prepareRoot(target *targetScan) (state.Outcome, bool) {
	var st fsstat.FileStat
	if err := e.fs.Lstat(target.path, &st); err != nil {
		target.recordError(err)
		if isVanished(err) {
			return state.OutcomeMissing, false
		}
		return state.OutcomeRootError, false
	}
	if st.Mode&fs.ModeSymlink != 0 {
		// Target resolution is expected to have followed this already. Reaching here means the
		// configured path is an unresolved symlink, which the previous implementation measured as
		// zero bytes while reporting success â€” a failure that looked healthy on every dashboard.
		target.recordError(errUnresolvedSymlink)
		return state.OutcomeRootError, false
	}
	if !st.IsDir() {
		return state.OutcomeMissing, false
	}
	if e.fs.Caps().Has(fsstat.CapInode) {
		target.rootDev, target.hasRootDev = st.Dev, true
	}
	return "", true
}

// supervise waits for one target's walk to finish, enforcing both timeouts, and reports the result.
func (e *Engine) supervise(ctx context.Context, target *targetScan, sink Sink) {
	scanCtx := ctx
	var cancel context.CancelFunc = func() {}
	if e.cfg.ScanTimeout > 0 {
		scanCtx, cancel = context.WithTimeout(ctx, e.cfg.ScanTimeout)
	}
	defer cancel()

	stopHeartbeat := e.startHeartbeat(target)
	defer stopHeartbeat()

	hard := newOptionalTimer(e.cfg.HardTimeout)
	defer hard.Stop()

	outcome := state.OutcomeComplete
	select {
	case <-target.done:
	case <-scanCtx.Done():
		// Workers check this flag once per batch, so the walk stops within roughly one batch of
		// the deadline rather than after the next full directory.
		target.cancelled.Store(true)

		// This wait must have a deadline of its own rather than reusing the hard timer. On process
		// shutdown workers return as soon as they notice the root context, which can leave this
		// target's queue undrained so its latch never fires. With --scan.timeout unset the hard
		// timer is disabled, so waiting on it alone would block here forever.
		drain := newOptionalTimer(longest(e.cfg.HardTimeout, e.cfg.WorkerDrainTimeout))
		defer drain.Stop()
		select {
		case <-target.done:
		case <-drain.C():
			e.abandon(target)
		}
	case <-hard.C():
		e.abandon(target)
	}

	switch {
	case ctx.Err() != nil:
		// The process is shutting down. This says nothing about the directory, so internal/state
		// treats it as a complete no-op rather than as evidence of anything.
		outcome = state.OutcomeCancelled
	case scanCtx.Err() != nil || target.abandoned.Load():
		outcome = state.OutcomeTimeout
	default:
		if _, total := target.errorSnapshot(); total > 0 || target.inodesExhausted.Load() {
			outcome = state.OutcomePartial
		}
	}

	stopHeartbeat()
	sink.Apply(e.buildResult(target, outcome))
	e.logSummary(target, outcome)
}

// abandon gives up on a target whose workers are presumed stuck, releasing its latch so the cycle
// can finish. Refs still queued for it are discarded without I/O when a worker reaches them.
func (e *Engine) abandon(target *targetScan) {
	target.abandoned.Store(true)
	target.finish()
	e.logger.Error("Abandoning scan; workers appear stuck in an uninterruptible filesystem call",
		"target", target.path,
		"hard_timeout", e.cfg.HardTimeout,
		"elapsed_seconds", time.Since(target.startedAt).Seconds(),
		"entries_scanned", target.entries.Load())
}

func (e *Engine) buildResult(target *targetScan, outcome state.Outcome) state.Result {
	errorCounts, _ := target.errorSnapshot()
	return state.Result{
		Target:                    target.path,
		Outcome:                   outcome,
		SizeBytes:                 target.size.Load(),
		AllocBytes:                target.alloc.Load(),
		Files:                     target.files.Load(),
		Directories:               target.dirs.Load(),
		HardlinkedFiles:           target.hardlinked.Load(),
		DedupSavedBytes:           target.dedupSaved.Load(),
		HardlinkTrackingExhausted: target.inodesExhausted.Load(),
		DurationSeconds:           time.Since(target.startedAt).Seconds(),
		Errors:                    errorCounts,
		VanishedFiles:             target.vanished.Load(),
		EntriesScanned:            target.entries.Load(),
		StatCalls:                 target.statCalls.Load(),
		DirsRead:                  target.dirsRead.Load(),
		SkippedOtherFilesystem:    target.skippedXdev.Load(),
	}
}

// worker is one member of the shared pool.
func (e *Engine) worker(ctx context.Context, work *pool) {
	if e.cfg.WorkerInit != nil {
		if err := e.cfg.WorkerInit(); err != nil {
			// Logged once per worker per cycle and otherwise ignored: failing to lower priority
			// makes the scan noisier than intended, but refusing to scan at all would be a worse
			// outcome than scanning at normal priority.
			e.logger.Warn("Could not apply scan worker scheduling priority", "err", err)
		}
	}

	// Reused across every entry this worker ever handles, so the hot path allocates nothing.
	var st fsstat.FileStat
	var totals dirTotals
	var local []*dirRef

	for {
		ref, ok := work.next(&local)
		if !ok {
			return
		}
		target := ref.target
		if target.cancelled.Load() || target.abandoned.Load() {
			// Discard without touching the filesystem, so a cancelled target's queue drains
			// immediately and its latch fires promptly instead of after thousands more syscalls.
			target.completeDir()
			continue
		}
		children := e.processDir(ctx, ref, &st, &totals)
		if len(children) > 0 {
			// Reserve before publishing. See targetScan.addPending for why the order matters.
			target.addPending(int64(len(children)))
			work.push(children, &local)
		}
		target.completeDir()
		if ctx.Err() != nil {
			return
		}
	}
}

// waitForTokens acquires budget for a whole batch at once.
func (e *Engine) waitForTokens(ctx context.Context, n int) error {
	if burst := e.limiter.Burst(); n > burst {
		n = burst
	}
	started := time.Now()
	err := e.limiter.WaitN(ctx, n)
	e.rateWaitNanos.Add(int64(time.Since(started)))
	return err
}

// --- work pool -------------------------------------------------------------------------------

// pool holds directories discovered but not yet read.
//
// Each worker keeps a private LIFO stack and only donates to the shared queue once it has plenty
// of work. LIFO means depth-first, which keeps the working set small and the directory-entry cache
// hot. A worker that cannot donate keeps the work instead of blocking, so no worker ever waits to
// enqueue â€” which is precisely the deadlock a bounded channel with recursive sends would hit.
type pool struct {
	mu        sync.Mutex
	cond      *sync.Cond
	deque     []*dirRef
	limit     int
	shareOver int
	closed    atomic.Bool
}

func newPool(limit, shareOver int) *pool {
	p := &pool{limit: limit, shareOver: shareOver}
	p.cond = sync.NewCond(&p.mu)
	return p
}

// push adds work, keeping most of it local when a local stack is supplied.
func (p *pool) push(refs []*dirRef, local *[]*dirRef) {
	if local == nil {
		p.mu.Lock()
		p.deque = append(p.deque, refs...)
		p.cond.Broadcast()
		p.mu.Unlock()
		return
	}

	*local = append(*local, refs...)
	if len(*local) <= p.shareOver {
		return
	}
	// Donate the oldest half: those are the shallowest directories, so other workers get subtrees
	// that are furthest from this worker's current position and least likely to be contended.
	share := len(*local) / 2
	p.mu.Lock()
	if free := p.limit - len(p.deque); free > 0 {
		if share > free {
			share = free
		}
		p.deque = append(p.deque, (*local)[:share]...)
		*local = append((*local)[:0], (*local)[share:]...)
		p.cond.Broadcast()
	}
	p.mu.Unlock()
}

// next returns the next directory to process, blocking while other workers still hold work.
func (p *pool) next(local *[]*dirRef) (*dirRef, bool) {
	if p.closed.Load() {
		return nil, false
	}
	if n := len(*local); n > 0 {
		ref := (*local)[n-1]
		(*local)[n-1] = nil
		*local = (*local)[:n-1]
		return ref, true
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	for {
		if n := len(p.deque); n > 0 {
			ref := p.deque[n-1]
			p.deque[n-1] = nil
			p.deque = p.deque[:n-1]
			return ref, true
		}
		if p.closed.Load() {
			return nil, false
		}
		p.cond.Wait()
	}
}

// close releases every parked worker. Workers also drop their local stacks once closed, so a
// cycle ends promptly even with work still queued for an abandoned target.
func (p *pool) close() {
	p.mu.Lock()
	p.closed.Store(true)
	p.deque = nil
	p.cond.Broadcast()
	p.mu.Unlock()
}

// --- small helpers ---------------------------------------------------------------------------

// optionalTimer models a timeout that may be disabled, without the caller special-casing nil.
type optionalTimer struct{ timer *time.Timer }

func newOptionalTimer(d time.Duration) optionalTimer {
	if d <= 0 {
		return optionalTimer{}
	}
	return optionalTimer{timer: time.NewTimer(d)}
}

func (t optionalTimer) C() <-chan time.Time {
	if t.timer == nil {
		return nil // a nil channel blocks forever, which is exactly "disabled" in a select
	}
	return t.timer.C
}

func (t optionalTimer) Stop() {
	if t.timer != nil {
		t.timer.Stop()
	}
}

// longest returns the larger duration, treating a non-positive value as "unset" so a disabled
// timeout never wins and produces a disabled timer.
func longest(a, b time.Duration) time.Duration {
	if a > b {
		return a
	}
	return b
}
