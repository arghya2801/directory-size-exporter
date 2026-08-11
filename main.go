package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/prometheus/common/promslog"
	"github.com/prometheus/exporter-toolkit/web"
)

const (
	defaultScanInterval = 5 * time.Minute
	shutdownTimeout     = 10 * time.Second
)

func newHTTPServer(registry *prometheus.Registry) *http.Server {
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(registry, promhttp.HandlerOpts{}))
	return &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    8 << 10,
	}
}

func main() {
	var targets targetList
	var legacyTargets string
	var scanInterval time.Duration
	var scanTimeout time.Duration
	var enableCounts bool
	var listenAddress string

	flag.Var(&targets, "path.target", "Directory to monitor. Repeat this flag for multiple directories.")
	flag.StringVar(&legacyTargets, "path.targets", "", "Deprecated comma-separated target directories; use --path.target repeatedly.")
	flag.DurationVar(&scanInterval, "scan.interval", defaultScanInterval, "Interval between directory scans.")
	flag.DurationVar(&scanTimeout, "scan.timeout", 0, "Maximum duration of one scan; 0 disables the timeout.")
	flag.BoolVar(&enableCounts, "collector.file-counts", false, "Expose regular-file and subdirectory count metrics.")
	flag.StringVar(&listenAddress, "web.listen-address", ":9115", "Address on which to expose Prometheus metrics.")
	flag.Parse()

	logger := promslog.New(&promslog.Config{})
	if legacyTargets != "" {
		targets = append(targets, splitLegacyTargets(legacyTargets)...)
	}
	normalized, err := normalizeTargets(targets)
	if err != nil {
		logger.Error("Invalid target configuration", "err", err)
		os.Exit(2)
	}
	if scanInterval <= 0 {
		logger.Error("scan.interval must be greater than zero")
		os.Exit(2)
	}
	if scanTimeout < 0 {
		logger.Error("scan.timeout cannot be negative")
		os.Exit(2)
	}

	collector := NewDirectoryCollector(normalized, enableCounts)
	registry := prometheus.NewRegistry()
	registry.MustRegister(collector)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	collector.Start(ctx, scanInterval, scanTimeout, logger)

	server := newHTTPServer(registry)
	addresses := []string{listenAddress}
	systemdSocket := false
	noWebConfig := "" // A blank toolkit configuration deliberately disables TLS and authentication.
	toolkitFlags := &web.FlagConfig{
		WebListenAddresses: &addresses,
		WebSystemdSocket:   &systemdSocket,
		WebConfigFile:      &noWebConfig,
	}

	errCh := make(chan error, 1)
	go func() { errCh <- web.ListenAndServe(server, toolkitFlags, logger) }()
	logger.Info("Directory size exporter started", "address", listenAddress, "targets", len(normalized))

	select {
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("HTTP server failed", "err", err)
		}
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			logger.Error("HTTP server shutdown failed", "err", err)
		}
		if err := <-errCh; err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("HTTP server stopped with an error", "err", err)
		}
	}
}

// targetList supports repeatable --path.target flags, including paths containing commas.
type targetList []string

func (t *targetList) String() string { return fmt.Sprint([]string(*t)) }
func (t *targetList) Set(value string) error {
	*t = append(*t, value)
	return nil
}
