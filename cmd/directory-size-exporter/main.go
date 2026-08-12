// Command directory-size-exporter publishes per-directory size and capacity metrics.
//
// This file is wiring only. Every decision of consequence lives in a package that can be tested
// without a process, a socket or a clock.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/alecthomas/kingpin/v2"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/promslog"
	promslogflag "github.com/prometheus/common/promslog/flag"
	"github.com/prometheus/common/version"
	"github.com/prometheus/exporter-toolkit/web"
	webflag "github.com/prometheus/exporter-toolkit/web/kingpinflag"

	"github.com/local/directory-size-exporter/internal/collector"
	"github.com/local/directory-size-exporter/internal/config"
	"github.com/local/directory-size-exporter/internal/fsstat"
	"github.com/local/directory-size-exporter/internal/scan"
	"github.com/local/directory-size-exporter/internal/schedprio"
	"github.com/local/directory-size-exporter/internal/server"
	"github.com/local/directory-size-exporter/internal/state"
	"github.com/local/directory-size-exporter/internal/targets"
)

const shutdownTimeout = 10 * time.Second

func main() {
	if err := run(); err != nil {
		// Written directly because a configuration error can occur before the logger exists.
		fmt.Fprintln(os.Stderr, "error: "+err.Error())
		os.Exit(2)
	}
}

func run() error {
	app := kingpin.New("directory-size-exporter",
		"Publishes the size of configured directories as Prometheus metrics.")
	app.HelpFlag.Short('h')
	app.Version(version.Print("directory-size-exporter"))

	registry := config.NewRegistry()
	registry.RegisterFlags(app)
	webFlags := webflag.AddFlags(app, ":9115")
	logConfig := &promslog.Config{}
	promslogflag.AddFlags(app, logConfig)

	if _, err := app.Parse(registry.NormalizeArgs(os.Args[1:])); err != nil {
		return err
	}
	logger := promslog.New(logConfig)

	if err := registry.Resolve(registry.Config().ConfigFile); err != nil {
		return err
	}
	resolved, err := registry.Validate(fsstat.Caps(), logger)
	if err != nil {
		return err
	}

	logger.Info("Starting directory-size-exporter", "version", version.Info(), "build_context", version.BuildContext())
	registry.LogAudit(logger, resolved)

	exporter, err := newApp(resolved, logger)
	if err != nil {
		return err
	}
	return exporter.serve(webFlags, logger)
}

// application holds the wired-together components.
type application struct {
	cfg      *config.Resolved
	logger   *slog.Logger
	resolver *targets.Resolver
	store    *state.Store
	engine   *scan.Engine
	fsCache  *collector.FilesystemCache
	metrics  *collector.Collector
	registry *prometheus.Registry

	// firstCycleComplete reports whether a full scan cycle has finished, which is what readiness
	// waits for.
	//
	// Deliberately a property of the cycle rather than of every target's success. Requiring every
	// target to have published would leave a single permanently-unreadable directory holding the
	// whole exporter at 503 forever, even while nineteen other targets report perfectly — and
	// under an orchestrator that means the process never enters service at all.
	//
	// It is also deliberately one-way. Re-evaluating per request would drop the exporter out of
	// service every time a glob picked up a new directory, which is a routine event on a host with
	// dated log paths. A newly added target that has not been scanned yet is already visible: its
	// size series is absent and its scan_age is unset.
	firstCycleComplete atomic.Bool
}

func newApp(cfg *config.Resolved, logger *slog.Logger) (*application, error) {
	filesystem := fsstat.System()

	resolver := targets.New(cfg.TargetPatterns, targets.Options{
		Max:            cfg.TargetsMax,
		RetainVanished: cfg.TargetsRetainVanished,
	})
	initial, err := resolver.Resolve()
	if err != nil {
		return nil, err
	}
	logResolution(logger, initial)
	if len(initial.Targets) == 0 {
		return nil, fmt.Errorf("no target directories matched %v", cfg.TargetPatterns)
	}

	store := state.New(initial.Paths(), state.Options{
		PublishPartial: cfg.ScanPublishPartial,
		StaleAfter:     cfg.ScanStaleAfter,
	})

	workerInit, err := workerInitFunc(cfg, logger)
	if err != nil {
		return nil, err
	}
	engine := scan.NewEngine(filesystem, scan.Config{
		Concurrency:        cfg.ScanConcurrency,
		BatchSize:          cfg.ScanBatchSize,
		ShareThreshold:     cfg.ScanShareThreshold,
		QueueLimit:         cfg.ScanQueueLimit,
		RateLimit:          cfg.ScanRateLimit,
		RateLimitBurst:     cfg.ScanRateLimitBurst,
		ScanTimeout:        cfg.ScanTimeout,
		HardTimeout:        cfg.ScanHardTimeout,
		WorkerDrainTimeout: cfg.ScanWorkerDrainTimeout,
		OneFilesystem:      cfg.OneFilesystem,
		DedupHardlinks:     cfg.DedupHardlinks,
		MaxTrackedInodes:   cfg.ScanMaxTrackedInodes,
		HeartbeatAfter:     cfg.LogScanHeartbeat,
		HeartbeatEvery:     cfg.LogHeartbeatInterval,
		WorkerInit:         workerInit,
	}, logger)

	fsCache := collector.NewFilesystemCache(filesystem, cfg.ScanStatfsTimeout, logger)
	metrics := collector.New(store, engine, fsCache, fsstat.Caps(), collector.Options{
		FileCounts:     cfg.CollectorFileCounts,
		DiskUsage:      cfg.DiskUsage,
		Growth:         cfg.CollectorGrowth,
		Filesystem:     cfg.Filesystem,
		DedupHardlinks: cfg.DedupHardlinks,
		OneFilesystem:  cfg.OneFilesystem,
		ScanHistogram:  cfg.CollectorScanHistogram,
	})

	promRegistry := prometheus.NewRegistry()
	if err := promRegistry.Register(metrics); err != nil {
		return nil, fmt.Errorf("register collector: %w", err)
	}
	if err := collector.RegisterSelf(promRegistry, cfg.CollectorSelf); err != nil {
		return nil, fmt.Errorf("register self metrics: %w", err)
	}

	return &application{
		cfg: cfg, logger: logger, resolver: resolver, store: store,
		engine: engine, fsCache: fsCache, metrics: metrics, registry: promRegistry,
	}, nil
}

// workerInitFunc builds the per-worker hook that lowers scheduling priority.
//
// It is resolved once at startup so an unsupported platform produces a single audit line rather
// than a warning per worker per cycle.
func workerInitFunc(cfg *config.Resolved, logger *slog.Logger) (func() error, error) {
	class, err := schedprio.ParseIOClass(cfg.ScanIOPriority)
	if err != nil {
		return nil, err
	}
	if !schedprio.Supported() {
		logger.Warn("Scan worker scheduling priority is unavailable on this platform; scans will compete normally with other work",
			"goos", runtime.GOOS, "requested_nice", cfg.ScanNice, "requested_io_priority", class)
		return nil, nil
	}
	logger.Info("Scan workers will run at lowered scheduling priority",
		"nice", cfg.ScanNice, "io_priority", class.String())

	priority := schedprio.Priority{Nice: cfg.ScanNice, IOClass: class}
	return func() error {
		// Pinned for the goroutine's lifetime and deliberately never unlocked: priority belongs to
		// the OS thread, so allowing Go to migrate this goroutine would leave a deprioritised
		// thread behind serving unrelated work.
		runtime.LockOSThread()
		return schedprio.ApplyToCurrentThread(priority)
	}, nil
}

func (a *application) serve(webFlags *web.FlagConfig, logger *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	httpServer, control := server.New(a.registry, server.Options{
		MetricsPath:       a.cfg.WebTelemetryPath,
		EnableLifecycle:   a.cfg.WebEnableLifecycle,
		ReadyRequiresScan: a.cfg.WebReadyRequiresScan,
		Ready:             a.firstCycleComplete.Load,
		Reload:            a.reload,
	})

	go a.scanLoop(ctx)
	go a.refreshLoop(ctx)
	go a.watchReloadSignals(ctx)

	errCh := make(chan error, 1)
	go func() { errCh <- web.ListenAndServe(httpServer, webFlags, logger) }()
	logger.Info("Listening", "addresses", *webFlags.WebListenAddresses, "telemetry_path", a.cfg.WebTelemetryPath)

	select {
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("http server: %w", err)
		}
		return nil
	case <-ctx.Done():
		logger.Info("Shutting down")
		// Reported unhealthy before the listener closes, so a load balancer can drain scrapes
		// rather than seeing connections refused.
		control.SetHealthy(false)

		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		if err := httpServer.Shutdown(shutdownCtx); err != nil {
			logger.Error("HTTP server shutdown failed", "err", err)
		}
		if err := <-errCh; err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("http server: %w", err)
		}
		return nil
	}
}

// scanLoop runs cycles back to back with the configured gap between them.
//
// The interval starts after a cycle finishes rather than on a fixed schedule, so a tree that takes
// longer than the interval to walk does not queue up continuous rescans.
func (a *application) scanLoop(ctx context.Context) {
	for {
		a.runCycle(ctx)
		if ctx.Err() != nil {
			return
		}
		timer := time.NewTimer(a.cfg.ScanInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

func (a *application) runCycle(ctx context.Context) {
	paths := a.currentPaths()
	if len(paths) == 0 {
		return
	}
	if a.cfg.Filesystem {
		a.fsCache.Refresh(ctx, paths)
	}
	if !a.engine.ScanAll(ctx, paths, a.metrics.Recorder()) {
		return
	}
	if ctx.Err() == nil {
		a.firstCycleComplete.Store(true)
	}
}

func (a *application) currentPaths() []string {
	snapshots := a.store.Snapshot()
	paths := make([]string, 0, len(snapshots))
	for _, snapshot := range snapshots {
		paths = append(paths, snapshot.Target)
	}
	return paths
}

// refreshLoop re-expands glob patterns so directories created after startup are picked up without
// a restart, which on a host with dated log paths is the difference between monitoring today's
// data and yesterday's.
func (a *application) refreshLoop(ctx context.Context) {
	ticker := time.NewTicker(a.cfg.TargetsRefreshInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := a.reload(); err != nil {
				a.logger.Error("Target refresh failed; keeping the previous target set", "err", err)
			}
		}
	}
}

// reload re-resolves targets and applies the result. It is also what SIGHUP and /-/reload call.
func (a *application) reload() error {
	resolution, err := a.resolver.Resolve()
	if err != nil {
		return err
	}
	logResolution(a.logger, resolution)
	// Dropping a removed target is what garbage-collects its series; leaving it would report a
	// frozen size forever.
	a.store.SetTargets(resolution.Paths())
	return nil
}

func (a *application) watchReloadSignals(ctx context.Context) {
	signals := make(chan os.Signal, 1)
	notifyReload(signals)
	defer signal.Stop(signals)
	for {
		select {
		case <-ctx.Done():
			return
		case <-signals:
			a.logger.Info("Reload signal received")
			if err := a.reload(); err != nil {
				a.logger.Error("Reload failed; keeping the previous configuration", "err", err)
			}
		}
	}
}

func logResolution(logger *slog.Logger, resolution targets.Resolution) {
	for _, warning := range resolution.Warnings {
		logger.Warn("Target resolution warning", "detail", warning)
	}
	for _, target := range resolution.Targets {
		if len(target.Overlaps) > 0 {
			// Not corrected automatically: overlap is occasionally intentional, and guessing which
			// target to drop would be worse than reporting the double-counting.
			logger.Warn("Target is contained by another target, so its bytes are counted twice",
				"target", target.Path, "contained_by", target.Overlaps)
		}
	}
	for _, path := range resolution.Added {
		logger.Info("Target added", "target", path)
	}
	for _, path := range resolution.Removed {
		logger.Info("Target removed", "target", path)
	}
	logger.Info("Targets resolved", "count", len(resolution.Targets))
}
