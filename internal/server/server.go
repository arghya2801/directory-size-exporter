// Package server exposes the metrics endpoint and the operational endpoints around it.
package server

import (
	"fmt"
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

func (s *Server) handleReload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost && r.Method != http.MethodPut {
		w.Header().Set("Allow", "POST, PUT")
		http.Error(w, "reload requires POST", http.StatusMethodNotAllowed)
		return
	}
	if err := s.opts.Reload(); err != nil {
		http.Error(w, "reload failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writePlain(w, http.StatusOK, "reloaded")
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
