// Package state holds the retention rules that decide what the exporter is allowed to publish.
//
// It is deliberately free of any dependency on Prometheus and on the filesystem. The rules here
// are the highest-risk correctness surface in the exporter — they are what stands between a failed
// scan and a dashboard that reads as though a terabyte of trade logs was deleted — so they are
// expressed as pure logic over an injected clock and tested exhaustively in isolation.
//
// The governing rule, from which everything else follows:
//
//	The published measurement is replaced ONLY by a scan that completed without errors.
//	Every other outcome updates status metrics alone.
//
// A partial walk, a timeout, or a directory that briefly vanished all produce a systematically LOW
// number. Publishing one is indistinguishable from the directory genuinely shrinking.
package state

import (
	"sync"
	"time"
)

// Outcome classifies how a scan ended. The distinctions matter: a timeout and a partial error both
// withhold publication but need different alerts, and a cancelled scan must not count as evidence
// about the directory at all.
type Outcome string

const (
	// OutcomeComplete means the whole tree was walked with no errors. Only this publishes.
	OutcomeComplete Outcome = "complete"
	// OutcomePartial means the walk finished but some entries could not be read.
	OutcomePartial Outcome = "partial"
	// OutcomeTimeout means the scan deadline elapsed before the walk finished.
	OutcomeTimeout Outcome = "timeout"
	// OutcomeCancelled means the process is shutting down. This is not evidence about the target.
	OutcomeCancelled Outcome = "cancelled"
	// OutcomeMissing means the target does not exist or is not a directory.
	OutcomeMissing Outcome = "missing"
	// OutcomeRootError means the target exists but its root could not be opened.
	OutcomeRootError Outcome = "root_error"
)

// AllOutcomes lists every outcome, so counter series exist from process start rather than
// appearing only after the corresponding failure first happens.
var AllOutcomes = []Outcome{
	OutcomeComplete, OutcomePartial, OutcomeTimeout, OutcomeCancelled, OutcomeMissing, OutcomeRootError,
}

// Result is one finished scan of one target.
type Result struct {
	Target  string
	Outcome Outcome

	// Measurement. Meaningful only when Outcome is OutcomeComplete, or when PublishPartial is set
	// and the walk produced numbers at all.
	SizeBytes   int64
	AllocBytes  int64
	Files       int64
	Directories int64

	HardlinkedFiles           int64
	DedupSavedBytes           int64
	HardlinkTrackingExhausted bool

	DurationSeconds float64

	// Errors counts failures by class (permission, io, loop, ...). Entries that vanished mid-walk
	// are tracked separately in VanishedFiles because log rotation makes them routine, not faults.
	Errors        map[string]int64
	VanishedFiles int64

	// Scan cost, for tuning the walk against its impact on the host.
	EntriesScanned         int64
	StatCalls              int64
	DirsRead               int64
	SkippedOtherFilesystem int64
}

// Snapshot is the complete published view of one target at one instant. The collector renders it
// directly and makes no decisions of its own.
//
// The Has* flags carry the difference between "zero" and "no value". A metric that is absent is
// unambiguous in PromQL; a metric that is present and zero looks exactly like a real measurement.
type Snapshot struct {
	Target string

	// HasAttempt reports whether any scan has finished for this target.
	HasAttempt bool
	// Success is true only after a scan that completed with no errors.
	Success bool
	// Partial marks a result known to be incomplete.
	Partial bool
	// Present reports whether the target existed at the last attempt.
	Present bool

	// HasGood reports whether a measurement may be published at all.
	HasGood                   bool
	SizeBytes                 int64
	AllocBytes                int64
	Files                     int64
	Directories               int64
	HardlinkedFiles           int64
	DedupSavedBytes           int64
	HardlinkTrackingExhausted bool

	LastGoodTimestamp float64
	ScanAgeSeconds    float64

	// HasDelta is false until two scans have completed; a delta against an implicit zero would
	// report the whole directory as newly created on every restart.
	HasDelta             bool
	DeltaBytes           int64
	DeltaIntervalSeconds float64

	BytesAddedTotal   uint64
	BytesRemovedTotal uint64

	LastAttemptTimestamp    float64
	LastScanDurationSeconds float64
	LastErrors              map[string]int64
	ErrorsTotal             map[string]uint64
	VanishedTotal           uint64
	ScansTotal              map[Outcome]uint64

	EntriesTotal                uint64
	StatCallsTotal              uint64
	DirsReadTotal               uint64
	SkippedOtherFilesystemTotal uint64

	InProgress                 bool
	CurrentScanDurationSeconds float64
}

// Options configures the retention rules.
type Options struct {
	// Now is the clock. Defaults to time.Now.
	Now func() time.Time

	// PublishPartial publishes the measurement from a partial scan. It stays false by default
	// because a partial reading is systematically low and reads as freed space. Even when enabled,
	// deltas and the byte counters remain frozen: an operator may want to see a rough number, but
	// a known-low reading must never become growth data feeding a capacity forecast.
	PublishPartial bool

	// StaleAfter withdraws the measurement once it is older than this. Zero never withdraws.
	// Without it, a deleted directory reports its final size forever.
	StaleAfter time.Duration
}

// Store holds retention state for every configured target.
type Store struct {
	mu      sync.RWMutex
	now     func() time.Time
	opts    Options
	targets map[string]*targetState
	order   []string
}

type targetState struct {
	// published is what the collector renders. It moves on a completed scan, and also on a partial
	// one when PublishPartial is set.
	published    measurement
	hasPublished bool

	// good is the baseline for deltas, and moves ONLY on a completed scan. Keeping it separate
	// from published is what lets PublishPartial show a rough number without corrupting growth.
	good    measurement
	hasGood bool
	goodAt  time.Time

	// previousGood supports the delta, which must span completed scans only.
	previousGoodAt   time.Time
	hasPreviousGood  bool
	previousGoodSize int64

	hasAttempt   bool
	attemptAt    time.Time
	lastDuration float64
	success      bool
	partial      bool
	present      bool
	lastErrors   map[string]int64

	bytesAdded   uint64
	bytesRemoved uint64

	errorsTotal   map[string]uint64
	vanishedTotal uint64
	scansTotal    map[Outcome]uint64

	entriesTotal     uint64
	statCallsTotal   uint64
	dirsReadTotal    uint64
	skippedXdevTotal uint64

	scanStartedAt time.Time
	scanning      bool
}

type measurement struct {
	size                      int64
	alloc                     int64
	files                     int64
	directories               int64
	hardlinkedFiles           int64
	dedupSavedBytes           int64
	hardlinkTrackingExhausted bool
}

func measurementOf(r Result) measurement {
	return measurement{
		size:                      r.SizeBytes,
		alloc:                     r.AllocBytes,
		files:                     r.Files,
		directories:               r.Directories,
		hardlinkedFiles:           r.HardlinkedFiles,
		dedupSavedBytes:           r.DedupSavedBytes,
		hardlinkTrackingExhausted: r.HardlinkTrackingExhausted,
	}
}

// New returns a Store tracking the given targets.
func New(targets []string, opts Options) *Store {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	store := &Store{now: opts.Now, opts: opts, targets: make(map[string]*targetState, len(targets))}
	store.SetTargets(targets)
	return store
}

// SetTargets replaces the tracked set, retaining state for targets that survive.
//
// Removing a target drops its state entirely so its series disappear. A target that vanished from
// a glob must stop reporting rather than freeze at its final size forever.
func (s *Store) SetTargets(targets []string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	next := make(map[string]*targetState, len(targets))
	order := make([]string, 0, len(targets))
	for _, target := range targets {
		if _, duplicate := next[target]; duplicate {
			continue
		}
		if existing, ok := s.targets[target]; ok {
			next[target] = existing
		} else {
			next[target] = newTargetState()
		}
		order = append(order, target)
	}
	s.targets, s.order = next, order
}

func newTargetState() *targetState {
	return &targetState{
		lastErrors:  map[string]int64{},
		errorsTotal: map[string]uint64{},
		scansTotal:  map[Outcome]uint64{},
	}
}

// MarkScanStarted records that a walk of target is under way, so the in-flight metrics can
// distinguish a slow scan from a stalled one while the previous value keeps being served.
func (s *Store) MarkScanStarted(target string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if state, ok := s.targets[target]; ok {
		state.scanning, state.scanStartedAt = true, s.now()
	}
}

// Apply folds a finished scan into the retention state. This is the whole retention table.
func (s *Store) Apply(r Result) {
	s.mu.Lock()
	defer s.mu.Unlock()

	state, ok := s.targets[r.Target]
	if !ok {
		// A scan that finishes after a config reload dropped its target must not resurrect it.
		return
	}
	state.scanning = false
	state.scansTotal[r.Outcome]++

	// A cancelled scan says nothing about the directory — the process was told to stop, and the
	// walk was cut short at an arbitrary point. Recording it as an attempt would make the next
	// scrape look fresher than it is, so it is a complete no-op beyond its own counter.
	if r.Outcome == OutcomeCancelled {
		return
	}

	now := s.now()
	state.hasAttempt = true
	state.attemptAt = now
	state.lastDuration = r.DurationSeconds
	state.success = r.Outcome == OutcomeComplete
	state.partial = r.Outcome != OutcomeComplete && r.Outcome != OutcomeMissing
	state.present = r.Outcome != OutcomeMissing

	state.lastErrors = make(map[string]int64, len(r.Errors))
	for class, count := range r.Errors {
		state.lastErrors[class] = count
		state.errorsTotal[class] += uint64(count)
	}
	state.vanishedTotal += uint64(r.VanishedFiles)
	state.entriesTotal += uint64(r.EntriesScanned)
	state.statCallsTotal += uint64(r.StatCalls)
	state.dirsReadTotal += uint64(r.DirsRead)
	state.skippedXdevTotal += uint64(r.SkippedOtherFilesystem)

	if r.Outcome != OutcomeComplete {
		// Everything below this line moves the published measurement, and only a clean scan has
		// earned the right to do that. The one concession is PublishPartial, which shows the
		// number without letting it become a baseline.
		if s.opts.PublishPartial && r.Outcome == OutcomePartial {
			state.published, state.hasPublished = measurementOf(r), true
		}
		return
	}

	if state.hasGood {
		state.previousGoodSize, state.previousGoodAt, state.hasPreviousGood = state.good.size, state.goodAt, true
		delta := r.SizeBytes - state.good.size
		if delta > 0 {
			state.bytesAdded += uint64(delta)
		} else {
			state.bytesRemoved += uint64(-delta)
		}
	}
	state.good, state.hasGood, state.goodAt = measurementOf(r), true, now
	state.published, state.hasPublished = state.good, true
}

// Snapshot returns the published view of every target, in configuration order.
func (s *Store) Snapshot() []Snapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	now := s.now()
	snapshots := make([]Snapshot, 0, len(s.order))
	for _, target := range s.order {
		snapshots = append(snapshots, s.snapshotLocked(target, s.targets[target], now))
	}
	return snapshots
}

// Targets returns the tracked target paths in configuration order.
//
// Separate from Snapshot because a caller that only needs the list should not pay for the deep
// copies a scrape requires: Snapshot duplicates three maps per target so a scrape cannot mutate
// store state, and on the scan path every one of those allocations is discarded immediately.
func (s *Store) Targets() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]string(nil), s.order...)
}

// SnapshotFor returns the published view of one target.
func (s *Store) SnapshotFor(target string) (Snapshot, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	state, ok := s.targets[target]
	if !ok {
		return Snapshot{}, false
	}
	return s.snapshotLocked(target, state, s.now()), true
}

func (s *Store) snapshotLocked(target string, state *targetState, now time.Time) Snapshot {
	snap := Snapshot{
		Target:                      target,
		HasAttempt:                  state.hasAttempt,
		Success:                     state.success,
		Partial:                     state.partial,
		Present:                     state.present,
		BytesAddedTotal:             state.bytesAdded,
		BytesRemovedTotal:           state.bytesRemoved,
		VanishedTotal:               state.vanishedTotal,
		EntriesTotal:                state.entriesTotal,
		StatCallsTotal:              state.statCallsTotal,
		DirsReadTotal:               state.dirsReadTotal,
		SkippedOtherFilesystemTotal: state.skippedXdevTotal,
		LastErrors:                  copyIntMap(state.lastErrors),
		ErrorsTotal:                 copyUintMap(state.errorsTotal),
		ScansTotal:                  copyOutcomeMap(state.scansTotal),
	}
	if state.hasAttempt {
		snap.LastAttemptTimestamp = float64(state.attemptAt.Unix())
		snap.LastScanDurationSeconds = state.lastDuration
	}
	if state.scanning {
		snap.InProgress = true
		snap.CurrentScanDurationSeconds = now.Sub(state.scanStartedAt).Seconds()
	}
	if !state.hasGood {
		// No completed scan yet. Publishing a partial reading here is still allowed, but there is
		// no freshness or delta to report against.
		snap.HasGood = state.hasPublished
		if snap.HasGood {
			applyMeasurement(&snap, state.published)
		}
		return snap
	}

	age := now.Sub(state.goodAt).Seconds()
	snap.LastGoodTimestamp = float64(state.goodAt.Unix())
	snap.ScanAgeSeconds = age

	// Beyond the staleness threshold the measurement is withdrawn but the status stays, so the
	// staleness itself remains visible and alertable rather than the target going silent.
	if s.opts.StaleAfter > 0 && age > s.opts.StaleAfter.Seconds() {
		return snap
	}

	snap.HasGood = state.hasPublished
	applyMeasurement(&snap, state.published)

	if state.hasPreviousGood {
		snap.HasDelta = true
		snap.DeltaBytes = state.good.size - state.previousGoodSize
		// Measured across completed scans, which may span several intervals if scans failed in
		// between. Dividing the delta by the configured interval instead would be wrong by exactly
		// that factor, and so would any forecast derived from it.
		snap.DeltaIntervalSeconds = state.goodAt.Sub(state.previousGoodAt).Seconds()
	}
	return snap
}

func applyMeasurement(snap *Snapshot, m measurement) {
	snap.SizeBytes = m.size
	snap.AllocBytes = m.alloc
	snap.Files = m.files
	snap.Directories = m.directories
	snap.HardlinkedFiles = m.hardlinkedFiles
	snap.DedupSavedBytes = m.dedupSavedBytes
	snap.HardlinkTrackingExhausted = m.hardlinkTrackingExhausted
}

// The maps below are copied rather than shared. A scrape must never be able to mutate store state,
// and the collector hands these to Prometheus while scans continue writing.

func copyIntMap(source map[string]int64) map[string]int64 {
	out := make(map[string]int64, len(source))
	for key, value := range source {
		out[key] = value
	}
	return out
}

func copyUintMap(source map[string]uint64) map[string]uint64 {
	out := make(map[string]uint64, len(source))
	for key, value := range source {
		out[key] = value
	}
	return out
}

func copyOutcomeMap(source map[Outcome]uint64) map[Outcome]uint64 {
	out := make(map[Outcome]uint64, len(source))
	for key, value := range source {
		out[key] = value
	}
	return out
}
