package exporter

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestScanCountsOnlyRegularFiles(t *testing.T) {
	target := t.TempDir()
	if err := os.WriteFile(filepath.Join(target, "first.log"), []byte("abc"), 0o600); err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(target, "nested")
	if err := os.Mkdir(nested, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nested, "second.log"), []byte("12345"), 0o600); err != nil {
		t.Fatal(err)
	}
	collector := NewDirectoryCollector([]string{target}, true)
	if !collector.ScanAll(context.Background(), 0, nil) {
		t.Fatal("scan was unexpectedly skipped")
	}
	stats := cachedStats(t, collector, target)
	if stats.SizeBytes != 8 || stats.FilesTotal != 2 || stats.DirsTotal != 1 {
		t.Fatalf("unexpected stats: %+v", stats)
	}
	if stats.LastScrapeSuccess != 1 || stats.LastScanErrors != 0 || stats.LastScanTimestamp == 0 {
		t.Fatalf("unexpected scan status: %+v", stats)
	}
	registry := prometheus.NewRegistry()
	registry.MustRegister(collector)
	if _, err := registry.Gather(); err != nil {
		t.Fatalf("gather metrics: %v", err)
	}
}

func TestMissingTargetIsReportedAsFailedScan(t *testing.T) {
	target := filepath.Join(t.TempDir(), "missing")
	collector := NewDirectoryCollector([]string{target}, false)
	collector.ScanAll(context.Background(), 0, nil)
	stats := cachedStats(t, collector, target)
	if stats.LastScrapeSuccess != 0 || stats.LastScanErrors == 0 || stats.LastScanTimestamp == 0 {
		t.Fatalf("missing target was not reported as failed: %+v", stats)
	}
	// The failed scan must not publish a size at all. Publishing its zero would read as a sudden
	// drop to empty on a disk-usage dashboard.
	if _, ok := goodStats(t, collector, target); ok {
		t.Fatal("failed scan published a size")
	}
}

func TestFailedScanRetainsLastGoodSize(t *testing.T) {
	target := filepath.Join(t.TempDir(), "logs")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "trades.log"), []byte("0123456789"), 0o600); err != nil {
		t.Fatal(err)
	}
	collector := NewDirectoryCollector([]string{target}, false)
	collector.ScanAll(context.Background(), 0, nil)
	good, ok := goodStats(t, collector, target)
	if !ok || good.SizeBytes != 10 {
		t.Fatalf("first scan did not publish the expected size: %+v ok=%v", good, ok)
	}

	// A vanished directory stands in for the whole family of failures — transient unmount, mid-
	// rotation swap, permission storm — that produce a systematically low result. None of them may
	// replace the last good size, because doing so reads as freed disk space.
	if err := os.RemoveAll(target); err != nil {
		t.Fatal(err)
	}
	collector.ScanAll(context.Background(), 0, nil)

	retained, ok := goodStats(t, collector, target)
	if !ok || retained.SizeBytes != 10 {
		t.Fatalf("failed scan overwrote the last good size: %+v ok=%v", retained, ok)
	}
	if last := cachedStats(t, collector, target); last.LastScrapeSuccess != 0 || last.LastScanErrors == 0 {
		t.Fatalf("failed scan was not reported as failed: %+v", last)
	}

	// The retained size must still be served to Prometheus, not just held internally.
	if got, ok := publishedSize(t, collector, target); !ok || got != 10 {
		t.Fatalf("retained size was not published: got %v ok=%v, want 10", got, ok)
	}
}

func TestNoSizeSeriesBeforeFirstScan(t *testing.T) {
	collector := NewDirectoryCollector([]string{t.TempDir()}, true)
	registry := prometheus.NewRegistry()
	registry.MustRegister(collector)

	// Before any scan, a zero size would be indistinguishable from a genuinely empty directory and
	// would fire every "space was freed" alert on process restart. The series must be absent.
	for _, name := range []string{"dir_exporter_size_bytes", "dir_exporter_files", "dir_exporter_directories"} {
		if count := testutil.CollectAndCount(collector, name); count != 0 {
			t.Errorf("%s was published before the first scan (%d series)", name, count)
		}
	}
	if count := testutil.CollectAndCount(collector, "dir_exporter_last_scan_success"); count != 1 {
		t.Errorf("last_scan_success should be present and 0 before the first scan, got %d series", count)
	}
}

func TestSymlinkedTargetRootIsResolved(t *testing.T) {
	base := t.TempDir()
	real := filepath.Join(base, "real")
	if err := os.Mkdir(real, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(real, "trades.log"), []byte("0123456789"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("cannot create symlinks on this host: %v", err)
	}

	// A symlinked root previously reported zero bytes with last_scan_success=1 — it failed green,
	// which is worse than failing loudly.
	collector := NewDirectoryCollector([]string{link}, false)
	collector.ScanAll(context.Background(), 0, nil)
	good, ok := goodStats(t, collector, link)
	if !ok || good.SizeBytes != 10 {
		t.Fatalf("symlinked root was not resolved: %+v ok=%v", good, ok)
	}
}

func TestNonDirectoryTargetIsReportedAsFailedScan(t *testing.T) {
	target := filepath.Join(t.TempDir(), "trades.log")
	if err := os.WriteFile(target, []byte("0123456789"), 0o600); err != nil {
		t.Fatal(err)
	}
	collector := NewDirectoryCollector([]string{target}, false)
	collector.ScanAll(context.Background(), 0, nil)
	if stats := cachedStats(t, collector, target); stats.LastScrapeSuccess != 0 || stats.LastScanErrors == 0 {
		t.Fatalf("non-directory target was not reported as failed: %+v", stats)
	}
}

func TestScanAllSkipsConcurrentRun(t *testing.T) {
	collector := NewDirectoryCollector([]string{t.TempDir()}, false)
	collector.scanMu.Lock()
	defer collector.scanMu.Unlock()
	if collector.ScanAll(context.Background(), 0, nil) {
		t.Fatal("concurrent scan was not skipped")
	}
}
func TestCancelledScanIsReportedAsFailed(t *testing.T) {
	target := t.TempDir()
	collector := NewDirectoryCollector([]string{target}, false)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	stats := collector.scanTarget(ctx, target)
	if stats.LastScrapeSuccess != 0 || stats.LastScanErrors == 0 {
		t.Fatalf("cancelled scan was not reported as failed: %+v", stats)
	}
}
func TestNormalizeTargets(t *testing.T) {
	base := t.TempDir()
	targets, err := NormalizeTargets([]string{base, "  " + base + "  "})
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 1 || targets[0] != base {
		t.Fatalf("unexpected targets: %#v", targets)
	}
	if _, err := NormalizeTargets([]string{""}); err == nil {
		t.Fatal("empty target was accepted")
	}
	if _, err := NormalizeTargets(nil); err == nil {
		t.Fatal("missing targets were accepted")
	}
}

// cachedStats returns the most recent scan attempt for target, whatever its outcome.
func cachedStats(t *testing.T, collector *DirectoryCollector, target string) TargetStats {
	t.Helper()
	collector.statsMu.RLock()
	defer collector.statsMu.RUnlock()
	return collector.cache[target].last
}

// publishedSize gathers dir_exporter_size_bytes for target as Prometheus would see it, and
// reports whether the series was emitted at all.
func publishedSize(t *testing.T, collector *DirectoryCollector, target string) (float64, bool) {
	t.Helper()
	registry := prometheus.NewRegistry()
	registry.MustRegister(collector)
	families, err := registry.Gather()
	if err != nil {
		t.Fatalf("gather metrics: %v", err)
	}
	for _, family := range families {
		if family.GetName() != "dir_exporter_size_bytes" {
			continue
		}
		for _, metric := range family.GetMetric() {
			for _, label := range metric.GetLabel() {
				if label.GetName() == "target_path" && label.GetValue() == target {
					return metric.GetGauge().GetValue(), true
				}
			}
		}
	}
	return 0, false
}

// goodStats returns the last error-free scan for target, and whether one exists at all.
func goodStats(t *testing.T, collector *DirectoryCollector, target string) (TargetStats, bool) {
	t.Helper()
	collector.statsMu.RLock()
	defer collector.statsMu.RUnlock()
	state := collector.cache[target]
	return state.good, state.hasGood
}
