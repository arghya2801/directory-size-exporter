package collector

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	versioncollector "github.com/prometheus/client_golang/prometheus/collectors/version"
)

// RegisterSelf adds the exporter's own runtime metrics to a registry.
//
// The previous implementation built a bare registry and never registered these, so the exporter
// published nothing about itself: no goroutine counts, no memory, no build identity. That makes an
// exporter leaking memory or wedged on a hung mount invisible to the very monitoring system it
// feeds, and makes "which build is on this host" unanswerable from metrics.
//
// includeRuntime gates go_* and process_* so a deployment can drop them if node_exporter already
// covers process-level data. build_info is always registered; it is a single series and is the
// only way to correlate behaviour with a release.
func RegisterSelf(registry prometheus.Registerer, includeRuntime bool) error {
	if err := registry.Register(versioncollector.NewCollector(Namespace)); err != nil {
		return err
	}
	if !includeRuntime {
		return nil
	}
	if err := registry.Register(collectors.NewGoCollector()); err != nil {
		return err
	}
	return registry.Register(collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
}
