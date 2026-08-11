package main

import (
	"flag"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/prometheus/exporter-toolkit/web"
	"github.com/prometheus/common/promslog"
	"log/slog"
)

// TargetStats holds the in-memory aggregated metrics for a single directory.
type TargetStats struct {
	SizeBytes         int64
	FilesTotal        int64
	DirsTotal         int64
	ScrapeDurationSec float64
	ScrapeErrorsTotal int64
	LastScrapeSuccess float64
}

// DirectoryCollector implements the prometheus.Collector interface.
type DirectoryCollector struct {
	targets      []string
	enableCounts bool

	statsLock sync.RWMutex
	cache     map[string]TargetStats

	// Metric Descriptors
	sizeDesc     *prometheus.Desc
	filesDesc    *prometheus.Desc
	dirsDesc     *prometheus.Desc
	durationDesc *prometheus.Desc
	errorsDesc   *prometheus.Desc
	successDesc  *prometheus.Desc
}

func NewDirectoryCollector(targets []string, enableCounts bool) *DirectoryCollector {
	labels := []string{"target_path"}
	return &DirectoryCollector{
		targets:      targets,
		enableCounts: enableCounts,
		cache:        make(map[string]TargetStats),

		sizeDesc: prometheus.NewDesc(
			"dir_exporter_size_bytes",
			"Total size of the directory in bytes.",
			labels, nil,
		),
		filesDesc: prometheus.NewDesc(
			"dir_exporter_files_total",
			"Total number of regular files in the target directory.",
			labels, nil,
		),
		dirsDesc: prometheus.NewDesc(
			"dir_exporter_directories_total",
			"Total number of subdirectories inside the target directory.",
			labels, nil,
		),
		durationDesc: prometheus.NewDesc(
			"dir_exporter_scrape_duration_seconds",
			"Time taken to scan the target directory in seconds.",
			labels, nil,
		),
		errorsDesc: prometheus.NewDesc(
			"dir_exporter_scrape_errors_total",
			"Total number of file/directory read errors encountered during scan.",
			labels, nil,
		),
		successDesc: prometheus.NewDesc(
			"dir_exporter_last_scrape_success",
			"1 if the last background scan was successful, 0 otherwise.",
			labels, nil,
		),
	}
}

func (c *DirectoryCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.sizeDesc
	ch <- c.durationDesc
	ch <- c.errorsDesc
	ch <- c.successDesc

	if c.enableCounts {
		ch <- c.filesDesc
		ch <- c.dirsDesc
	}
}

func (c *DirectoryCollector) Collect(ch chan<- prometheus.Metric) {
	c.statsLock.RLock()
	defer c.statsLock.RUnlock()

	for target, stats := range c.cache {
		ch <- prometheus.MustNewConstMetric(c.sizeDesc, prometheus.GaugeValue, float64(stats.SizeBytes), target)
		ch <- prometheus.MustNewConstMetric(c.durationDesc, prometheus.GaugeValue, stats.ScrapeDurationSec, target)
		ch <- prometheus.MustNewConstMetric(c.errorsDesc, prometheus.CounterValue, float64(stats.ScrapeErrorsTotal), target)
		ch <- prometheus.MustNewConstMetric(c.successDesc, prometheus.GaugeValue, stats.LastScrapeSuccess, target)

		if c.enableCounts {
			ch <- prometheus.MustNewConstMetric(c.filesDesc, prometheus.GaugeValue, float64(stats.FilesTotal), target)
			ch <- prometheus.MustNewConstMetric(c.dirsDesc, prometheus.GaugeValue, float64(stats.DirsTotal), target)
		}
	}
}

func (c *DirectoryCollector) ScanAll(logger *slog.Logger) {
	for _, target := range c.targets {
		stats := c.scanTarget(target, logger)
		c.statsLock.Lock()
		c.cache[target] = stats
		c.statsLock.Unlock()
	}
}

func (c *DirectoryCollector) scanTarget(targetDir string, logger *slog.Logger) TargetStats {
	start := time.Now()
	stats := TargetStats{LastScrapeSuccess: 1}

	err := filepath.WalkDir(targetDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			stats.ScrapeErrorsTotal++
			logger.Debug("Error accessing path", "path", path, "err", err)
			return nil // Skip unreadable paths, keep scanning
		}

		// Skip symbolic links completely per requirements
		if d.Type()&os.ModeSymlink != 0 {
			return nil
		}

		if d.IsDir() {
			if path != targetDir {
				stats.DirsTotal++
			}
			return nil
		}

		// Process regular files
		info, err := d.Info()
		if err != nil {
			stats.ScrapeErrorsTotal++
			return nil
		}

		stats.SizeBytes += info.Size()
		stats.FilesTotal++

		return nil
	})

	stats.ScrapeDurationSec = time.Since(start).Seconds()

	if err != nil {
		stats.LastScrapeSuccess = 0
		logger.Error("Failed to walk directory completely", "target", targetDir, "err", err)
	}

	return stats
}

func main() {
	var (
		rawTargets    = flag.String("path.targets", "", "Comma-separated list of target directory paths to monitor.")
		scanInterval  = flag.Duration("scan.interval", 5*time.Minute, "Interval between background directory scans.")
		listenAddress = flag.String("web.listen-address", ":9115", "Address on which to expose metrics and web interface.")
		webConfigFile = flag.String("web.config.file", "", "Path to configuration file that can enable TLS or authentication.")
		
		enableCounts  = flag.Bool("collector.file-counts", true, "Enable file and subdirectory count metrics.")
	)

	flag.Parse()

	logger := promslog.New(&promslog.Config{})

	if *rawTargets == "" {
		logger.Error("No target directories specified. Use --path.targets=/path1,/path2")
		os.Exit(1)
	}

	targets := strings.Split(*rawTargets, ",")
	for i := range targets {
		targets[i] = strings.TrimSpace(targets[i])
	}

	collector := NewDirectoryCollector(targets, *enableCounts)
	prometheus.MustRegister(collector)

	// Perform an initial scan on startup
	logger.Info("Starting initial directory scan", "targets", len(targets))
	collector.ScanAll(logger)

	// Start background ticker loop
	go func() {
		ticker := time.NewTicker(*scanInterval)
		defer ticker.Stop()
		for range ticker.C {
			logger.Info("Executing scheduled directory scan")
			collector.ScanAll(logger)
		}
	}()

	http.Handle("/metrics", promhttp.Handler())
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`<html>
			<head><title>Directory Size Exporter</title></head>
			<body>
			<h1>Directory Size Exporter</h1>
			<p><a href="/metrics">Metrics</a></p>
			</body>
			</html>`))
	})

	server := &http.Server{}
	
	// Set up exporter-toolkit flags
	toolkitFlags := &web.FlagConfig{
		WebListenAddresses: &[]string{*listenAddress},
		WebSystemdSocket:   func(b bool) *bool { return &b }(false),
		WebConfigFile:      webConfigFile,
	}

	logger.Info("Starting HTTP server", "address", *listenAddress)
	
	// Use the exporter-toolkit to start the server
	if err := web.ListenAndServe(server, toolkitFlags, logger); err != nil {
		logger.Error("HTTP server failed", "err", err)
		os.Exit(1)
	}
}