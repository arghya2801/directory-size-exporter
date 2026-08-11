package exporter

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

func TestMetricsEndpointAcceptance(t *testing.T) {
	target := t.TempDir()
	if err := os.WriteFile(filepath.Join(target, "access.log"), []byte("log-entry"), 0o600); err != nil {
		t.Fatal(err)
	}
	collector := NewDirectoryCollector([]string{target}, true)
	collector.ScanAll(context.Background(), 0, nil)
	registry := prometheus.NewRegistry()
	registry.MustRegister(collector)
	server := httptest.NewServer(NewHTTPServer(registry).Handler)
	defer server.Close()
	response, err := http.Get(server.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("metrics status = %d", response.StatusCode)
	}
	for _, expected := range []string{"dir_exporter_size_bytes", "dir_exporter_last_scan_success", "dir_exporter_scan_errors_total"} {
		if !strings.Contains(string(body), expected) {
			t.Errorf("missing %q from metrics response", expected)
		}
	}
	response, err = http.Get(server.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("unexpected non-metrics endpoint status = %d", response.StatusCode)
	}
}
