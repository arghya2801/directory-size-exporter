// Package collector renders the exporter's published state as Prometheus metrics.
//
// It makes no decisions of its own. Whether a measurement may be published at all is settled in
// internal/state, and whether a platform can produce one is settled in internal/fsstat. This
// package only translates, which is what keeps the retention rules testable without Prometheus.
//
// Collection never touches the filesystem. A scrape reads an in-memory snapshot, so a scrape can
// never be slowed by a hung mount and a slow scan can never delay a scrape.
package collector

import (
	"github.com/prometheus/client_golang/prometheus"

	"github.com/local/directory-size-exporter/internal/fsstat"
	"github.com/local/directory-size-exporter/internal/scan"
	"github.com/local/directory-size-exporter/internal/state"
)

// Options gates optional metric families. Each corresponds to a command-line flag.
type Options struct {
	FileCounts     bool
	DiskUsage      bool
	Growth         bool
	Filesystem     bool
	DedupHardlinks bool
	OneFilesystem  bool
	ScanHistogram  bool
}

// StatsProvider supplies engine-wide counters. *scan.Engine satisfies it.
type StatsProvider interface {
	Stats() scan.Stats
}

// Collector publishes per-target and engine-wide metrics.
type Collector struct {
	store   *state.Store
	engine  StatsProvider
	fsCache *FilesystemCache
	caps    fsstat.Capability
	opts    Options
	descs   *descs

	histogram *prometheus.HistogramVec
}

// scanDurationBuckets span a single-digit-seconds scan of a small directory through a multi-hour
// walk of a multi-terabyte tree. Prometheus defaults top out at ten seconds, which would place
// every real scan on this workload in the +Inf bucket.
var scanDurationBuckets = []float64{1, 2, 5, 10, 30, 60, 120, 300, 600, 1800, 3600, 7200}

// New builds a collector over the given store.
func New(store *state.Store, engine StatsProvider, fsCache *FilesystemCache, caps fsstat.Capability, opts Options) *Collector {
	c := &Collector{store: store, engine: engine, fsCache: fsCache, caps: caps, opts: opts, descs: newDescs()}
	if opts.ScanHistogram {
		// Deliberately not labelled by target: twenty targets times six outcomes times thirteen
		// buckets is over fifteen hundred series for a single exporter. Per-target duration is
		// already available as last_scan_duration_seconds.
		//
		// Outcomes are deliberately NOT pre-initialised either. Pre-creating them is worth it for
		// counters, where a series that does not exist cannot be alerted on, but a histogram of
		// durations for an outcome that has never happened is fifteen series describing the
		// distribution of nothing. They appear when the outcome first occurs.
		c.histogram = prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: Namespace,
			Name:      "scan_duration_seconds",
			Help:      "Distribution of scan durations by outcome, across all targets.",
			Buckets:   scanDurationBuckets,
		}, []string{"result"})
	}
	return c
}

// Recorder returns a scan.Sink that updates retention state and, when enabled, the duration
// histogram. Wiring both through one sink keeps them from drifting apart.
func (c *Collector) Recorder() scan.Sink { return &recorder{collector: c} }

type recorder struct{ collector *Collector }

func (r *recorder) MarkScanStarted(target string) { r.collector.store.MarkScanStarted(target) }

func (r *recorder) Apply(result state.Result) {
	r.collector.store.Apply(result)
	if r.collector.histogram != nil && result.Outcome != state.OutcomeCancelled {
		// A cancelled scan was cut short by shutdown at an arbitrary point, so its duration says
		// nothing about how long scanning this target actually takes.
		r.collector.histogram.WithLabelValues(string(result.Outcome)).Observe(result.DurationSeconds)
	}
}

// Describe advertises every descriptor the collector may emit, including those currently gated
// off. Describing the full surface keeps registration checked, so the registry catches duplicate
// series and label-consistency mistakes rather than letting them reach a scrape.
func (c *Collector) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range c.descs.all {
		ch <- d
	}
	if c.histogram != nil {
		c.histogram.Describe(ch)
	}
}

func (c *Collector) Collect(ch chan<- prometheus.Metric) {
	if c.histogram != nil {
		c.histogram.Collect(ch)
	}

	snapshots := c.store.Snapshot()
	for i := range snapshots {
		c.collectTarget(ch, &snapshots[i])
	}
	c.collectEngine(ch, len(snapshots))
	if c.opts.Filesystem && c.fsCache != nil {
		c.collectFilesystems(ch)
	}
}

func (c *Collector) collectTarget(ch chan<- prometheus.Metric, snap *state.Snapshot) {
	target := snap.Target
	d := c.descs

	// Status is published from process start, so "never scanned" is distinguishable from "the
	// exporter is down" — the latter makes every series for the target disappear.
	gauge(ch, d.success, boolValue(snap.Success), target)
	gauge(ch, d.partial, boolValue(snap.Partial), target)
	gauge(ch, d.present, boolValue(snap.Present), target)
	gauge(ch, d.inProgress, boolValue(snap.InProgress), target)
	gauge(ch, d.currentScan, snap.CurrentScanDurationSeconds, target)

	for _, outcome := range state.AllOutcomes {
		counter(ch, d.scansTotal, float64(snap.ScansTotal[outcome]), target, string(outcome))
	}
	for _, class := range scan.AllErrorClasses {
		gauge(ch, d.lastErrors, float64(snap.LastErrors[class]), target, class)
		counter(ch, d.errorsTotal, float64(snap.ErrorsTotal[class]), target, class)
	}
	counter(ch, d.vanishedTotal, float64(snap.VanishedTotal), target)
	counter(ch, d.entriesTotal, float64(snap.EntriesTotal), target)
	counter(ch, d.statCallsTotal, float64(snap.StatCallsTotal), target)
	counter(ch, d.dirsReadTotal, float64(snap.DirsReadTotal), target)
	if c.opts.OneFilesystem {
		counter(ch, d.skippedXdev, float64(snap.SkippedOtherFilesystemTotal), target)
	}

	if snap.HasAttempt {
		gauge(ch, d.lastAttempt, snap.LastAttemptTimestamp, target)
		gauge(ch, d.lastDuration, snap.LastScanDurationSeconds, target)
	}
	if snap.LastGoodTimestamp > 0 {
		// Freshness is reported even once the measurement itself has gone stale and been withdrawn,
		// so the staleness stays visible instead of the target going silent.
		gauge(ch, d.lastGoodTime, snap.LastGoodTimestamp, target)
		gauge(ch, d.scanAge, snap.ScanAgeSeconds, target)
	}

	if !snap.HasGood {
		// Nothing measurable yet. Emitting zero here would be indistinguishable from a genuinely
		// empty directory and would fire capacity alerts during the first walk of a large tree.
		return
	}

	gauge(ch, d.size, float64(snap.SizeBytes), target)
	if c.opts.DiskUsage && c.caps.Has(fsstat.CapAllocBytes) {
		gauge(ch, d.diskUsage, float64(snap.AllocBytes), target)
	}
	if c.opts.FileCounts {
		gauge(ch, d.files, float64(snap.Files), target)
		gauge(ch, d.dirs, float64(snap.Directories), target)
	}
	if c.opts.DedupHardlinks {
		gauge(ch, d.hardlinked, float64(snap.HardlinkedFiles), target)
		gauge(ch, d.dedupSaved, float64(snap.DedupSavedBytes), target)
		gauge(ch, d.dedupBroke, boolValue(snap.HardlinkTrackingExhausted), target)
	}
	if c.opts.Growth {
		counter(ch, d.bytesAdded, float64(snap.BytesAddedTotal), target)
		counter(ch, d.bytesRemoved, float64(snap.BytesRemovedTotal), target)
		if snap.HasDelta {
			gauge(ch, d.delta, float64(snap.DeltaBytes), target)
			gauge(ch, d.deltaInterval, snap.DeltaIntervalSeconds, target)
		}
	}
}

func (c *Collector) collectEngine(ch chan<- prometheus.Metric, targetCount int) {
	stats := c.engine.Stats()
	counter(ch, c.descs.scansSkipped, float64(stats.ScansSkipped))
	gauge(ch, c.descs.abandonedWorkers, float64(stats.AbandonedWorkers))
	counter(ch, c.descs.rateLimitWait, stats.RateLimitWait)
	gauge(ch, c.descs.targetsResolved, float64(targetCount))

	// Published for every capability, present or absent, so a rule can assert that a production
	// host is running a correctly built binary rather than one silently missing disk-usage support.
	for _, capability := range fsstat.AllCapabilities {
		gauge(ch, c.descs.capability, boolValue(c.caps.Has(capability.Bit)), capability.Label)
	}
}

// --- small helpers ---------------------------------------------------------------------------

func gauge(ch chan<- prometheus.Metric, d *prometheus.Desc, value float64, labels ...string) {
	ch <- prometheus.MustNewConstMetric(d, prometheus.GaugeValue, value, labels...)
}

func counter(ch chan<- prometheus.Metric, d *prometheus.Desc, value float64, labels ...string) {
	ch <- prometheus.MustNewConstMetric(d, prometheus.CounterValue, value, labels...)
}

func boolValue(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
