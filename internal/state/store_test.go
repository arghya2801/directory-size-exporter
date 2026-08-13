package state

import (
	"sync"
	"testing"
	"time"
)

const target = "/data/logs/venue-a"

var base = time.Date(2026, 8, 12, 9, 0, 0, 0, time.UTC)

// testClock is the injected clock. The retention rules are entirely about what happened when, so
// every assertion here would otherwise be a race against wall time.
type testClock struct{ now time.Time }

func (c *testClock) Now() time.Time          { return c.now }
func (c *testClock) advance(d time.Duration) { c.now = c.now.Add(d) }

func newTestStore(t *testing.T, opts Options) (*Store, *testClock) {
	t.Helper()
	clock := &testClock{now: base}
	opts.Now = clock.Now
	return New([]string{target}, opts), clock
}

func complete(size int64) Result {
	return Result{Target: target, Outcome: OutcomeComplete, SizeBytes: size, AllocBytes: size, Files: 1, DurationSeconds: 1}
}

func failed(outcome Outcome, size int64) Result {
	return Result{
		Target: target, Outcome: outcome, SizeBytes: size, AllocBytes: size, Files: 1,
		DurationSeconds: 1, Errors: map[string]int64{"permission": 3},
	}
}

func snapshotOf(t *testing.T, store *Store) Snapshot {
	t.Helper()
	snap, ok := store.SnapshotFor(target)
	if !ok {
		t.Fatalf("no snapshot for %s", target)
	}
	return snap
}

// TestStore_RetentionTable is the specification of the retention rules, one subtest per row.
//
// The single governing rule: the published measurement is replaced only by a scan that completed
// without errors. Every other outcome updates status alone. A partial walk, a timeout, or a
// briefly-vanished directory all produce a systematically LOW number, and publishing one reads on
// a disk-usage dashboard as space being freed — the precise false signal this exporter exists to
// avoid raising.
func TestStore_RetentionTable(t *testing.T) {
	t.Run("a: complete with a prior good value replaces it and computes delta", func(t *testing.T) {
		store, clock := newTestStore(t, Options{})
		store.Apply(complete(100))
		clock.advance(5 * time.Minute)
		store.Apply(complete(150))

		got := snapshotOf(t, store)
		assertMeasurement(t, got, 150)
		if !got.HasDelta || got.DeltaBytes != 50 {
			t.Errorf("delta = %d (present=%v), want 50", got.DeltaBytes, got.HasDelta)
		}
		if got.DeltaIntervalSeconds != 300 {
			t.Errorf("delta interval = %v, want 300", got.DeltaIntervalSeconds)
		}
		if got.BytesAddedTotal != 50 || got.BytesRemovedTotal != 0 {
			t.Errorf("counters = +%d/-%d, want +50/-0", got.BytesAddedTotal, got.BytesRemovedTotal)
		}
		if !got.Success || got.Partial || !got.Present {
			t.Errorf("status = success:%v partial:%v present:%v, want true/false/true", got.Success, got.Partial, got.Present)
		}
		if got.ScanAgeSeconds != 0 {
			t.Errorf("scan age = %v, want 0 immediately after a completed scan", got.ScanAgeSeconds)
		}
	})

	t.Run("a: a shrinking directory records removed bytes", func(t *testing.T) {
		store, clock := newTestStore(t, Options{})
		store.Apply(complete(150))
		clock.advance(time.Minute)
		store.Apply(complete(100))

		got := snapshotOf(t, store)
		if got.DeltaBytes != -50 {
			t.Errorf("delta = %d, want -50", got.DeltaBytes)
		}
		if got.BytesAddedTotal != 0 || got.BytesRemovedTotal != 50 {
			t.Errorf("counters = +%d/-%d, want +0/-50", got.BytesAddedTotal, got.BytesRemovedTotal)
		}
	})

	t.Run("a-prime: the first completed scan publishes but has no delta", func(t *testing.T) {
		store, _ := newTestStore(t, Options{})
		store.Apply(complete(100))

		got := snapshotOf(t, store)
		assertMeasurement(t, got, 100)
		// A delta needs two completed scans. Emitting one derived from an implicit zero baseline
		// would report the entire directory as freshly created on every process start.
		if got.HasDelta {
			t.Errorf("delta was published after a single scan: %+v", got)
		}
		if got.BytesAddedTotal != 0 {
			t.Errorf("bytes added = %d, want 0", got.BytesAddedTotal)
		}
	})

	for _, testCase := range []struct {
		name    string
		outcome Outcome
		partial bool
		present bool
	}{
		{"b: partial", OutcomePartial, true, true},
		{"c: timeout", OutcomeTimeout, true, true},
		{"d: missing", OutcomeMissing, false, false},
		{"d-prime: root error", OutcomeRootError, true, true},
	} {
		t.Run(testCase.name+" retains the last good value", func(t *testing.T) {
			store, clock := newTestStore(t, Options{})
			store.Apply(complete(100))
			clock.advance(5 * time.Minute)
			store.Apply(failed(testCase.outcome, 7)) // a drastically low partial reading

			got := snapshotOf(t, store)
			assertMeasurement(t, got, 100)
			if got.HasDelta {
				t.Errorf("%s produced a delta; only completed scans may move it", testCase.outcome)
			}
			if got.BytesAddedTotal != 0 || got.BytesRemovedTotal != 0 {
				t.Errorf("%s moved the byte counters", testCase.outcome)
			}
			if got.Success {
				t.Errorf("%s was reported as a success", testCase.outcome)
			}
			if got.Partial != testCase.partial {
				t.Errorf("partial = %v, want %v", got.Partial, testCase.partial)
			}
			if got.Present != testCase.present {
				t.Errorf("present = %v, want %v", got.Present, testCase.present)
			}
			// Freshness must degrade visibly, otherwise a stuck target looks healthy forever.
			if got.ScanAgeSeconds != 300 {
				t.Errorf("scan age = %v, want 300", got.ScanAgeSeconds)
			}
			if got.LastGoodTimestamp != float64(base.Unix()) {
				t.Errorf("last good timestamp moved on a failed scan")
			}
			if got.LastAttemptTimestamp != float64(base.Add(5*time.Minute).Unix()) {
				t.Errorf("last attempt timestamp = %v, want the failed scan's time", got.LastAttemptTimestamp)
			}
		})
	}

	t.Run("e: nothing is published before the first completed scan", func(t *testing.T) {
		store, _ := newTestStore(t, Options{})

		got := snapshotOf(t, store)
		// Absence is unambiguous in PromQL; a zero is a lie that looks like data and would fire
		// every capacity alert on process start, before the first walk of a multi-TB tree lands.
		if got.HasGood {
			t.Errorf("a measurement was published before any scan: %+v", got)
		}
		if got.HasDelta || got.HasAttempt {
			t.Errorf("delta or attempt data published before any scan: %+v", got)
		}
		// last_scan_success must still exist, so "never scanned" is distinguishable from
		// "exporter is down" — the latter makes the whole series vanish.
		if got.Success {
			t.Error("success should be present and false before the first scan")
		}
	})

	t.Run("e: a failed first scan still publishes no measurement", func(t *testing.T) {
		store, _ := newTestStore(t, Options{})
		store.Apply(failed(OutcomeMissing, 0))

		got := snapshotOf(t, store)
		if got.HasGood {
			t.Errorf("a failed first scan published a measurement: %+v", got)
		}
		if !got.HasAttempt {
			t.Error("the attempt itself should be recorded")
		}
		if got.Present {
			t.Error("a missing target should not be reported as present")
		}
	})

	t.Run("f: a cancelled scan changes nothing at all", func(t *testing.T) {
		store, clock := newTestStore(t, Options{})
		store.Apply(complete(100))
		before := snapshotOf(t, store)

		clock.advance(time.Minute)
		store.Apply(Result{Target: target, Outcome: OutcomeCancelled, SizeBytes: 3})
		after := snapshotOf(t, store)

		// Shutdown must not perturb the freshness picture: a SIGTERM mid-scan is not evidence
		// about the directory, so even the attempt timestamp stays put.
		if after.LastAttemptTimestamp != before.LastAttemptTimestamp {
			t.Errorf("cancellation moved the attempt timestamp: %v -> %v", before.LastAttemptTimestamp, after.LastAttemptTimestamp)
		}
		if after.SizeBytes != before.SizeBytes || after.Success != before.Success || after.Partial != before.Partial {
			t.Errorf("cancellation changed published state: %+v -> %+v", before, after)
		}
		if after.ScansTotal[OutcomeCancelled] != 1 {
			t.Errorf("cancellation was not counted: %v", after.ScansTotal)
		}
	})
}

// TestStore_DeltaSpansOnlyCompletedScans is the reason DeltaIntervalSeconds exists. Dividing the
// delta by the configured scan interval would be threefold wrong here, and the resulting
// "hours until the volume fills" forecast wrong by the same factor.
func TestStore_DeltaSpansOnlyCompletedScans(t *testing.T) {
	store, clock := newTestStore(t, Options{})
	store.Apply(complete(100))
	clock.advance(5 * time.Minute)
	store.Apply(failed(OutcomePartial, 40))
	clock.advance(5 * time.Minute)
	store.Apply(failed(OutcomeTimeout, 60))
	clock.advance(5 * time.Minute)
	store.Apply(complete(150))

	got := snapshotOf(t, store)
	if got.DeltaBytes != 50 {
		t.Errorf("delta = %d, want 50 measured against the last COMPLETED scan", got.DeltaBytes)
	}
	if got.DeltaIntervalSeconds != 900 {
		t.Errorf("delta interval = %v, want 900 (three intervals, not one)", got.DeltaIntervalSeconds)
	}
}

func TestStore_ByteCountersAreMonotonicAcrossMixedOutcomes(t *testing.T) {
	store, clock := newTestStore(t, Options{})
	sizes := []int64{100, 300, 250, 250, 900, 10, 40}
	outcomes := []Outcome{OutcomeComplete, OutcomePartial, OutcomeComplete, OutcomeTimeout, OutcomeComplete, OutcomeMissing, OutcomeComplete}

	var lastAdded, lastRemoved uint64
	var firstGood, lastGood int64
	seenGood := false
	for i, size := range sizes {
		clock.advance(time.Minute)
		if outcomes[i] == OutcomeComplete {
			store.Apply(complete(size))
			if !seenGood {
				firstGood, seenGood = size, true
			}
			lastGood = size
		} else {
			store.Apply(failed(outcomes[i], size))
		}

		got := snapshotOf(t, store)
		if got.BytesAddedTotal < lastAdded || got.BytesRemovedTotal < lastRemoved {
			t.Fatalf("step %d: counters went backwards: +%d/-%d then +%d/-%d",
				i, lastAdded, lastRemoved, got.BytesAddedTotal, got.BytesRemovedTotal)
		}
		lastAdded, lastRemoved = got.BytesAddedTotal, got.BytesRemovedTotal
	}

	// Net movement across all completed scans must reconcile with the endpoints, proving no failed
	// scan leaked into the counters.
	net := int64(lastAdded) - int64(lastRemoved)
	if want := lastGood - firstGood; net != want {
		t.Errorf("net counter movement = %d, want %d", net, want)
	}
}

func TestStore_PublishPartialStillFreezesDeltaAndCounters(t *testing.T) {
	store, clock := newTestStore(t, Options{PublishPartial: true})
	store.Apply(complete(100))
	clock.advance(5 * time.Minute)
	store.Apply(failed(OutcomePartial, 60))

	got := snapshotOf(t, store)
	// The operator asked to see partial numbers, so the size is published...
	if !got.HasGood || got.SizeBytes != 60 {
		t.Errorf("size = %d (present=%v), want the partial value 60", got.SizeBytes, got.HasGood)
	}
	if !got.Partial {
		t.Error("a published partial must still be flagged as partial")
	}
	// ...but a known-low reading must never be turned into growth data, which is what alerting
	// and capacity forecasting actually consume.
	if got.HasDelta {
		t.Error("a partial result produced a delta")
	}
	if got.BytesAddedTotal != 0 || got.BytesRemovedTotal != 0 {
		t.Errorf("a partial result moved the byte counters: +%d/-%d", got.BytesAddedTotal, got.BytesRemovedTotal)
	}
	if got.ScanAgeSeconds != 300 {
		t.Errorf("scan age = %v, want 300; publishing a partial does not make it fresh", got.ScanAgeSeconds)
	}

	// The next completed scan measures against the last COMPLETED one, not the published partial.
	clock.advance(5 * time.Minute)
	store.Apply(complete(140))
	if got := snapshotOf(t, store); got.DeltaBytes != 40 {
		t.Errorf("delta = %d, want 40 measured from the last completed scan of 100", got.DeltaBytes)
	}
}

func TestStore_StaleAfterStopsPublishingTheMeasurement(t *testing.T) {
	store, clock := newTestStore(t, Options{StaleAfter: 10 * time.Minute})
	store.Apply(complete(100))

	clock.advance(9 * time.Minute)
	if got := snapshotOf(t, store); !got.HasGood {
		t.Fatal("measurement withdrawn before the staleness threshold")
	}
	clock.advance(2 * time.Minute)
	got := snapshotOf(t, store)
	// A deleted directory would otherwise report its final size forever.
	if got.HasGood {
		t.Error("a stale measurement was still published")
	}
	// Status must survive so the staleness itself remains visible and alertable.
	if !got.HasAttempt || got.ScanAgeSeconds != 660 {
		t.Errorf("status was withdrawn along with the measurement: %+v", got)
	}
}

func TestStore_InProgressTracksTheRunningScan(t *testing.T) {
	store, clock := newTestStore(t, Options{})
	store.Apply(complete(100))

	store.MarkScanStarted(target)
	clock.advance(90 * time.Second)

	got := snapshotOf(t, store)
	if !got.InProgress {
		t.Error("a running scan was not reported as in progress")
	}
	if got.CurrentScanDurationSeconds != 90 {
		t.Errorf("current scan duration = %v, want 90", got.CurrentScanDurationSeconds)
	}
	// The previous good value keeps being served throughout, which is the entire point: a long
	// walk must not blank the dashboard.
	if !got.HasGood || got.SizeBytes != 100 {
		t.Errorf("previous good value not served during a scan: %+v", got)
	}

	store.Apply(complete(120))
	got = snapshotOf(t, store)
	if got.InProgress || got.CurrentScanDurationSeconds != 0 {
		t.Errorf("scan still marked in progress after completing: %+v", got)
	}
}

func TestStore_AccumulatesPerOutcomeAndPerClassCounters(t *testing.T) {
	store, clock := newTestStore(t, Options{})
	store.Apply(complete(100))
	clock.advance(time.Minute)
	store.Apply(failed(OutcomePartial, 50))
	clock.advance(time.Minute)
	store.Apply(failed(OutcomePartial, 50))

	got := snapshotOf(t, store)
	if got.ScansTotal[OutcomeComplete] != 1 || got.ScansTotal[OutcomePartial] != 2 {
		t.Errorf("scan counts = %v, want 1 complete and 2 partial", got.ScansTotal)
	}
	if got.ErrorsTotal["permission"] != 6 {
		t.Errorf("cumulative permission errors = %d, want 6", got.ErrorsTotal["permission"])
	}
	if got.LastErrors["permission"] != 3 {
		t.Errorf("last-scan permission errors = %d, want 3", got.LastErrors["permission"])
	}
}

func TestStore_SetTargetsAddsAndRemovesSeries(t *testing.T) {
	store, _ := newTestStore(t, Options{})
	store.Apply(complete(100))

	other := "/data/logs/venue-b"
	store.SetTargets([]string{target, other})
	if len(store.Snapshot()) != 2 {
		t.Fatalf("expected 2 targets after adding one, got %d", len(store.Snapshot()))
	}
	if snap, ok := store.SnapshotFor(target); !ok || snap.SizeBytes != 100 {
		t.Error("adding a target disturbed an existing target's retained value")
	}

	// A target that disappears from a glob must have its series garbage-collected, not left to
	// report a frozen size forever.
	store.SetTargets([]string{other})
	if _, ok := store.SnapshotFor(target); ok {
		t.Error("a removed target still reports a snapshot")
	}
	if len(store.Snapshot()) != 1 {
		t.Errorf("expected 1 target after removal, got %d", len(store.Snapshot()))
	}
}

func TestStore_IgnoresResultsForUnknownTargets(t *testing.T) {
	store, _ := newTestStore(t, Options{})
	// A scan finishing after its target was dropped by a config reload must not resurrect it.
	store.Apply(Result{Target: "/data/logs/gone", Outcome: OutcomeComplete, SizeBytes: 5})
	if _, ok := store.SnapshotFor("/data/logs/gone"); ok {
		t.Error("a result for an unknown target created a series")
	}
}

// TestStore_ConcurrentApplyAndSnapshot is meaningful only under -race, which needs cgo and
// therefore runs in Linux CI rather than on the Windows development machine.
func TestStore_ConcurrentApplyAndSnapshot(t *testing.T) {
	targets := []string{"/a", "/b", "/c", "/d"}
	store := New(targets, Options{Now: time.Now})

	var group sync.WaitGroup
	for _, name := range targets {
		group.Add(1)
		go func(name string) {
			defer group.Done()
			for i := 0; i < 200; i++ {
				store.MarkScanStarted(name)
				store.Apply(Result{Target: name, Outcome: OutcomeComplete, SizeBytes: int64(i)})
			}
		}(name)
	}
	for i := 0; i < 4; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			for i := 0; i < 200; i++ {
				for _, snap := range store.Snapshot() {
					_ = snap.SizeBytes
					_ = snap.LastErrors
				}
			}
		}()
	}
	group.Wait()

	for _, name := range targets {
		snap, ok := store.SnapshotFor(name)
		if !ok || snap.SizeBytes != 199 {
			t.Errorf("%s final size = %d (ok=%v), want 199", name, snap.SizeBytes, ok)
		}
	}
}

// TestStore_SnapshotDoesNotAliasInternalMaps guards against a scrape mutating store state, which
// would be a data race that -race cannot see because it is a logic error, not a memory one.
func TestStore_SnapshotDoesNotAliasInternalMaps(t *testing.T) {
	store, _ := newTestStore(t, Options{})
	store.Apply(failed(OutcomePartial, 10))

	snap := snapshotOf(t, store)
	snap.LastErrors["permission"] = 999
	snap.ErrorsTotal["permission"] = 999
	snap.ScansTotal[OutcomeComplete] = 999

	fresh := snapshotOf(t, store)
	if fresh.LastErrors["permission"] != 3 || fresh.ErrorsTotal["permission"] != 3 {
		t.Errorf("mutating a snapshot changed store state: %+v", fresh)
	}
	if fresh.ScansTotal[OutcomeComplete] != 0 {
		t.Errorf("mutating a snapshot changed scan counts: %v", fresh.ScansTotal)
	}
}

func assertMeasurement(t *testing.T, got Snapshot, wantSize int64) {
	t.Helper()
	if !got.HasGood {
		t.Fatalf("no measurement published: %+v", got)
	}
	if got.SizeBytes != wantSize {
		t.Errorf("size = %d, want %d", got.SizeBytes, wantSize)
	}
}
