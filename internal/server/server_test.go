package server

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/exporter-toolkit/web"
	"golang.org/x/crypto/bcrypt"
)

func newTestServer(t *testing.T, opts Options) (*httptest.Server, *Server) {
	t.Helper()
	registry := prometheus.NewRegistry()
	registry.MustRegister(prometheus.NewGaugeFunc(
		prometheus.GaugeOpts{Name: "dir_exporter_size_bytes", Help: "test"},
		func() float64 { return 42 },
	))
	handler, srv := New(registry, opts)
	test := httptest.NewServer(handler.Handler)
	t.Cleanup(test.Close)
	return test, srv
}

func get(t *testing.T, url string) (int, string) {
	t.Helper()
	response, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return response.StatusCode, string(body)
}

func TestServer_MetricsEndpoint(t *testing.T) {
	test, _ := newTestServer(t, Options{})
	status, body := get(t, test.URL+"/metrics")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if !strings.Contains(body, "dir_exporter_size_bytes") {
		t.Error("metrics response did not contain the expected series")
	}
}

// TestServer_LandingPage replaces the previous behaviour of returning 404 at the root, which made
// anyone checking whether the exporter was up conclude that it was not.
func TestServer_LandingPage(t *testing.T) {
	test, _ := newTestServer(t, Options{})

	status, body := get(t, test.URL+"/")
	if status != http.StatusOK {
		t.Fatalf("root status = %d, want 200", status)
	}
	if !strings.Contains(body, "/metrics") {
		t.Error("landing page does not link to the metrics endpoint")
	}

	// Unknown paths must still 404 rather than serving the landing page for everything.
	if status, _ := get(t, test.URL+"/nope"); status != http.StatusNotFound {
		t.Errorf("unknown path status = %d, want 404", status)
	}
}

func TestServer_HealthyReflectsShutdown(t *testing.T) {
	test, srv := newTestServer(t, Options{})
	if status, _ := get(t, test.URL+"/-/healthy"); status != http.StatusOK {
		t.Fatalf("healthy status = %d, want 200", status)
	}

	// Marking unhealthy before the listener closes lets a load balancer drain scrapes first.
	srv.SetHealthy(false)
	if status, _ := get(t, test.URL+"/-/healthy"); status != http.StatusServiceUnavailable {
		t.Errorf("healthy status during shutdown = %d, want 503", status)
	}
}

func TestServer_ReadyWaitsForTheFirstScan(t *testing.T) {
	var scanned atomic.Bool
	test, _ := newTestServer(t, Options{
		ReadyRequiresScan: true,
		Ready:             scanned.Load,
	})

	// A freshly started exporter has no measurements. Reporting ready would let a deployment gate
	// treat "not scanned yet" as healthy, and the first walk of a large tree takes minutes.
	if status, _ := get(t, test.URL+"/-/ready"); status != http.StatusServiceUnavailable {
		t.Fatalf("ready status before the first scan = %d, want 503", status)
	}
	scanned.Store(true)
	if status, _ := get(t, test.URL+"/-/ready"); status != http.StatusOK {
		t.Errorf("ready status after the first scan = %d, want 200", status)
	}
}

func TestServer_ReadyIgnoresScanStateWhenNotRequired(t *testing.T) {
	test, _ := newTestServer(t, Options{ReadyRequiresScan: false, Ready: func() bool { return false }})
	if status, _ := get(t, test.URL+"/-/ready"); status != http.StatusOK {
		t.Errorf("ready status = %d, want 200 when the scan requirement is disabled", status)
	}
}

func TestServer_ReloadIsOffByDefault(t *testing.T) {
	var reloads atomic.Int64
	test, _ := newTestServer(t, Options{Reload: func() error { reloads.Add(1); return nil }})

	// The exporter is commonly deployed without authentication on a trusted network, so an
	// unauthenticated endpoint that triggers work must be opt-in.
	response, err := http.Post(test.URL+"/-/reload", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusNotFound {
		t.Errorf("reload status = %d, want 404 while lifecycle is disabled", response.StatusCode)
	}
	if reloads.Load() != 0 {
		t.Error("reload ran while the lifecycle endpoint was disabled")
	}
}

func TestServer_ReloadRequiresPost(t *testing.T) {
	var reloads atomic.Int64
	test, _ := newTestServer(t, Options{
		EnableLifecycle: true,
		Reload:          func() error { reloads.Add(1); return nil },
	})

	// A GET must not trigger work: crawlers, health checkers and browsers all issue GETs.
	if status, _ := get(t, test.URL+"/-/reload"); status != http.StatusMethodNotAllowed {
		t.Errorf("GET status = %d, want 405", status)
	}
	if reloads.Load() != 0 {
		t.Fatal("a GET triggered a reload")
	}

	response, err := http.Post(test.URL+"/-/reload", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Errorf("POST status = %d, want 200", response.StatusCode)
	}
	if reloads.Load() != 1 {
		t.Errorf("reloads = %d, want 1", reloads.Load())
	}
}

// TestServer_BasicAuthViaWebConfig proves the exporter-toolkit wiring actually works. The previous
// implementation imported the toolkit but hard-coded an empty config path, so TLS and
// authentication were unreachable no matter what the operator configured.
func TestServer_BasicAuthViaWebConfig(t *testing.T) {
	dir := t.TempDir()
	// Generated rather than hardcoded: a stale literal hash silently degrades this into a test
	// that only ever proves the 401 path.
	hash, err := bcrypt.GenerateFromPassword([]byte("secret"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(dir, "web-config.yml")
	config := "basic_auth_users:\n  prometheus: " + strconv.Quote(string(hash)) + "\n"
	if err := os.WriteFile(configPath, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}

	// Authentication lives inside web.Serve rather than an exported handler wrapper, so the test
	// drives the real serving path the binary uses. Anything less would not prove the wiring.
	baseURL := serveWithWebConfig(t, configPath)

	if status, _ := get(t, baseURL+"/metrics"); status != http.StatusUnauthorized {
		t.Errorf("status without credentials = %d, want 401", status)
	}

	request, _ := http.NewRequest(http.MethodGet, baseURL+"/metrics", nil)
	request.SetBasicAuth("prometheus", "secret")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Errorf("status with credentials = %d, want 200", response.StatusCode)
	}
}

// serveWithWebConfig starts the server through exporter-toolkit with the given web config and
// returns its base URL.
func serveWithWebConfig(t *testing.T, configPath string) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	registry := prometheus.NewRegistry()
	handler, _ := New(registry, Options{})
	addresses := []string{listener.Addr().String()}
	systemdSocket := false
	flags := &web.FlagConfig{
		WebListenAddresses: &addresses,
		WebSystemdSocket:   &systemdSocket,
		WebConfigFile:      &configPath,
	}

	served := make(chan error, 1)
	go func() {
		served <- web.Serve(listener, handler, flags, slog.New(slog.DiscardHandler))
	}()
	t.Cleanup(func() {
		_ = handler.Close()
		<-served
	})
	return "http://" + listener.Addr().String()
}

// logCapture records log records so a test can assert on what was reported outside a response.
type logCapture struct {
	mu      sync.Mutex
	records []slog.Record
}

func (h *logCapture) Enabled(context.Context, slog.Level) bool { return true }
func (h *logCapture) WithAttrs([]slog.Attr) slog.Handler       { return h }
func (h *logCapture) WithGroup(string) slog.Handler            { return h }

func (h *logCapture) Handle(_ context.Context, record slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.records = append(h.records, record.Clone())
	return nil
}

func (h *logCapture) messages() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]string, 0, len(h.records))
	for _, record := range h.records {
		out = append(out, record.Message)
	}
	return out
}

// TestServer_SlowReloadAnswersBeforeTheWriteDeadline covers a reload that outlasts the response
// budget. A reload re-expands globs and stats every target, which on a slow or partially-hung
// mount can exceed the server's write timeout; answering late would truncate the response and make
// a succeeding operation look like a failure.
func TestServer_SlowReloadAnswersBeforeTheWriteDeadline(t *testing.T) {
	release := make(chan struct{})
	test, _ := newTestServer(t, Options{
		EnableLifecycle: true,
		ReloadTimeout:   50 * time.Millisecond,
		Reload: func() error {
			<-release
			return nil
		},
	})

	response, err := http.Post(test.URL+"/-/reload", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	close(release)

	if response.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 while the reload is still running", response.StatusCode)
	}
	if !strings.Contains(string(body), "background") {
		t.Errorf("body = %q, want it to say the reload is continuing", body)
	}
}

// TestServer_SlowReloadFailureIsLogged is the guard against losing an error entirely.
//
// Once the response deadline fires the caller has already been told the reload is continuing, so
// an error arriving afterwards has nowhere left to go. That is the worst case to drop: slowness
// and failure share a cause, a struggling filesystem, so the reload most likely to fail is the one
// most likely to answer late. Without this the operator reads 202 and concludes it worked.
func TestServer_SlowReloadFailureIsLogged(t *testing.T) {
	release := make(chan struct{})
	logs := &logCapture{}
	test, _ := newTestServer(t, Options{
		EnableLifecycle: true,
		ReloadTimeout:   50 * time.Millisecond,
		Logger:          slog.New(logs),
		Reload: func() error {
			<-release
			return errors.New("glob expanded past --targets.max")
		},
	})

	response, err := http.Post(test.URL+"/-/reload", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", response.StatusCode)
	}

	// The caller has been answered; only now does the reload fail.
	close(release)

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		for _, message := range logs.messages() {
			if strings.Contains(message, "Reload failed") {
				return
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("a reload that failed after the response deadline was never logged; messages: %v", logs.messages())
}

func TestServer_FastReloadFailureIsReportedAndLogged(t *testing.T) {
	logs := &logCapture{}
	test, _ := newTestServer(t, Options{
		EnableLifecycle: true,
		Logger:          slog.New(logs),
		Reload:          func() error { return errors.New("boom") },
	})

	response, err := http.Post(test.URL+"/-/reload", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", response.StatusCode)
	}
	var logged bool
	for _, message := range logs.messages() {
		if strings.Contains(message, "Reload failed") {
			logged = true
		}
	}
	if !logged {
		t.Error("a reload failure returned in the response was not logged")
	}
}

func TestServer_TimeoutsAreSet(t *testing.T) {
	registry := prometheus.NewRegistry()
	handler, _ := New(registry, Options{})
	// An exporter reachable from a network needs these; without ReadHeaderTimeout a single slow
	// client can hold a connection open indefinitely.
	if handler.ReadHeaderTimeout == 0 || handler.WriteTimeout == 0 || handler.IdleTimeout == 0 {
		t.Errorf("server timeouts are not configured: %+v", handler)
	}
	if handler.MaxHeaderBytes == 0 {
		t.Error("MaxHeaderBytes is unset")
	}
}

func TestServer_TLSViaWebConfig(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath := writeSelfSignedCert(t, dir)
	configPath := filepath.Join(dir, "web-config.yml")
	config := "tls_server_config:\n  cert_file: " + filepath.ToSlash(certPath) +
		"\n  key_file: " + filepath.ToSlash(keyPath) + "\n"
	if err := os.WriteFile(configPath, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}

	baseURL := serveWithWebConfig(t, configPath)
	client := &http.Client{Transport: &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // self-signed test cert
	}}
	response, err := client.Get(strings.Replace(baseURL, "http://", "https://", 1) + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Errorf("TLS status = %d, want 200", response.StatusCode)
	}
}

func writeSelfSignedCert(t *testing.T, dir string) (string, string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	template := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "localhost"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}

	certPath := filepath.Join(dir, "cert.pem")
	keyPath := filepath.Join(dir, "key.pem")
	writePEM(t, certPath, &pem.Block{Type: "CERTIFICATE", Bytes: der})
	writePEM(t, keyPath, &pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	return certPath, keyPath
}

func writePEM(t *testing.T, path string, block *pem.Block) {
	t.Helper()
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if err := pem.Encode(file, block); err != nil {
		t.Fatal(err)
	}
}

func TestServer_CustomMetricsPath(t *testing.T) {
	test, _ := newTestServer(t, Options{MetricsPath: "/custom"})
	if status, _ := get(t, test.URL+"/custom"); status != http.StatusOK {
		t.Errorf("custom metrics path status = %d, want 200", status)
	}
	_, body := get(t, test.URL+"/")
	if !strings.Contains(body, "/custom") {
		t.Error("landing page does not link to the configured metrics path")
	}
}
