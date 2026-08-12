package scan

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/local/directory-size-exporter/internal/fsstat"
	"github.com/local/directory-size-exporter/internal/state"
)

// recordingSink captures results in place of a real state.Store.
type recordingSink struct {
	mu      sync.Mutex
	started []string
	results map[string]state.Result
}

func newSink() *recordingSink {
	return &recordingSink{results: map[string]state.Result{}}
}

func (s *recordingSink) MarkScanStarted(target string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.started = append(s.started, target)
}

func (s *recordingSink) Apply(result state.Result) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.results[result.Target] = result
}

func (s *recordingSink) get(t *testing.T, target string) state.Result {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	result, ok := s.results[target]
	if !ok {
		t.Fatalf("no result for %s (have %v)", target, s.results)
	}
	return result
}

// capturingHandler records slog records so tests can assert on log volume and content. The
// "never log per file" rule is only meaningful if something checks it.
type capturingHandler struct {
	mu      sync.Mutex
	records []slog.Record
}

func (h *capturingHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *capturingHandler) WithAttrs([]slog.Attr) slog.Handler       { return h }
func (h *capturingHandler) WithGroup(string) slog.Handler            { return h }

func (h *capturingHandler) Handle(_ context.Context, record slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.records = append(h.records, record.Clone())
	return nil
}

func (h *capturingHandler) all() []slog.Record {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]slog.Record(nil), h.records...)
}

func testConfig(cfg Config) Config {
	if cfg.WorkerDrainTimeout == 0 {
		cfg.WorkerDrainTimeout = 2 * time.Second
	}
	return cfg
}

func runScan(t *testing.T, fake *fsstat.FakeFS, cfg Config, targets ...string) (*recordingSink, *Engine) {
	t.Helper()
	engine := NewEngine(fake, testConfig(cfg), nil)
	sink := newSink()
	if !engine.ScanAll(context.Background(), targets, sink) {
		t.Fatal("scan was unexpectedly skipped")
	}
	return sink, engine
}

// --- measurement ------------------------------------------------------------------------------

func TestWalk_CountsOnlyRegularFilesAcrossNestedDirectories(t *testing.T) {
	fake := fsstat.NewFake(fsstat.CapAllocBytes|fsstat.CapInode).
		AddFile("/logs/trades.log", 10).
		AddFile("/logs/nested/fills.log", 5).
		AddFile("/logs/nested/deeper/orders.log", 7).
		AddSymlink("/logs/archive").
		AddFileStat("/logs/pipe", fsstat.FileStat{Mode: fs.ModeNamedPipe, Size: 999}).
		AddFileStat("/logs/sock", fsstat.FileStat{Mode: fs.ModeSocket, Size: 999})

	sink, _ := runScan(t, fake, Config{}, "/logs")
	got := sink.get(t, "/logs")

	if got.Outcome != state.OutcomeComplete {
		t.Fatalf("outcome = %s, want complete", got.Outcome)
	}
	if got.SizeBytes != 22 {
		t.Errorf("size = %d, want 22 (sockets, pipes and symlinks must not count)", got.SizeBytes)
	}
	if got.Files != 3 {
		t.Errorf("files = %d, want 3", got.Files)
	}
	if got.Directories != 2 {
		t.Errorf("directories = %d, want 2", got.Directories)
	}
	// Allocation rounds each file up to a block, so it must exceed the logical total.
	if got.AllocBytes != 3*512 {
		t.Errorf("alloc = %d, want %d", got.AllocBytes, 3*512)
	}
}

func TestWalk_DoesNotFollowSymlinkedDirectories(t *testing.T) {
	fake := fsstat.NewFake(0).
		AddFile("/logs/trades.log", 10).
		AddFile("/elsewhere/huge.log", 1_000_000)
	// A symlink pointing at another tree must contribute nothing, or a target could silently
	// measure the whole disk.
	fake.AddSymlink("/logs/link-to-elsewhere")

	sink, _ := runScan(t, fake, Config{}, "/logs")
	if got := sink.get(t, "/logs"); got.SizeBytes != 10 {
		t.Fatalf("size = %d, want 10", got.SizeBytes)
	}
}

func TestWalk_UnsortedReadOrderDoesNotAffectTotals(t *testing.T) {
	build := func(shuffle bool) state.Result {
		fake := fsstat.NewFake(0)
		for i := 0; i < 50; i++ {
			fake.AddFile(fmt.Sprintf("/logs/dir%02d/file.log", i), int64(i))
		}
		if shuffle {
			fake.ShuffleReads(7)
		}
		sink, _ := runScan(t, fake, Config{Concurrency: 4, BatchSize: 8}, "/logs")
		return sink.get(t, "/logs")
	}

	sorted, shuffled := build(false), build(true)
	if sorted.SizeBytes != shuffled.SizeBytes || sorted.Files != shuffled.Files {
		t.Fatalf("ordering changed totals: %+v vs %+v", sorted, shuffled)
	}
	if sorted.SizeBytes != 1225 { // 0+1+...+49
		t.Errorf("size = %d, want 1225", sorted.SizeBytes)
	}
}

// TestWalk_StatsEntriesByBareName locks in the descriptor-relative design. A joined path here means
// the walker reverted to full-path resolution, which makes the kernel re-walk every ancestor
// directory for every one of ten million files.
func TestWalk_StatsEntriesByBareName(t *testing.T) {
	fake := fsstat.NewFake(0).
		AddFile("/logs/a/b/c/trades.log", 10).
		AddFile("/logs/a/b/c/fills.log", 5)

	runScan(t, fake, Config{}, "/logs")
	if joined := fake.NonBareStatNames(); len(joined) > 0 {
		t.Fatalf("entries were stat'ed by joined path: %v", joined)
	}
}

// --- outcomes ---------------------------------------------------------------------------------

func TestWalk_MissingTargetReportsMissingWithoutMeasuring(t *testing.T) {
	fake := fsstat.NewFake(0).AddFile("/logs/trades.log", 10)

	sink, _ := runScan(t, fake, Config{}, "/gone")
	got := sink.get(t, "/gone")
	if got.Outcome != state.OutcomeMissing {
		t.Fatalf("outcome = %s, want missing", got.Outcome)
	}
	// A zero here would be published by the retention rules as a real measurement if the outcome
	// were ever misclassified, so the outcome is the whole safety mechanism.
	if got.SizeBytes != 0 {
		t.Errorf("size = %d, want 0", got.SizeBytes)
	}
}

func TestWalk_UnresolvedSymlinkRootIsAnError(t *testing.T) {
	fake := fsstat.NewFake(0).AddFile("/real/trades.log", 10)
	fake.AddSymlink("/logs")

	// The historical failure was measuring zero bytes and reporting success. Failing loudly is the
	// point: a green zero is far harder to notice than a red error.
	sink, _ := runScan(t, fake, Config{}, "/logs")
	got := sink.get(t, "/logs")
	if got.Outcome != state.OutcomeRootError {
		t.Fatalf("outcome = %s, want root_error", got.Outcome)
	}
	if got.SizeBytes != 0 {
		t.Errorf("size = %d, want 0", got.SizeBytes)
	}
}

func TestWalk_UnreadableSubdirectoryProducesPartial(t *testing.T) {
	fake := fsstat.NewFake(0).
		AddFile("/logs/trades.log", 10).
		AddFile("/logs/locked/secret.log", 500).
		SetOpenError("/logs/locked", fs.ErrPermission)

	sink, _ := runScan(t, fake, Config{}, "/logs")
	got := sink.get(t, "/logs")
	if got.Outcome != state.OutcomePartial {
		t.Fatalf("outcome = %s, want partial", got.Outcome)
	}
	if got.Errors[ClassPermission] != 1 {
		t.Errorf("permission errors = %v, want 1", got.Errors)
	}
	// The bytes it did manage to read are reported, but the partial outcome is what stops the
	// retention rules from publishing this undercount as the directory's size.
	if got.SizeBytes != 10 {
		t.Errorf("size = %d, want 10", got.SizeBytes)
	}
}

// TestWalk_VanishedEntriesAreNotErrors matters because log rotation makes this constant. Counting
// it as an error would mark every scan partial and leave a busy log directory permanently unable
// to publish a size at all.
func TestWalk_VanishedEntriesAreNotErrors(t *testing.T) {
	fake := fsstat.NewFake(0).
		AddFile("/logs/trades.log", 10).
		AddFile("/logs/rotated.log", 500).
		SetStatError("/logs/rotated.log", fs.ErrNotExist)

	sink, _ := runScan(t, fake, Config{}, "/logs")
	got := sink.get(t, "/logs")
	if got.Outcome != state.OutcomeComplete {
		t.Fatalf("outcome = %s, want complete; a rotated file is not a fault", got.Outcome)
	}
	if got.VanishedFiles != 1 {
		t.Errorf("vanished = %d, want 1", got.VanishedFiles)
	}
	if len(got.Errors) != 0 {
		t.Errorf("errors = %v, want none", got.Errors)
	}
	if got.SizeBytes != 10 {
		t.Errorf("size = %d, want 10", got.SizeBytes)
	}
}

// TestWalk_CancellationStopsWithinOneBatch is the regression guard for checking cancellation per
// entry. The check moved to once per batch because context.Err takes a mutex, which at ten million
// entries is both a cost and a point of contention shared by every worker.
func TestWalk_CancellationStopsWithinOneBatch(t *testing.T) {
	const batch = 1024
	fake := fsstat.NewFake(0).AddSyntheticFiles("/logs", 1_000_000, 8)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var stats atomic.Int64
	fake.BeforeStat(func(string, string) error {
		if stats.Add(1) == 1 {
			cancel()
		}
		return nil
	})

	engine := NewEngine(fake, testConfig(Config{Concurrency: 1, BatchSize: batch}), nil)
	sink := newSink()
	engine.ScanAll(ctx, []string{"/logs"}, sink)

	// The in-flight batch finishes, then the loop notices. Anything beyond two batches means the
	// walk is ignoring cancellation between batches.
	if got := fake.StatCalls(); got > 2*batch {
		t.Fatalf("stat calls after cancellation = %d, want at most %d", got, 2*batch)
	}
	if got := sink.get(t, "/logs"); got.Outcome != state.OutcomeCancelled {
		t.Errorf("outcome = %s, want cancelled", got.Outcome)
	}
}

func TestWalk_ScanTimeoutProducesTimeoutNotPartial(t *testing.T) {
	fake := fsstat.NewFake(0).AddSyntheticFiles("/logs", 500_000, 8)
	fake.BeforeRead(func(string) error {
		time.Sleep(20 * time.Millisecond)
		return nil
	})

	sink, _ := runScan(t, fake, Config{Concurrency: 1, BatchSize: 16, ScanTimeout: 40 * time.Millisecond}, "/logs")
	got := sink.get(t, "/logs")
	// Timeout and partial are both withheld from publication, but they need different alerts: one
	// says tune the deadline, the other says fix the permissions.
	if got.Outcome != state.OutcomeTimeout {
		t.Fatalf("outcome = %s, want timeout", got.Outcome)
	}
}

// --- traversal options ------------------------------------------------------------------------

func TestWalk_HardlinkDedupCountsSharedBytesOnce(t *testing.T) {
	shared := fsstat.FileStat{Size: 100, AllocBytes: 512, Mode: 0o644, Nlink: 2, Dev: 1, Ino: 42}
	build := func(dedup bool) state.Result {
		fake := fsstat.NewFake(fsstat.CapInode).
			AddFileStat("/logs/trades.log", shared).
			AddFileStat("/logs/archive/trades.log.1", shared)
		sink, _ := runScan(t, fake, Config{DedupHardlinks: dedup}, "/logs")
		return sink.get(t, "/logs")
	}

	if got := build(false); got.SizeBytes != 200 {
		t.Errorf("without dedup size = %d, want 200", got.SizeBytes)
	}
	got := build(true)
	if got.SizeBytes != 100 {
		t.Errorf("with dedup size = %d, want 100", got.SizeBytes)
	}
	if got.DedupSavedBytes != 100 {
		t.Errorf("dedup saved = %d, want 100", got.DedupSavedBytes)
	}
	if got.HardlinkedFiles != 2 {
		t.Errorf("hardlinked files = %d, want 2", got.HardlinkedFiles)
	}
}

// TestWalk_HardlinkTrackingExhaustionIsSurfaced checks that hitting the memory cap degrades
// loudly. Past the cap duplicates are counted again, overstating the size, so the condition must
// be visible rather than silently changing what the number means.
func TestWalk_HardlinkTrackingExhaustionIsSurfaced(t *testing.T) {
	fake := fsstat.NewFake(fsstat.CapInode)
	for i := 0; i < 10; i++ {
		fake.AddFileStat(fmt.Sprintf("/logs/file%02d.log", i), fsstat.FileStat{
			Size: 10, Mode: 0o644, Nlink: 2, Dev: 1, Ino: uint64(i),
		})
	}

	sink, _ := runScan(t, fake, Config{DedupHardlinks: true, MaxTrackedInodes: 3}, "/logs")
	got := sink.get(t, "/logs")
	if !got.HardlinkTrackingExhausted {
		t.Fatal("exhaustion was not reported")
	}
	// Exhaustion means the number can no longer be trusted as exact, so the scan is partial.
	if got.Outcome != state.OutcomePartial {
		t.Errorf("outcome = %s, want partial once dedup can no longer be guaranteed", got.Outcome)
	}
}

func TestWalk_OneFilesystemStopsAtMountBoundaries(t *testing.T) {
	fake := fsstat.NewFake(fsstat.CapInode).
		AddFileStat("/logs", fsstat.FileStat{Mode: fs.ModeDir | 0o755, Dev: 1, Ino: 1}).
		AddFileStat("/logs/trades.log", fsstat.FileStat{Size: 10, Mode: 0o644, Dev: 1, Ino: 2}).
		AddFileStat("/logs/mounted", fsstat.FileStat{Mode: fs.ModeDir | 0o755, Dev: 99, Ino: 3}).
		AddFileStat("/logs/mounted/other.log", fsstat.FileStat{Size: 5000, Mode: 0o644, Dev: 99, Ino: 4})

	if got := mustScan(t, fake, Config{OneFilesystem: false}); got.SizeBytes != 5010 {
		t.Errorf("without one-filesystem size = %d, want 5010", got.SizeBytes)
	}
	got := mustScan(t, fake, Config{OneFilesystem: true})
	// A nested mount has its own capacity; counting it here would attribute bytes to a volume that
	// does not hold them, which breaks the percentage-of-volume query outright.
	if got.SizeBytes != 10 {
		t.Errorf("with one-filesystem size = %d, want 10", got.SizeBytes)
	}
	if got.SkippedOtherFilesystem != 1 {
		t.Errorf("skipped = %d, want 1", got.SkippedOtherFilesystem)
	}
}

func mustScan(t *testing.T, fake *fsstat.FakeFS, cfg Config) state.Result {
	t.Helper()
	sink, _ := runScan(t, fake, cfg, "/logs")
	return sink.get(t, "/logs")
}

// --- engine behaviour -------------------------------------------------------------------------

func TestEngine_ConcurrencyNeverExceedsTheConfiguredLimit(t *testing.T) {
	fake := fsstat.NewFake(0)
	for i := 0; i < 40; i++ {
		for j := 0; j < 5; j++ {
			fake.AddFile(fmt.Sprintf("/logs/dir%02d/sub%d/file.log", i, j), 1)
		}
	}
	fake.BeforeRead(func(string) error {
		time.Sleep(time.Millisecond)
		return nil
	})

	runScan(t, fake, Config{Concurrency: 3, BatchSize: 4}, "/logs")
	// Directories are opened, drained and closed before children are queued, so this bound holds
	// regardless of tree depth. Holding handles in the queue instead would make descriptor
	// exhaustion a function of tree shape.
	if got := fake.MaxOpenDirs(); got > 3 {
		t.Fatalf("max open directories = %d, want at most 3", got)
	}
}

func TestEngine_PerTargetTotalsAreNotCrossContaminated(t *testing.T) {
	fake := fsstat.NewFake(0)
	for i := 0; i < 20; i++ {
		fake.AddFile(fmt.Sprintf("/a/dir%02d/file.log", i), 10)
		fake.AddFile(fmt.Sprintf("/b/dir%02d/file.log", i), 100)
		fake.AddFile(fmt.Sprintf("/c/dir%02d/file.log", i), 1000)
	}

	for run := 0; run < 20; run++ {
		sink, _ := runScan(t, fake, Config{Concurrency: 4, BatchSize: 2}, "/a", "/b", "/c")
		for target, want := range map[string]int64{"/a": 200, "/b": 2000, "/c": 20000} {
			if got := sink.get(t, target).SizeBytes; got != want {
				t.Fatalf("run %d: %s size = %d, want %d", run, target, got, want)
			}
		}
	}
}

// TestEngine_LatchDoesNotFireEarly is the highest-severity concurrency guard in the package.
//
// If the completion latch can fire while work remains, a target is finalised holding a fraction of
// its size AND marked complete — which the retention rules would happily publish. That is strictly
// worse than any failure mode this exporter is designed to prevent, so it is exercised hard.
func TestEngine_LatchDoesNotFireEarly(t *testing.T) {
	// A deep chain maximises the number of moments where exactly one directory is in flight and
	// the queue is momentarily empty, which is precisely when a mis-ordered latch fires.
	const depth = 40
	path := "/logs"
	fake := fsstat.NewFake(0)
	for i := 0; i < depth; i++ {
		path = fmt.Sprintf("%s/level%02d", path, i)
		fake.AddFile(path+"/file.log", 1)
	}
	fake.BeforeRead(func(string) error {
		// Widen the pop-to-publish window so a wrong ordering is reliably caught rather than
		// occasionally caught.
		time.Sleep(50 * time.Microsecond)
		return nil
	})

	for run := 0; run < 200; run++ {
		sink, _ := runScan(t, fake, Config{Concurrency: 4, BatchSize: 1}, "/logs")
		got := sink.get(t, "/logs")
		if got.SizeBytes != depth {
			t.Fatalf("run %d: size = %d, want %d — the latch fired before the walk finished", run, got.SizeBytes, depth)
		}
		if got.Outcome != state.OutcomeComplete {
			t.Fatalf("run %d: outcome = %s, want complete", run, got.Outcome)
		}
	}
}

func TestEngine_SingleFlightRejectsOverlappingScans(t *testing.T) {
	fake := fsstat.NewFake(0).AddSyntheticFiles("/logs", 20_000, 1)
	release := make(chan struct{})
	var once sync.Once
	fake.BeforeRead(func(string) error {
		once.Do(func() { <-release })
		return nil
	})

	engine := NewEngine(fake, testConfig(Config{Concurrency: 1, BatchSize: 16}), nil)
	started := make(chan struct{})
	go func() {
		close(started)
		engine.ScanAll(context.Background(), []string{"/logs"}, newSink())
	}()
	<-started
	// Wait until the first scan is genuinely inside the walk.
	for fake.ReadCalls() == 0 {
		time.Sleep(time.Millisecond)
	}

	if engine.ScanAll(context.Background(), []string{"/logs"}, newSink()) {
		t.Fatal("an overlapping scan was accepted; two walks would double the filesystem load")
	}
	if got := engine.Stats().ScansSkipped; got != 1 {
		t.Errorf("skipped scans = %d, want 1; silent skips make a stale metric unexplainable", got)
	}
	close(release)
}

// TestEngine_AbandonedWorkersIsZeroAfterHealthyScans separates "workers are running" from
// "workers are stuck".
//
// Reporting the live worker count as abandoned makes the gauge equal the concurrency limit during
// every normal scan. The shipped alert fires at or above the concurrency limit, so on a tree whose
// cycle legitimately exceeds the alert's `for` window — precisely the workload this exporter is
// built for — it would fire continuously, get silenced, and take the real signal with it.
func TestEngine_AbandonedWorkersIsZeroAfterHealthyScans(t *testing.T) {
	fake := fsstat.NewFake(0)
	for i := 0; i < 200; i++ {
		fake.AddFile(fmt.Sprintf("/logs/dir%03d/file.log", i), 10)
	}
	fake.BeforeRead(func(string) error {
		time.Sleep(500 * time.Microsecond)
		return nil
	})

	const concurrency = 4
	engine := NewEngine(fake, testConfig(Config{Concurrency: concurrency, BatchSize: 1}), nil)

	done := make(chan struct{})
	go func() {
		engine.ScanAll(context.Background(), []string{"/logs"}, newSink())
		close(done)
	}()

	// The gauge has to be sampled WHILE the scan runs, because that is when Prometheus scrapes it.
	// A sample taken after ScanAll returns proves nothing: every worker has exited by then, so a
	// gauge wired to the live worker count would read zero and look correct.
	for fake.ReadCalls() < 10 {
		time.Sleep(time.Millisecond)
	}
	midScan := engine.Stats().AbandonedWorkers
	<-done

	if midScan != 0 {
		t.Fatalf("abandoned workers = %d during a healthy scan, want 0; the alert at >= concurrency (%d) would fire on every long cycle",
			midScan, concurrency)
	}
	if got := engine.Stats().AbandonedWorkers; got != 0 {
		t.Errorf("abandoned workers = %d after a clean scan, want 0", got)
	}
}

// TestEngine_ShutdownDoesNotBlockWithoutAHardTimeout covers the default configuration, where
// --scan.timeout is unset and therefore no hard timeout is derived.
//
// Workers return as soon as they notice the root context, which can leave a target's queue
// undrained so its completion latch never fires. If the cancelled path waited only on the hard
// timer, that wait would be on a disabled timer and the cycle would never finish.
func TestEngine_ShutdownDoesNotBlockWithoutAHardTimeout(t *testing.T) {
	fake := fsstat.NewFake(0)
	for i := 0; i < 200; i++ {
		fake.AddFile(fmt.Sprintf("/logs/dir%03d/file.log", i), 10)
	}
	fake.BeforeRead(func(string) error {
		time.Sleep(200 * time.Microsecond)
		return nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	engine := NewEngine(fake, Config{
		Concurrency: 2, BatchSize: 1,
		// Deliberately the defaults: no scan timeout, so no hard timeout is derived either.
		ScanTimeout: 0, HardTimeout: 0,
		WorkerDrainTimeout: 250 * time.Millisecond,
	}, nil)

	sink := newSink()
	done := make(chan bool, 1)
	go func() { done <- engine.ScanAll(ctx, []string{"/logs"}, sink) }()

	// Cancel once the walk is genuinely under way.
	for fake.ReadCalls() < 5 {
		time.Sleep(time.Millisecond)
	}
	cancel()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("ScanAll never returned after cancellation; the cycle deadlocked waiting on a disabled timer")
	}
	if got := sink.get(t, "/logs"); got.Outcome != state.OutcomeCancelled {
		t.Errorf("outcome = %s, want cancelled", got.Outcome)
	}
}

// TestEngine_HardTimeoutAbandonsStuckWorker models a hung NFS mount. Filesystem syscalls in that
// state are uninterruptible, so no context plumbing can rescue the worker; the engine must give up
// on the target and keep running, or one bad mount wedges every future cycle.
func TestEngine_HardTimeoutAbandonsStuckWorker(t *testing.T) {
	fake := fsstat.NewFake(0).AddFile("/logs/trades.log", 10)
	blocked := make(chan struct{})
	defer close(blocked)
	fake.BeforeRead(func(string) error {
		<-blocked // never returns during the test, exactly like a D-state syscall
		return nil
	})

	engine := NewEngine(fake, Config{
		Concurrency: 1, ScanTimeout: 20 * time.Millisecond,
		HardTimeout: 60 * time.Millisecond, WorkerDrainTimeout: 50 * time.Millisecond,
	}, nil)
	sink := newSink()

	finished := make(chan struct{})
	go func() {
		engine.ScanAll(context.Background(), []string{"/logs"}, sink)
		close(finished)
	}()
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("ScanAll never returned; a single stuck worker wedged the whole cycle")
	}

	if got := sink.get(t, "/logs"); got.Outcome != state.OutcomeTimeout {
		t.Errorf("outcome = %s, want timeout", got.Outcome)
	}
	// Measured only once the pool has closed and workers have had their chance to exit, so this
	// counts a genuine leak rather than a scan that happened to be running.
	if got := engine.Stats().AbandonedWorkers; got != 1 {
		t.Errorf("abandoned workers = %d, want exactly 1 (the single stuck worker)", got)
	}
}

func TestEngine_RateLimiterIsSharedAcrossTargets(t *testing.T) {
	fake := fsstat.NewFake(0)
	for i := 0; i < 4; i++ {
		fake.AddSyntheticFiles(fmt.Sprintf("/t%d", i), 200, 1)
	}

	// A burst far below the total work forces measurable waiting; asserting on accumulated wait
	// rather than wall-clock keeps this deterministic.
	engine := NewEngine(fake, testConfig(Config{
		Concurrency: 4, BatchSize: 32, RateLimit: 2000, RateLimitBurst: 64,
	}), nil)
	targets := []string{"/t0", "/t1", "/t2", "/t3"}
	if !engine.ScanAll(context.Background(), targets, newSink()) {
		t.Fatal("scan was skipped")
	}
	if engine.Stats().RateLimitWait <= 0 {
		t.Error("no rate-limit wait recorded; the limiter is not being consulted")
	}
}

func TestEngine_BurstIsRaisedToCoverAWholeBatch(t *testing.T) {
	// A batch is acquired with one WaitN, and WaitN fails outright when n exceeds the burst. If the
	// burst were left too small, every read would error instead of merely being slowed.
	cfg := NewEngine(fsstat.NewFake(0), Config{BatchSize: 512, RateLimit: 10}, nil).Config()
	if cfg.RateLimitBurst < cfg.BatchSize+1 {
		t.Fatalf("burst = %d, want at least %d", cfg.RateLimitBurst, cfg.BatchSize+1)
	}
}

// --- logging ------------------------------------------------------------------------------------

// TestScan_NeverLogsPerFileErrors is the enforcement point for the log-volume rule. A single wrong
// mode bit on a large tree must not be able to emit a line per failure: that would fill the disk on
// the very host whose disk usage is being monitored.
func TestScan_NeverLogsPerFileErrors(t *testing.T) {
	const failures = 2000
	fake := fsstat.NewFake(0).AddFile("/logs/trades.log", 10)
	for i := 0; i < failures; i++ {
		path := fmt.Sprintf("/logs/locked%04d", i)
		fake.AddDir(path)
		fake.SetOpenError(path, fs.ErrPermission)
	}

	handler := &capturingHandler{}
	engine := NewEngine(fake, testConfig(Config{}), slog.New(handler))
	sink := newSink()
	engine.ScanAll(context.Background(), []string{"/logs"}, sink)

	records := handler.all()
	if len(records) != 1 {
		t.Fatalf("emitted %d log records for %d failures, want exactly 1 summary line", len(records), failures)
	}
	attrs := SummaryAttrs(records[0])
	if attrs["errors_"+ClassPermission] != int64(failures) {
		t.Errorf("summary permission count = %v, want %d", attrs["errors_"+ClassPermission], failures)
	}
	if attrs["outcome"] != string(state.OutcomePartial) {
		t.Errorf("summary outcome = %v, want partial", attrs["outcome"])
	}
	if got := sink.get(t, "/logs"); got.Errors[ClassPermission] != failures {
		t.Errorf("result permission count = %d, want %d", got.Errors[ClassPermission], failures)
	}
}

func TestProgress_HeartbeatOnlyAfterTheThreshold(t *testing.T) {
	quick := fsstat.NewFake(0).AddFile("/logs/trades.log", 10)
	handler := &capturingHandler{}
	engine := NewEngine(quick, testConfig(Config{HeartbeatAfter: time.Hour}), slog.New(handler))
	engine.ScanAll(context.Background(), []string{"/logs"}, newSink())
	for _, record := range handler.all() {
		if record.Message == "Directory scan still running" {
			t.Fatal("a short scan emitted a heartbeat")
		}
	}

	slow := fsstat.NewFake(0).AddSyntheticFiles("/logs", 4000, 1)
	slow.BeforeRead(func(string) error {
		time.Sleep(5 * time.Millisecond)
		return nil
	})
	slowHandler := &capturingHandler{}
	slowEngine := NewEngine(slow, testConfig(Config{
		Concurrency: 1, BatchSize: 8,
		HeartbeatAfter: 10 * time.Millisecond, HeartbeatEvery: 10 * time.Millisecond,
	}), slog.New(slowHandler))
	slowEngine.ScanAll(context.Background(), []string{"/logs"}, newSink())

	var heartbeats int
	for _, record := range slowHandler.all() {
		if record.Message == "Directory scan still running" {
			heartbeats++
		}
	}
	// Without these, a multi-minute walk is a silent gap and a slow scan cannot be told from a hang.
	if heartbeats == 0 {
		t.Fatal("a long scan emitted no heartbeat")
	}
}

// --- error classification -----------------------------------------------------------------------

func TestClassify(t *testing.T) {
	for _, testCase := range []struct {
		name string
		err  error
		want string
	}{
		{"nil", nil, ""},
		{"permission", &fs.PathError{Err: fs.ErrPermission}, ClassPermission},
		{"not found", &fs.PathError{Err: fs.ErrNotExist}, ClassNotFound},
		{"unknown", errors.New("boom"), ClassOther},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if got := classify(testCase.err); got != testCase.want {
				t.Errorf("classify = %q, want %q", got, testCase.want)
			}
		})
	}
}

func TestIsVanished(t *testing.T) {
	if !isVanished(&fs.PathError{Err: fs.ErrNotExist}) {
		t.Error("a missing entry should count as vanished")
	}
	if isVanished(&fs.PathError{Err: fs.ErrPermission}) {
		t.Error("a permission error must not be excused as a vanished entry")
	}
}

func TestErrorTallySnapshotIsACopy(t *testing.T) {
	var tally errorTally
	tally.add(&fs.PathError{Err: fs.ErrPermission})

	snapshot := tally.snapshot()
	snapshot[ClassPermission] = 999
	if fresh := tally.snapshot(); fresh[ClassPermission] != 1 {
		t.Fatalf("snapshot aliased the tally: %v", fresh)
	}
}

func TestAllErrorClassesAreReachable(t *testing.T) {
	// Every class must be listed, so its counter series exists from process start. An alert on a
	// series that does not exist yet cannot fire.
	listed := map[string]bool{}
	for _, class := range AllErrorClasses {
		listed[class] = true
	}
	for _, class := range []string{ClassPermission, ClassNotFound, ClassIO, ClassLoop, ClassNameTooLong, ClassTooManyFiles, ClassOther} {
		if !listed[class] {
			t.Errorf("class %q is missing from AllErrorClasses", class)
		}
	}
}
