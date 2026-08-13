// Package server exposes the metrics endpoint and the operational endpoints around it.
package server

import (
	"fmt"
	"log/slog"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/prometheus/exporter-toolkit/web"
)

// Options configures the HTTP surface.
type Options struct {
	MetricsPath string
	// EnableLifecycle exposes POST /-/reload. It is off by default because an unauthenticated
	// reload endpoint lets anyone force repeated reloads, and the exporter is commonly deployed
	// without authentication on a trusted network.
	EnableLifecycle bool
	// ReadyRequiresScan holds /-/ready at 503 until Ready reports true. That is usually what you
	// want: a freshly started exporter has no measurements, and satisfying a deployment gate on it
	// would treat "not scanned yet" as healthy.
	ReadyRequiresScan bool
	// Reload is invoked by the lifecycle endpoint. Nil disables the endpoint regardless.
	Reload func() error
	// Ready reports whether the exporter has enough data to be worth scraping. The caller decides
	// what that means; see the note on readiness in README.
	Ready func() bool
	// ReloadTimeout bounds how long a reload request waits before answering. Zero uses
	// defaultReloadTimeout.
	ReloadTimeout time.Duration
	// Logger records outcomes that no longer have a request to be reported on, which is the whole
	// reason it exists here; see handleReload.
	Logger *slog.Logger
}

// Server wires the registry and operational endpoints into an http.Server.
type Server struct {
	opts    Options
	healthy atomic.Bool
}

// New returns the HTTP server. It does not listen; the caller drives that through
// exporter-toolkit so TLS and authentication come from --web.config.file.
func New(registry *prometheus.Registry, opts Options) (*http.Server, *Server) {
	if opts.MetricsPath == "" {
		opts.MetricsPath = "/metrics"
	}
	srv := &Server{opts: opts}
	srv.healthy.Store(true)

	mux := http.NewServeMux()
	mux.Handle(opts.MetricsPath, promhttp.HandlerFor(registry, promhttp.HandlerOpts{
		// A scrape reads an in-memory snapshot, so a failure here is a bug in rendering rather
		// than a transient condition worth hiding behind a partial response.
		ErrorHandling: promhttp.HTTPErrorOnError,
	}))
	mux.HandleFunc("/-/healthy", srv.handleHealthy)
	mux.HandleFunc("/-/ready", srv.handleReady)
	if opts.EnableLifecycle && opts.Reload != nil {
		mux.HandleFunc("/-/reload", srv.handleReload)
	}
	mux.Handle("/", landingHandler(opts.MetricsPath))

	return &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    8 << 10,
	}, srv
}

// SetHealthy marks the process unhealthy, which is used during shutdown so a load balancer stops
// sending scrapes before the listener closes.
func (s *Server) SetHealthy(healthy bool) { s.healthy.Store(healthy) }

func (s *Server) handleHealthy(w http.ResponseWriter, _ *http.Request) {
	if !s.healthy.Load() {
		http.Error(w, "shutting down", http.StatusServiceUnavailable)
		return
	}
	writePlain(w, http.StatusOK, "OK")
}

func (s *Server) handleReady(w http.ResponseWriter, _ *http.Request) {
	if s.opts.ReadyRequiresScan && s.opts.Ready != nil && !s.opts.Ready() {
		// Not an error: the first walk of a multi-terabyte tree legitimately takes minutes.
		writePlain(w, http.StatusServiceUnavailable, "waiting for the first scan cycle to complete")
		return
	}
	writePlain(w, http.StatusOK, "READY")
}

// defaultReloadTimeout bounds how long a reload request waits before answering. It sits below the
// server's write timeout on purpose: a reload re-expands globs and stats every target, which on a
// slow or partially-hung mount can outlast the write deadline. Without this the response would be
// truncated mid-flight and the caller would read a failure for an operation that actually
// succeeded.
const defaultReloadTimeout = 20 * time.Second

func (s *Server) handleReload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost && r.Method != http.MethodPut {
		w.Header().Set("Allow", "POST, PUT")
		http.Error(w, "reload requires POST", http.StatusMethodNotAllowed)
		return
	}

	// The reload outlives this request when it is slow, so its outcome is logged from inside the
	// goroutine rather than only reported through the response.
	//
	// Reporting it through the response alone loses it entirely on the slow path: once the
	// deadline below fires the caller has already been told the reload is continuing, and an error
	// arriving afterwards would have nowhere left to go. That is the worst case to drop, because
	// slowness and failure share a cause — a struggling filesystem — so the reload most likely to
	// fail is exactly the one most likely to answer late. The caller would read 202 and conclude
	// the reload worked.
	//
	// Buffered so the goroutine can finish and exit even once nobody is waiting for it.
	done := make(chan error, 1)
	go func() {
		err := s.opts.Reload()
		if err != nil {
			s.logger().Error("Reload failed", "err", err)
		}
		done <- err
	}()

	timer := time.NewTimer(s.reloadTimeout())
	defer timer.Stop()
	select {
	case err := <-done:
		if err != nil {
			http.Error(w, "reload failed: "+err.Error(), http.StatusInternalServerError)
			return
		}
		writePlain(w, http.StatusOK, "reloaded")
	case <-timer.C:
		// Still running and will finish; only the answer is being cut short. Whatever it returns
		// is logged by the goroutine above.
		writePlain(w, http.StatusAccepted,
			"reload is taking longer than "+s.reloadTimeout().String()+" and is continuing in the background; check the logs for its outcome")
	}
}

func (s *Server) reloadTimeout() time.Duration {
	if s.opts.ReloadTimeout > 0 {
		return s.opts.ReloadTimeout
	}
	return defaultReloadTimeout
}

func (s *Server) logger() *slog.Logger {
	if s.opts.Logger != nil {
		return s.opts.Logger
	}
	return slog.New(slog.DiscardHandler)
}

func writePlain(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(status)
	fmt.Fprintln(w, body)
}

// landingHandler serves a minimal index at the root and a 404 everywhere else.
//
// The previous implementation returned 404 for the root, which makes a human checking whether the
// exporter is up conclude that it is not.
func landingHandler(metricsPath string) http.Handler {
	page, err := web.NewLandingPage(web.LandingConfig{
		Name:        "Directory Size Exporter",
		Description: "Per-directory size and capacity metrics for Prometheus.",
		Links:       []web.LandingLinks{{Address: metricsPath, Text: "Metrics"}},
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		if err != nil {
			writePlain(w, http.StatusOK, "Directory Size Exporter\n"+metricsPath)
			return
		}
		page.ServeHTTP(w, r)
	})
}
