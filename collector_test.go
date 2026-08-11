package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
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
	targets, err := normalizeTargets([]string{base, "  " + base + "  "})
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 1 || targets[0] != base {
		t.Fatalf("unexpected targets: %#v", targets)
	}
	if _, err := normalizeTargets([]string{""}); err == nil {
		t.Fatal("empty target was accepted")
	}
	if _, err := normalizeTargets(nil); err == nil {
		t.Fatal("missing targets were accepted")
	}
}

func cachedStats(t *testing.T, collector *DirectoryCollector, target string) TargetStats {
	t.Helper()
	collector.statsMu.RLock()
	defer collector.statsMu.RUnlock()
	return collector.cache[target]
}
