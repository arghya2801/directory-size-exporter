package collector

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/local/directory-size-exporter/internal/fsstat"
	"github.com/local/directory-size-exporter/internal/scan"
	"github.com/local/directory-size-exporter/internal/state"
)

const target = "/data/logs/venue-a"

type stubStats struct{ stats scan.Stats }

func (s stubStats) Stats() scan.Stats { return s.stats }

func allOptions() Options {
	return Options{
		FileCounts: true, DiskUsage: true, Growth: true,
		Filesystem: true, DedupHardlinks: true, OneFilesystem: true, ScanHistogram: true,
	}
}

func newHarness(t *testing.T, opts Options, caps fsstat.Capability) (*state.Store, *Collector, *prometheus.Registry) {
	t.Helper()
	store := state.New([]string{target}, state.Options{Now: time.Now})
	cache := NewFilesystemCache(fsstat.NewFake(caps), time.Second, nil)
	collector := New(store, stubStats{}, cache, caps, opts)
	registry := prometheus.NewRegistry()
	registry.MustRegister(collector)
	return store, collector, registry
}

func completeScan(size int64) state.Result {
	return state.Result{
		Target: target, Outcome: state.OutcomeComplete,
		SizeBytes: size, AllocBytes: size + 512, Files: 3, Directories: 1,
		DurationSeconds: 2,
	}
}

// TestCollector_LintPasses catches malformed help text, wrong unit suffixes and counters missing a
// _total suffix automatically, which is cheaper than reviewing every descriptor by hand.
func TestCollector_LintPasses(t *testing.T) {
	store, collector, _ := newHarness(t, allOptions(), fsstat.CapAllocBytes|fsstat.CapInode|fsstat.CapFSBytes)
	store.Apply(completeScan(100))

	problems, err := testutil.CollectAndLint(collector)
	if err != nil {
		t.Fatal(err)
	}
	for _, problem := range problems {
		t.Errorf("%s: %s", problem.Metric, problem.Text)
	}
}

// TestCollector_NoMeasurementBeforeFirstScan is the published-metric half of the retention rules.
// A zero here would be indistinguishable from an empty directory and would fire capacity alerts
// during the first walk of a large tree.
func TestCollector_NoMeasurementBeforeFirstScan(t *testing.T) {
	_, collector, _ := newHarness(t, allOptions(), fsstat.CapAllocBytes)

	for _, name := range []string{
		Namespace + "_size_bytes",
		Namespace + "_disk_usage_bytes",
		Namespace + "_files",
		Namespace + "_size_delta_bytes",
	} {
		if count := testutil.CollectAndCount(collector, name); count != 0 {
			t.Errorf("%s published %d series before the first scan, want 0", name, count)
		}
	}
	// Status must exist so "never scanned" is distinguishable from "the exporter is down".
	if count := testutil.CollectAndCount(collector, Namespace+"_last_scan_success"); count != 1 {
		t.Errorf("last_scan_success series = %d, want 1", count)
	}
}

func TestCollector_PublishesMeasurementAfterCompletedScan(t *testing.T) {
	store, collector, _ := newHarness(t, allOptions(), fsstat.CapAllocBytes)
	store.Apply(completeScan(100))

	expected := `
# HELP dir_exporter_size_bytes Logical size of regular files in the directory, in bytes. Absent until a scan completes without errors.
# TYPE dir_exporter_size_bytes gauge
dir_exporter_size_bytes{target_path="/data/logs/venue-a"} 100
`
	if err := testutil.CollectAndCompare(collector, strings.NewReader(expected), Namespace+"_size_bytes"); err != nil {
		t.Fatal(err)
	}
	if got := testutil.CollectAndCount(collector, Namespace+"_disk_usage_bytes"); got != 1 {
		t.Errorf("disk_usage_bytes series = %d, want 1", got)
	}
}

// TestCollector_DiskUsageWithheldWithoutCapability enforces the degradation rule at the point it
// is actually visible. Publishing logical size as disk usage would look like a real measurement
// while silently understating every sparse file.
func TestCollector_DiskUsageWithheldWithoutCapability(t *testing.T) {
	store, collector, _ := newHarness(t, allOptions(), fsstat.CapFSBytes) // no CapAllocBytes
	store.Apply(completeScan(100))

	if got := testutil.CollectAndCount(collector, Namespace+"_disk_usage_bytes"); got != 0 {
		t.Fatalf("disk_usage_bytes published %d series without CapAllocBytes, want 0", got)
	}
	if got := testutil.CollectAndCount(collector, Namespace+"_size_bytes"); got != 1 {
		t.Errorf("size_bytes should still publish; it needs no platform support (got %d)", got)
	}
	// The absence must be explainable from metrics alone, not only from a startup log line.
	expected := `
# HELP dir_exporter_platform_capability Whether a platform measurement capability is available (1 for yes, 0 for no). Metrics depending on an absent capability are not published at all.
# TYPE dir_exporter_platform_capability gauge
dir_exporter_platform_capability{capability="alloc_bytes"} 0
dir_exporter_platform_capability{capability="fs_bytes"} 1
dir_exporter_platform_capability{capability="fs_inodes"} 0
dir_exporter_platform_capability{capability="inode"} 0
dir_exporter_platform_capability{capability="mountpoint"} 0
`
	if err := testutil.CollectAndCompare(collector, strings.NewReader(expected), Namespace+"_platform_capability"); err != nil {
		t.Fatal(err)
	}
}

func TestCollector_GatedFamiliesAbsentWhenDisabled(t *testing.T) {
	store, collector, _ := newHarness(t, Options{}, fsstat.CapAllocBytes|fsstat.CapInode)
	store.Apply(completeScan(100))
	store.Apply(completeScan(150))

	for _, name := range []string{
		Namespace + "_files",
		Namespace + "_directories",
		Namespace + "_disk_usage_bytes",
		Namespace + "_size_delta_bytes",
		Namespace + "_bytes_added_total",
		Namespace + "_hardlinked_files",
		Namespace + "_scan_skipped_other_filesystem_total",
		Namespace + "_filesystem_size_bytes",
		Namespace + "_scan_duration_seconds",
	} {
		if count := testutil.CollectAndCount(collector, name); count != 0 {
			t.Errorf("%s published %d series while disabled, want 0", name, count)
		}
	}
	if count := testutil.CollectAndCount(collector, Namespace+"_size_bytes"); count != 1 {
		t.Errorf("size_bytes is ungated and should be present, got %d series", count)
	}
}

func TestCollector_RetainsPreviousValueWhileScanning(t *testing.T) {
	store, collector, _ := newHarness(t, allOptions(), fsstat.CapAllocBytes)
	store.Apply(completeScan(100))
	store.MarkScanStarted(target)

	// The headline behaviour: a long walk must not blank the dashboard.
	expected := `
# HELP dir_exporter_size_bytes Logical size of regular files in the directory, in bytes. Absent until a scan completes without errors.
# TYPE dir_exporter_size_bytes gauge
dir_exporter_size_bytes{target_path="/data/logs/venue-a"} 100
`
	if err := testutil.CollectAndCompare(collector, strings.NewReader(expected), Namespace+"_size_bytes"); err != nil {
		t.Fatal(err)
	}
	inProgress := `
# HELP dir_exporter_scan_in_progress Whether a scan of this target is running now (1 for yes, 0 for no).
# TYPE dir_exporter_scan_in_progress gauge
dir_exporter_scan_in_progress{target_path="/data/logs/venue-a"} 1
`
	if err := testutil.CollectAndCompare(collector, strings.NewReader(inProgress), Namespace+"_scan_in_progress"); err != nil {
		t.Fatal(err)
	}
}

func TestCollector_FailedScanKeepsPublishingTheLastGoodValue(t *testing.T) {
	store, collector, _ := newHarness(t, allOptions(), fsstat.CapAllocBytes)
	store.Apply(completeScan(100))
	store.Apply(state.Result{
		Target: target, Outcome: state.OutcomePartial, SizeBytes: 7,
		Errors: map[string]int64{scan.ClassPermission: 12},
	})

	expected := `
# HELP dir_exporter_size_bytes Logical size of regular files in the directory, in bytes. Absent until a scan completes without errors.
# TYPE dir_exporter_size_bytes gauge
dir_exporter_size_bytes{target_path="/data/logs/venue-a"} 100
`
	if err := testutil.CollectAndCompare(collector, strings.NewReader(expected), Namespace+"_size_bytes"); err != nil {
		t.Fatalf("a partial scan reached the published size: %v", err)
	}
	if got := gaugeValue(t, collector, Namespace+"_last_scan_errors", "class", scan.ClassPermission); got != 12 {
		t.Errorf("permission errors = %v, want 12", got)
	}
}

// gaugeValue gathers one series identified by a label pair, as Prometheus would see it.
func gaugeValue(t *testing.T, collector prometheus.Collector, name, label, value string) float64 {
	t.Helper()
	registry := prometheus.NewRegistry()
	registry.MustRegister(collector)
	families, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range families {
		if family.GetName() != name {
			continue
		}
		for _, metric := range family.GetMetric() {
			for _, pair := range metric.GetLabel() {
				if pair.GetName() == label && pair.GetValue() == value {
					return metric.GetGauge().GetValue()
				}
			}
		}
	}
	t.Fatalf("no series %s{%s=%q}", name, label, value)
	return 0
}

func TestCollector_ErrorAndOutcomeSeriesExistFromProcessStart(t *testing.T) {
	_, collector, _ := newHarness(t, allOptions(), 0)

	// An alert on a series that does not exist yet cannot fire, so every class and outcome must be
	// present at zero rather than appearing only after the first failure of that kind.
	if got, want := testutil.CollectAndCount(collector, Namespace+"_scan_errors_total"), len(scan.AllErrorClasses); got != want {
		t.Errorf("scan_errors_total series = %d, want %d", got, want)
	}
	if got, want := testutil.CollectAndCount(collector, Namespace+"_scans_total"), len(state.AllOutcomes); got != want {
		t.Errorf("scans_total series = %d, want %d", got, want)
	}
}

func TestCollector_FilesystemSeriesDedupedByLabelSet(t *testing.T) {
	targets := []string{"/data/logs/a", "/data/logs/b", "/data/logs/c"}
	shared := fsstat.FSInfo{
		TotalBytes: 1 << 40, FreeBytes: 1 << 30, AvailBytes: 1 << 30,
		Mountpoint: "/data", Device: "8:1", FSType: "xfs",
	}
	fake := fsstat.NewFake(fsstat.CapFSBytes)
	for _, path := range targets {
		fake.AddDir(path)
	}
	fake.SetFSInfo("/data", shared)

	store := state.New(targets, state.Options{Now: time.Now})
	cache := NewFilesystemCache(fake, time.Second, nil)
	cache.Refresh(context.Background(), targets)
	collector := New(store, stubStats{}, cache, fsstat.CapFSBytes, allOptions())

	// Capacity belongs to the filesystem, not the target. Emitting it per target would produce
	// duplicate conflicting series, which Prometheus rejects outright.
	if got := testutil.CollectAndCount(collector, Namespace+"_filesystem_size_bytes"); got != 1 {
		t.Errorf("filesystem_size_bytes series = %d, want 1 for three targets on one volume", got)
	}
	// The join series is per target, and is what makes the share-of-volume query expressible.
	if got := testutil.CollectAndCount(collector, Namespace+"_target_mountpoint_info"); got != 3 {
		t.Errorf("target_mountpoint_info series = %d, want 3", got)
	}
	// Inode totals have no meaning without the capability.
	if got := testutil.CollectAndCount(collector, Namespace+"_filesystem_files"); got != 0 {
		t.Errorf("filesystem_files published %d series without CapFSInodes, want 0", got)
	}
}

func TestFilesystemCache_TimeoutIsReportedAndNotFatal(t *testing.T) {
	fake := fsstat.NewFake(fsstat.CapFSBytes).AddDir("/data/logs")
	// A statfs against a dead NFS server never returns; the cache must give up rather than wedge
	// the scan cycle waiting for it.
	blocked := make(chan struct{})
	defer close(blocked)
	fake.SetFSInfo("/data/logs", fsstat.FSInfo{Mountpoint: "/data"})
	slow := &blockingFS{FakeFS: fake, block: blocked}

	cache := NewFilesystemCache(slow, 20*time.Millisecond, nil)
	done := make(chan struct{})
	go func() {
		cache.Refresh(context.Background(), []string{"/data/logs"})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Refresh never returned; a hung mount wedged the cycle")
	}

	store := state.New([]string{"/data/logs"}, state.Options{Now: time.Now})
	collector := New(store, stubStats{}, cache, fsstat.CapFSBytes, allOptions())
	if got := testutil.CollectAndCount(collector, Namespace+"_filesystem_size_bytes"); got != 0 {
		t.Errorf("capacity was published despite the timeout (%d series)", got)
	}
	// Stale free space feeds directly into hours-until-full, so its absence must be visible.
	if got := testutil.CollectAndCount(collector, Namespace+"_filesystem_info_unavailable"); got != 1 {
		t.Errorf("filesystem_info_unavailable series = %d, want 1", got)
	}
	if got := testutil.CollectAndCount(collector, Namespace+"_statfs_timeouts_total"); got != 1 {
		t.Errorf("statfs_timeouts_total series = %d, want 1", got)
	}
}

type blockingFS struct {
	*fsstat.FakeFS
	block chan struct{}
}

func (b *blockingFS) StatFS(string) (fsstat.FSInfo, error) {
	<-b.block
	return fsstat.FSInfo{}, nil
}

// TestFilesystemCache_ForgetsTargetsThatAreGone matters on a host with dated glob targets, where
// directories are created and retired continuously: a timeout count kept for every target that
// ever existed would grow for the life of the process.
func TestFilesystemCache_ForgetsTargetsThatAreGone(t *testing.T) {
	fake := fsstat.NewFake(fsstat.CapFSBytes)
	for _, path := range []string{"/logs/day-01", "/logs/day-02"} {
		fake.AddDir(path)
	}
	blocked := make(chan struct{})
	defer close(blocked)
	cache := NewFilesystemCache(&blockingFS{FakeFS: fake, block: blocked}, 10*time.Millisecond, nil)

	cache.Refresh(context.Background(), []string{"/logs/day-01", "/logs/day-02"})
	if got := countTimeouts(cache); got != 2 {
		t.Fatalf("tracked timeouts = %d, want 2", got)
	}

	// day-01 is retired; only day-02 remains configured.
	cache.Refresh(context.Background(), []string{"/logs/day-02"})
	if got := countTimeouts(cache); got != 1 {
		t.Errorf("tracked timeouts = %d after a target was retired, want 1", got)
	}
	if _, still := cache.timeouts["/logs/day-01"]; still {
		t.Error("a retired target's timeout count was retained")
	}
}

func countTimeouts(cache *FilesystemCache) int {
	cache.mu.RLock()
	defer cache.mu.RUnlock()
	return len(cache.timeouts)
}

func TestRecorder_FeedsBothRetentionAndHistogram(t *testing.T) {
	store, collector, _ := newHarness(t, allOptions(), fsstat.CapAllocBytes)
	recorder := collector.Recorder()

	recorder.MarkScanStarted(target)
	recorder.Apply(completeScan(100))
	if snap, _ := store.SnapshotFor(target); snap.SizeBytes != 100 {
		t.Fatalf("retention state not updated: %+v", snap)
	}
	if got := testutil.CollectAndCount(collector, Namespace+"_scan_duration_seconds"); got == 0 {
		t.Error("histogram was not observed")
	}

	// A cancelled scan was cut short by shutdown at an arbitrary point, so its duration says
	// nothing about how long this target takes and would drag the distribution down.
	before := histogramSampleCount(t, collector, string(state.OutcomeCancelled))
	recorder.Apply(state.Result{Target: target, Outcome: state.OutcomeCancelled, DurationSeconds: 0.01})
	if after := histogramSampleCount(t, collector, string(state.OutcomeCancelled)); after != before {
		t.Errorf("cancelled scan was observed into the histogram: %d -> %d", before, after)
	}
}

// TestHistogram_OnlyCreatesSeriesForOutcomesThatHappened keeps the histogram's cost proportional
// to what actually occurs.
//
// Pre-creating every outcome is worth it for counters, where a series that does not exist cannot
// be alerted on. For a duration histogram it means fifteen series describing the distribution of
// an event that has never happened, and with six outcomes that is ninety series on an exporter
// that otherwise emits about fifty.
func TestHistogram_OnlyCreatesSeriesForOutcomesThatHappened(t *testing.T) {
	store, collector, _ := newHarness(t, allOptions(), fsstat.CapAllocBytes)
	recorder := collector.Recorder()

	if got := testutil.CollectAndCount(collector, Namespace+"_scan_duration_seconds"); got != 0 {
		t.Fatalf("histogram published %d series before any scan, want 0", got)
	}

	recorder.Apply(completeScan(100))
	afterOne := testutil.CollectAndCount(collector, Namespace+"_scan_duration_seconds")
	if afterOne == 0 {
		t.Fatal("histogram published nothing after a completed scan")
	}

	// A second outcome adds its own series; the four that never occur never appear.
	recorder.Apply(state.Result{Target: target, Outcome: state.OutcomePartial, DurationSeconds: 3})
	afterTwo := testutil.CollectAndCount(collector, Namespace+"_scan_duration_seconds")
	if afterTwo != 2*afterOne {
		t.Errorf("series after two outcomes = %d, want %d", afterTwo, 2*afterOne)
	}
	if store == nil {
		t.Fatal("unreachable")
	}
}

func histogramSampleCount(t *testing.T, collector *Collector, result string) uint64 {
	t.Helper()
	registry := prometheus.NewRegistry()
	registry.MustRegister(collector)
	families, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range families {
		if family.GetName() != Namespace+"_scan_duration_seconds" {
			continue
		}
		for _, metric := range family.GetMetric() {
			for _, label := range metric.GetLabel() {
				if label.GetName() == "result" && label.GetValue() == result {
					return metric.GetHistogram().GetSampleCount()
				}
			}
		}
	}
	return 0
}

func TestRegisterSelf(t *testing.T) {
	registry := prometheus.NewRegistry()
	if err := RegisterSelf(registry, true); err != nil {
		t.Fatal(err)
	}
	families, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	present := map[string]bool{}
	for _, family := range families {
		present[family.GetName()] = true
	}
	// Without these the exporter is invisible to the monitoring system it feeds: a goroutine leak
	// or a wedged worker pool would show up nowhere.
	for _, name := range []string{"go_goroutines", "process_start_time_seconds", Namespace + "_build_info"} {
		if !present[name] {
			t.Errorf("%s was not registered", name)
		}
	}
}

func TestRegisterSelf_RuntimeCollectorsCanBeDisabled(t *testing.T) {
	registry := prometheus.NewRegistry()
	if err := RegisterSelf(registry, false); err != nil {
		t.Fatal(err)
	}
	families, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	present := map[string]bool{}
	for _, family := range families {
		present[family.GetName()] = true
		if strings.HasPrefix(family.GetName(), "go_") || strings.HasPrefix(family.GetName(), "process_") {
			t.Errorf("%s registered while runtime collectors are disabled", family.GetName())
		}
	}
	// build_info is a single series and the only way to correlate behaviour with a release, so it
	// is never gated.
	if !present[Namespace+"_build_info"] {
		t.Error("build_info should be registered regardless of the runtime-collector setting")
	}
}
