// Package exporter implements cached directory-size collection and HTTP metrics serving.
package exporter

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

type TargetStats struct {
	SizeBytes         int64
	FilesTotal        int64
	DirsTotal         int64
	DurationSeconds   float64
	LastScanErrors    int64
	LastScrapeSuccess float64
	LastScanTimestamp float64
}

// targetState separates the last known-good measurement from the most recent scan attempt.
//
// Sizes are published only from a scan that completed without errors. A timeout, a permission
// error, or a directory that briefly disappeared therefore cannot overwrite a good size with a
// systematically low one, which on a disk-usage dashboard would read as freed space.
type targetState struct {
	good    TargetStats // last error-free scan; meaningful only when hasGood
	hasGood bool
	last    TargetStats // most recent attempt, whatever its outcome; meaningful only when hasLast
	hasLast bool
}

// DirectoryCollector exposes cached results. Metrics collection never walks the filesystem.
type DirectoryCollector struct {
	targets      []string
	enableCounts bool
	statsMu      sync.RWMutex
	cache        map[string]targetState
	scanMu       sync.Mutex
	scanning     atomic.Bool

	scanErrorsTotal atomic.Uint64

	sizeDesc      *prometheus.Desc
	filesDesc     *prometheus.Desc
	dirsDesc      *prometheus.Desc
	durationDesc  *prometheus.Desc
	errorsDesc    *prometheus.Desc
	errorsTotal   *prometheus.Desc
	successDesc   *prometheus.Desc
	timestampDesc *prometheus.Desc
	inProgress    *prometheus.Desc
}

func NewDirectoryCollector(targets []string, enableCounts bool) *DirectoryCollector {
	labels := []string{"target_path"}
	c := &DirectoryCollector{
		targets: targets, enableCounts: enableCounts, cache: make(map[string]targetState, len(targets)),
		sizeDesc:      prometheus.NewDesc("dir_exporter_size_bytes", "Logical size of regular files in the directory, in bytes.", labels, nil),
		filesDesc:     prometheus.NewDesc("dir_exporter_files", "Number of regular files in the directory.", labels, nil),
		dirsDesc:      prometheus.NewDesc("dir_exporter_directories", "Number of subdirectories in the directory.", labels, nil),
		durationDesc:  prometheus.NewDesc("dir_exporter_scan_duration_seconds", "Duration of the most recent directory scan.", labels, nil),
		errorsDesc:    prometheus.NewDesc("dir_exporter_scan_errors", "Number of errors, including cancellation or timeout, in the most recent directory scan.", labels, nil),
		errorsTotal:   prometheus.NewDesc("dir_exporter_scan_errors_total", "Total scan errors encountered since process start.", nil, nil),
		successDesc:   prometheus.NewDesc("dir_exporter_last_scan_success", "Whether the most recent scan completed without filesystem errors (1 for success, 0 for failure).", labels, nil),
		timestampDesc: prometheus.NewDesc("dir_exporter_last_scan_timestamp_seconds", "Unix timestamp when the most recent directory scan completed.", labels, nil),
		inProgress:    prometheus.NewDesc("dir_exporter_scan_in_progress", "Whether a background directory scan is in progress (1 for yes, 0 for no).", nil, nil),
	}
	// The cache is deliberately left empty. Seeding it with zero-valued entries would publish a
	// genuine dir_exporter_size_bytes=0 from process start until the first scan completes, which
	// on a multi-TB tree can be many minutes and makes every restart look like data loss.
	return c
}

func (c *DirectoryCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.sizeDesc
	ch <- c.durationDesc
	ch <- c.errorsDesc
	ch <- c.errorsTotal
	ch <- c.successDesc
	ch <- c.timestampDesc
	ch <- c.inProgress
	if c.enableCounts {
		ch <- c.filesDesc
		ch <- c.dirsDesc
	}
}
func (c *DirectoryCollector) Collect(ch chan<- prometheus.Metric) {
	c.statsMu.RLock()
	snapshot := make(map[string]targetState, len(c.cache))
	for target, state := range c.cache {
		snapshot[target] = state
	}
	c.statsMu.RUnlock()
	// Iterate c.targets rather than the snapshot so that a target with no cache entry yet still
	// reports last_scan_success=0, distinguishing "not scanned yet" from "exporter is down".
	for _, target := range c.targets {
		state := snapshot[target]
		ch <- prometheus.MustNewConstMetric(c.successDesc, prometheus.GaugeValue, state.last.LastScrapeSuccess, target)
		if state.hasLast {
			ch <- prometheus.MustNewConstMetric(c.durationDesc, prometheus.GaugeValue, state.last.DurationSeconds, target)
			ch <- prometheus.MustNewConstMetric(c.errorsDesc, prometheus.GaugeValue, float64(state.last.LastScanErrors), target)
			ch <- prometheus.MustNewConstMetric(c.timestampDesc, prometheus.GaugeValue, state.last.LastScanTimestamp, target)
		}
		// Absent beats zero: a missing series is unambiguous in PromQL, a zero is a lie that
		// looks like data.
		if !state.hasGood {
			continue
		}
		ch <- prometheus.MustNewConstMetric(c.sizeDesc, prometheus.GaugeValue, float64(state.good.SizeBytes), target)
		if c.enableCounts {
			ch <- prometheus.MustNewConstMetric(c.filesDesc, prometheus.GaugeValue, float64(state.good.FilesTotal), target)
			ch <- prometheus.MustNewConstMetric(c.dirsDesc, prometheus.GaugeValue, float64(state.good.DirsTotal), target)
		}
	}
	ch <- prometheus.MustNewConstMetric(c.errorsTotal, prometheus.CounterValue, float64(c.scanErrorsTotal.Load()))
	value := 0.0
	if c.scanning.Load() {
		value = 1
	}
	ch <- prometheus.MustNewConstMetric(c.inProgress, prometheus.GaugeValue, value)
}

// Start runs scans asynchronously, retaining the previous snapshot during each scan.
func (c *DirectoryCollector) Start(ctx context.Context, interval, timeout time.Duration, logger *slog.Logger) {
	go func() {
		for {
			c.ScanAll(ctx, timeout, logger)
			timer := time.NewTimer(interval)
			select {
			case <-ctx.Done():
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				return
			case <-timer.C:
			}
		}
	}()
}

// ScanAll is single-flight and serializes targets to cap filesystem metadata I/O.
func (c *DirectoryCollector) ScanAll(ctx context.Context, timeout time.Duration, logger *slog.Logger) bool {
	if !c.scanMu.TryLock() {
		return false
	}
	defer c.scanMu.Unlock()
	c.scanning.Store(true)
	defer c.scanning.Store(false)
	for _, target := range c.targets {
		if ctx.Err() != nil {
			return true
		}
		scanCtx, cancel := ctx, func() {}
		if timeout > 0 {
			scanCtx, cancel = context.WithTimeout(ctx, timeout)
		}
		stats := c.scanTarget(scanCtx, target)
		cancel()
		c.scanErrorsTotal.Add(uint64(stats.LastScanErrors))
		c.statsMu.Lock()
		state := c.cache[target]
		state.last, state.hasLast = stats, true
		if stats.LastScrapeSuccess == 1 {
			state.good, state.hasGood = stats, true
		}
		c.cache[target] = state
		c.statsMu.Unlock()
		if stats.LastScrapeSuccess == 0 && logger != nil {
			logger.Warn("Directory scan completed with errors", "target", target, "errors", stats.LastScanErrors, "duration_seconds", stats.DurationSeconds)
		}
	}
	return true
}
func (c *DirectoryCollector) scanTarget(ctx context.Context, target string) TargetStats {
	started := time.Now()
	stats := TargetStats{}
	// Resolve the root before walking. filepath.WalkDir lstats its root, so a symlinked target
	// (/var/log/app -> /data/logs/app, common when logs live on a dedicated volume) is seen as a
	// non-directory: the callback fires once, the symlink branch below returns nil, and the walk
	// ends having counted nothing. That failed *green* — zero bytes with last_scan_success=1.
	root, err := filepath.EvalSymlinks(target)
	if err != nil {
		return failedScan(&stats, started)
	}
	if info, err := os.Stat(root); err != nil || !info.IsDir() {
		return failedScan(&stats, started)
	}
	walkErr := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			stats.LastScanErrors++
			if entry != nil && entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		if entry.IsDir() {
			if c.enableCounts && path != root {
				stats.DirsTotal++
			}
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			stats.LastScanErrors++
			return nil
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		stats.SizeBytes += info.Size()
		if c.enableCounts {
			stats.FilesTotal++
		}
		return nil
	})
	if walkErr != nil {
		stats.LastScanErrors++
	}
	stats.DurationSeconds = time.Since(started).Seconds()
	stats.LastScanTimestamp = float64(time.Now().UnixNano()) / float64(time.Second)
	if stats.LastScanErrors == 0 {
		stats.LastScrapeSuccess = 1
	}
	return stats
}

// failedScan finalizes a scan that could not start, so an unresolvable or non-directory root is
// reported as an error rather than as a successful measurement of zero bytes.
func failedScan(stats *TargetStats, started time.Time) TargetStats {
	stats.LastScanErrors++
	stats.LastScrapeSuccess = 0
	stats.DurationSeconds = time.Since(started).Seconds()
	stats.LastScanTimestamp = float64(time.Now().UnixNano()) / float64(time.Second)
	return *stats
}

func NewHTTPServer(registry *prometheus.Registry) *http.Server {
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(registry, promhttp.HandlerOpts{}))
	return &http.Server{
		Handler: mux, ReadHeaderTimeout: 5 * time.Second, WriteTimeout: 30 * time.Second,
		IdleTimeout: 60 * time.Second, MaxHeaderBytes: 8 << 10,
	}
}

func SplitLegacyTargets(raw string) []string { return strings.Split(raw, ",") }
func NormalizeTargets(raw []string) ([]string, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("at least one --path.target is required")
	}
	seen := make(map[string]struct{}, len(raw))
	targets := make([]string, 0, len(raw))
	for _, value := range raw {
		value = strings.TrimSpace(value)
		if value == "" {
			return nil, fmt.Errorf("target paths cannot be empty")
		}
		path, err := filepath.Abs(value)
		if err != nil {
			return nil, fmt.Errorf("resolve target %q: %w", value, err)
		}
		path = filepath.Clean(path)
		if _, ok := seen[path]; ok {
			continue
		}
		seen[path] = struct{}{}
		targets = append(targets, path)
	}
	return targets, nil
}
