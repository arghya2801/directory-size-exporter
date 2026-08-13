package config

import (
	"fmt"
	"log/slog"
	"runtime"
	"sort"
	"time"

	"github.com/prometheus/exporter-toolkit/web"

	"github.com/local/directory-size-exporter/internal/fsstat"
)

// defaultHardTimeoutBackstop bounds a scan when no scan timeout is configured. It exists purely so
// that "no timeout configured" never means "wait forever on a stuck worker".
const defaultHardTimeoutBackstop = 24 * time.Hour

// Resolved is the configuration after validation and capability gating, with every tristate
// collapsed to a plain boolean. Downstream packages take this rather than Config so they cannot
// accidentally act on an unresolved tristate.
type Resolved struct {
	*Config

	OneFilesystem  bool
	DedupHardlinks bool
	DiskUsage      bool
	Filesystem     bool

	// Capabilities is what the platform can actually measure.
	Capabilities fsstat.Capability
	// Web is the listener configuration in the shape exporter-toolkit expects, built from settings
	// that went through the same flag, environment and YAML resolution as everything else.
	Web *web.FlagConfig
	// Disabled records features switched off because the platform cannot support them, so the
	// audit can explain a missing metric without the operator reading the source.
	Disabled map[string]string
}

// Validate checks ranges and resolves capability-dependent settings.
//
// It returns an error rather than logging and continuing whenever a value would make the exporter
// silently do something other than what was asked. Refusing to start is recoverable in seconds;
// a metric that quietly never appears is not noticed for weeks.
func (r *Registry) Validate(caps fsstat.Capability, logger *slog.Logger) (*Resolved, error) {
	cfg := r.cfg
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}

	if len(cfg.TargetPatterns) == 0 {
		return nil, fmt.Errorf("at least one --path.target is required")
	}
	for _, check := range []struct {
		name   string
		value  time.Duration
		zeroOK bool
	}{
		{"scan.interval", cfg.ScanInterval, false},
		{"scan.timeout", cfg.ScanTimeout, true},
		{"scan.hard-timeout", cfg.ScanHardTimeout, true},
		{"scan.stale-after", cfg.ScanStaleAfter, true},
		{"scan.statfs-timeout", cfg.ScanStatfsTimeout, false},
		{"scan.worker-drain-timeout", cfg.ScanWorkerDrainTimeout, false},
		{"targets.refresh-interval", cfg.TargetsRefreshInterval, false},
		{"targets.retain-vanished", cfg.TargetsRetainVanished, true},
		{"log.scan-heartbeat", cfg.LogScanHeartbeat, true},
		{"log.heartbeat-interval", cfg.LogHeartbeatInterval, false},
	} {
		if check.value < 0 || (!check.zeroOK && check.value == 0) {
			return nil, fmt.Errorf("--%s must be greater than zero", check.name)
		}
	}
	for _, check := range []struct {
		name  string
		value int
	}{
		{"scan.concurrency", cfg.ScanConcurrency},
		{"scan.batch-size", cfg.ScanBatchSize},
		{"scan.share-threshold", cfg.ScanShareThreshold},
		{"scan.queue-limit", cfg.ScanQueueLimit},
		{"targets.max", cfg.TargetsMax},
	} {
		if check.value <= 0 {
			return nil, fmt.Errorf("--%s must be greater than zero", check.name)
		}
	}
	if cfg.ScanRateLimit < 0 {
		return nil, fmt.Errorf("--scan.rate-limit cannot be negative")
	}
	if cfg.ScanMaxTrackedInodes < 0 {
		return nil, fmt.Errorf("--scan.max-tracked-inodes cannot be negative")
	}
	if cfg.WebTelemetryPath == "" || cfg.WebTelemetryPath[0] != '/' {
		return nil, fmt.Errorf("--web.telemetry-path must start with /")
	}
	if len(cfg.WebListenAddresses) == 0 && !cfg.WebSystemdSocket {
		return nil, fmt.Errorf("--web.listen-address is required unless --web.systemd-socket is set")
	}
	if cfg.WebSystemdSocket && runtime.GOOS != "linux" {
		// Failing here rather than at listen time, so the reason is obvious rather than surfacing
		// as an opaque socket error after startup appears to have succeeded.
		return nil, fmt.Errorf("--web.systemd-socket is only supported on Linux, not %s", runtime.GOOS)
	}
	switch cfg.ScanIOPriority {
	case "idle", "best-effort", "none":
	default:
		return nil, fmt.Errorf("--scan.io-priority must be idle, best-effort or none, got %q", cfg.ScanIOPriority)
	}
	if cfg.ScanNice < -20 || cfg.ScanNice > 19 {
		return nil, fmt.Errorf("--scan.nice must be between -20 and 19, got %d", cfg.ScanNice)
	}

	// A hard timeout at or below the scan timeout would abandon targets that are merely slow,
	// discarding work that was about to finish.
	if cfg.ScanHardTimeout > 0 && cfg.ScanTimeout > 0 && cfg.ScanHardTimeout <= cfg.ScanTimeout {
		return nil, fmt.Errorf("--scan.hard-timeout (%s) must exceed --scan.timeout (%s), or a merely slow target is abandoned",
			cfg.ScanHardTimeout, cfg.ScanTimeout)
	}
	// The hard timeout is the only thing that reclaims a scan whose worker is stuck in an
	// uninterruptible filesystem call, and a stuck scan blocks the cycle permanently rather than
	// just failing. It must therefore always have a value.
	//
	// Deriving it solely from scan.timeout left the default configuration — where scan.timeout is
	// disabled — with no protection at all against the exact failure this engine is built to
	// survive, and with no metric to reveal it either, since the abandoned-worker gauge is only
	// written once a cycle finishes.
	if cfg.ScanHardTimeout == 0 {
		if cfg.ScanTimeout > 0 {
			cfg.ScanHardTimeout = 2 * cfg.ScanTimeout
		} else {
			// Far above any plausible walk, so it can only fire on a genuinely stuck worker rather
			// than on a merely slow tree. A heavily rate-limited scan of a very large tree can
			// legitimately exceed this, which is why it stays configurable.
			cfg.ScanHardTimeout = defaultHardTimeoutBackstop
			logger.Warn("No --scan.timeout is set, so scans are unbounded; applying a hard-timeout backstop so a hung mount cannot stall scanning forever",
				"scan.hard-timeout", cfg.ScanHardTimeout,
				"suggestion", "set --scan.timeout to bound normal scans, or raise --scan.hard-timeout if a single scan legitimately runs longer than this")
		}
		r.markDerived("scan.hard-timeout")
	}

	// Withdrawing a measurement sooner than the next scan can replace it makes the series flap in
	// and out on every cycle, which reads as an exporter fault rather than as the staleness signal
	// it is meant to be.
	if cfg.ScanStaleAfter > 0 && cfg.ScanStaleAfter <= cfg.ScanInterval {
		return nil, fmt.Errorf("--scan.stale-after (%s) must exceed --scan.interval (%s), or the measurement is withdrawn between every scan; allow room for the scan itself to run",
			cfg.ScanStaleAfter, cfg.ScanInterval)
	}

	resolved := &Resolved{
		Config:       cfg,
		Capabilities: caps,
		Disabled:     map[string]string{},
		Web: &web.FlagConfig{
			WebListenAddresses: &cfg.WebListenAddresses,
			WebConfigFile:      &cfg.WebConfigFile,
			WebSystemdSocket:   &cfg.WebSystemdSocket,
		},
	}
	for _, gate := range []struct {
		name  string
		value Tristate
		need  fsstat.Capability
		out   *bool
	}{
		{"scan.one-filesystem", cfg.ScanOneFilesystem, fsstat.CapInode, &resolved.OneFilesystem},
		{"scan.dedup-hardlinks", cfg.ScanDedupHardlinks, fsstat.CapInode, &resolved.DedupHardlinks},
		{"collector.disk-usage", cfg.CollectorDiskUsage, fsstat.CapAllocBytes, &resolved.DiskUsage},
		{"collector.filesystem", cfg.CollectorFilesystem, fsstat.CapFSBytes, &resolved.Filesystem},
	} {
		enabled, err := resolveGate(gate.name, gate.value, gate.need, caps, resolved.Disabled, logger)
		if err != nil {
			return nil, err
		}
		*gate.out = enabled
	}

	// Disk usage counts hard-linked files once per link unless dedup is on, so on a tree where
	// archives are hard-linked the number silently overstates real consumption. Warning rather
	// than failing, because plenty of log trees have no hard links at all.
	if resolved.DiskUsage && !resolved.DedupHardlinks && caps.Has(fsstat.CapInode) {
		logger.Warn("Disk usage is enabled without hard-link dedup; files with multiple links are counted once per link",
			"suggestion", "--scan.dedup-hardlinks=true")
	}
	return resolved, nil
}

// resolveGate applies the tristate policy for one capability-dependent setting.
func resolveGate(name string, value Tristate, need, caps fsstat.Capability, disabled map[string]string, logger *slog.Logger) (bool, error) {
	switch value {
	case TriFalse:
		return false, nil
	case TriTrue:
		if !caps.Has(need) {
			// Explicitly requested and impossible. Failing loudly at startup is the whole point of
			// the tristate: the alternative is a panel that is empty for weeks before anyone asks.
			return false, fmt.Errorf("--%s=true requires the %s capability, which is unavailable on this platform (available: %s)",
				name, need, caps)
		}
		return true, nil
	default:
		if !caps.Has(need) {
			reason := fmt.Sprintf("requires the %s capability, unavailable on this platform", need)
			disabled[name] = reason
			logger.Warn("Feature disabled because the platform does not support it", "feature", name, "reason", reason)
			return false, nil
		}
		return true, nil
	}
}

// LogAudit prints every setting with the source it came from.
//
// This is what makes "why is it behaving this way" answerable from the logs alone, which matters
// most on a host where the answer is spread across a flag, an environment variable set by the unit
// file, and a configuration file managed by someone else.
func (r *Registry) LogAudit(logger *slog.Logger, resolved *Resolved) {
	if logger == nil {
		return
	}
	fields := append([]*field(nil), r.fields...)
	sort.Slice(fields, func(i, j int) bool { return fields[i].name < fields[j].name })

	logger.Info("Effective configuration", "settings", len(fields))
	for _, f := range fields {
		// Deliberately not keyed "source": promslog reserves that for the code location and renames
		// any attribute that collides, so the audit key would silently differ between a plain
		// handler and the one the binary actually uses — and an operator grepping the field name
		// from the documentation would find nothing.
		logger.Info("Setting resolved", "name", f.name, "value", f.value(), "origin", string(f.source))
	}
	for name, reason := range resolved.Disabled {
		logger.Warn("Setting disabled by platform", "name", name, "reason", reason)
	}
	logger.Info("Platform capabilities", "available", resolved.Capabilities.String())
}
