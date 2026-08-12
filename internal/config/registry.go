package config

import (
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/alecthomas/kingpin/v2"
	"gopkg.in/yaml.v3"

	"github.com/local/directory-size-exporter/internal/fsstat"
)

type kind int

const (
	kindString kind = iota
	kindStrings
	kindInt
	kindFloat
	kindBool
	kindDuration
	kindTristate
)

// field binds one Config field to its flag, environment variable, YAML key and validation.
//
// Keeping all four in one place is what stops them drifting apart. The usual failure is a setting
// that works as a flag but is silently ignored in YAML, which is maddening to debug because the
// configuration file looks correct.
type field struct {
	name string // flag name, also the dotted YAML path
	help string
	kind kind
	ptr  any
	def  string
	// yamlAlias is an additional, friendlier YAML key. Nesting "path.target" reads badly, so the
	// target list is also reachable as a top-level "targets".
	yamlAlias string
	// requires is the platform capability this setting depends on, for tristate fields.
	requires fsstat.Capability
	// noYAML marks settings that only make sense on the command line.
	noYAML bool

	setByFlag bool
	source    Source
}

// Registry holds every field, and is the single source of truth for the whole configuration
// surface.
type Registry struct {
	cfg    *Config
	fields []*field
	byName map[string]*field
}

// NewRegistry builds the registry over a fresh Config.
func NewRegistry() *Registry {
	cfg := &Config{}
	r := &Registry{cfg: cfg, byName: map[string]*field{}}

	add := func(f *field) {
		f.source = SourceDefault
		r.fields = append(r.fields, f)
		r.byName[f.name] = f
		if f.yamlAlias != "" {
			r.byName[f.yamlAlias] = f
		}
	}

	add(&field{name: "path.target", kind: kindStrings, ptr: &cfg.TargetPatterns, yamlAlias: "targets",
		help: "Directory to monitor. Repeat for multiple, and glob patterns are expanded and re-resolved periodically."})
	add(&field{name: "targets.max", kind: kindInt, ptr: &cfg.TargetsMax, def: "200",
		help: "Maximum directories to monitor. Exceeding this fails startup rather than silently monitoring a subset."})
	add(&field{name: "targets.retain-vanished", kind: kindDuration, ptr: &cfg.TargetsRetainVanished, def: "10m",
		help: "How long to keep a target whose pattern stopped matching, so log rotation does not churn series."})
	add(&field{name: "targets.refresh-interval", kind: kindDuration, ptr: &cfg.TargetsRefreshInterval, def: "5m",
		help: "How often glob patterns are re-expanded to pick up new directories."})

	add(&field{name: "scan.interval", kind: kindDuration, ptr: &cfg.ScanInterval, def: "5m",
		help: "Delay between the end of one scan cycle and the start of the next."})
	add(&field{name: "scan.timeout", kind: kindDuration, ptr: &cfg.ScanTimeout, def: "0",
		help: "Maximum duration of one target's scan; 0 disables it."})
	add(&field{name: "scan.hard-timeout", kind: kindDuration, ptr: &cfg.ScanHardTimeout, def: "0",
		help: "Point at which a target is abandoned without waiting for workers presumed stuck on a hung mount; 0 defaults to twice scan.timeout."})
	add(&field{name: "scan.worker-drain-timeout", kind: kindDuration, ptr: &cfg.ScanWorkerDrainTimeout, def: "5s",
		help: "How long to wait for scan workers to exit before declaring them leaked."})
	add(&field{name: "scan.statfs-timeout", kind: kindDuration, ptr: &cfg.ScanStatfsTimeout, def: "5s",
		help: "Maximum duration of a filesystem capacity query before the mount is treated as hung."})

	add(&field{name: "scan.concurrency", kind: kindInt, ptr: &cfg.ScanConcurrency, def: "2",
		help: "Directories read simultaneously across all targets. Deliberately low to stay out of a latency-sensitive application's way."})
	add(&field{name: "scan.batch-size", kind: kindInt, ptr: &cfg.ScanBatchSize, def: "1024",
		help: "Directory entries read per batch. Cancellation and rate limiting are applied once per batch."})
	add(&field{name: "scan.share-threshold", kind: kindInt, ptr: &cfg.ScanShareThreshold, def: "32",
		help: "Local queue depth past which a worker shares work with idle workers."})
	add(&field{name: "scan.queue-limit", kind: kindInt, ptr: &cfg.ScanQueueLimit, def: "65536",
		help: "Maximum shared queue length. Reaching it is harmless; workers keep work locally instead of blocking."})
	add(&field{name: "scan.rate-limit", kind: kindFloat, ptr: &cfg.ScanRateLimit, def: "0",
		help: "Filesystem operations per second across all workers; 0 is unlimited. Around 20000 is a reasonable starting point on a busy host."})
	add(&field{name: "scan.rate-limit-burst", kind: kindInt, ptr: &cfg.ScanRateLimitBurst, def: "0",
		help: "Rate limiter burst; 0 derives it from scan.batch-size."})
	add(&field{name: "scan.max-tracked-inodes", kind: kindInt, ptr: &cfg.ScanMaxTrackedInodes, def: "2000000",
		help: "Cap on hard-link tracking entries per target. Beyond it duplicates are counted again and the condition is reported."})

	add(&field{name: "scan.one-filesystem", kind: kindTristate, ptr: &cfg.ScanOneFilesystem, def: "false",
		requires: fsstat.CapInode,
		help:     "Do not descend into directories on a different filesystem than the target root."})
	add(&field{name: "scan.dedup-hardlinks", kind: kindTristate, ptr: &cfg.ScanDedupHardlinks, def: "false",
		requires: fsstat.CapInode,
		help:     "Count hard-linked files once per target. Required for disk usage to be accurate where archives are hard-linked."})
	add(&field{name: "scan.publish-partial", kind: kindBool, ptr: &cfg.ScanPublishPartial, def: "false",
		help: "Publish sizes from scans that did not complete. A partial reading is systematically low and reads as freed space, so deltas and byte counters stay frozen even when this is on."})
	add(&field{name: "scan.stale-after", kind: kindDuration, ptr: &cfg.ScanStaleAfter, def: "0",
		help: "Stop publishing a measurement once it is older than this; 0 never withdraws it."})

	add(&field{name: "scan.nice", kind: kindInt, ptr: &cfg.ScanNice, def: "19",
		help: "CPU nice value applied to scan worker threads only, so the HTTP server stays responsive. Linux only."})
	add(&field{name: "scan.io-priority", kind: kindString, ptr: &cfg.ScanIOPriority, def: "idle",
		help: "I/O priority class for scan worker threads: idle, best-effort or none. Linux only."})

	add(&field{name: "collector.file-counts", kind: kindBool, ptr: &cfg.CollectorFileCounts, def: "false",
		help: "Expose regular-file and subdirectory counts."})
	add(&field{name: "collector.disk-usage", kind: kindTristate, ptr: &cfg.CollectorDiskUsage, def: "auto",
		requires: fsstat.CapAllocBytes,
		help:     "Expose allocated on-disk size, which differs from logical size for sparse and compressed files."})
	add(&field{name: "collector.growth", kind: kindBool, ptr: &cfg.CollectorGrowth, def: "true",
		help: "Expose size deltas and cumulative added/removed byte counters."})
	add(&field{name: "collector.filesystem", kind: kindTristate, ptr: &cfg.CollectorFilesystem, def: "auto",
		requires: fsstat.CapFSBytes,
		help:     "Expose capacity of the filesystem holding each target, for share-of-volume and time-to-full queries."})
	add(&field{name: "collector.self", kind: kindBool, ptr: &cfg.CollectorSelf, def: "true",
		help: "Expose the exporter's own Go runtime and process metrics."})
	add(&field{name: "collector.scan-histogram", kind: kindBool, ptr: &cfg.CollectorScanHistogram, def: "true",
		help: "Expose the distribution of scan durations by outcome."})

	add(&field{name: "web.telemetry-path", kind: kindString, ptr: &cfg.WebTelemetryPath, def: "/metrics",
		help: "Path under which to expose metrics."})
	add(&field{name: "web.enable-lifecycle", kind: kindBool, ptr: &cfg.WebEnableLifecycle, def: "false",
		help: "Enable POST /-/reload. Off by default because the exporter is commonly deployed unauthenticated."})
	add(&field{name: "web.ready-requires-scan", kind: kindBool, ptr: &cfg.WebReadyRequiresScan, def: "true",
		help: "Hold /-/ready at 503 until every target has completed one scan."})

	add(&field{name: "log.scan-heartbeat", kind: kindDuration, ptr: &cfg.LogScanHeartbeat, def: "30s",
		help: "How long a scan must run before progress lines are logged; 0 disables them."})
	add(&field{name: "log.heartbeat-interval", kind: kindDuration, ptr: &cfg.LogHeartbeatInterval, def: "30s",
		help: "Interval between scan progress lines."})

	add(&field{name: "config.file", kind: kindString, ptr: &cfg.ConfigFile, def: "", noYAML: true,
		help: "YAML configuration file. Flags and environment variables override its values."})

	return r
}

// Config returns the underlying settings.
func (r *Registry) Config() *Config { return r.cfg }

// Fields returns every registered field in declaration order.
func (r *Registry) Fields() []*field { return r.fields }

// RegisterFlags declares every field on the kingpin application.
func (r *Registry) RegisterFlags(app *kingpin.Application) {
	for _, f := range r.fields {
		clause := app.Flag(f.name, f.helpWithEnv())
		if f.def != "" {
			clause = clause.Default(f.def)
		}
		clause = clause.IsSetByUser(&f.setByFlag)
		switch f.kind {
		case kindString:
			clause.StringVar(f.ptr.(*string))
		case kindStrings:
			clause.StringsVar(f.ptr.(*[]string))
		case kindInt:
			clause.IntVar(f.ptr.(*int))
		case kindFloat:
			clause.Float64Var(f.ptr.(*float64))
		case kindBool:
			clause.BoolVar(f.ptr.(*bool))
		case kindDuration:
			clause.DurationVar(f.ptr.(*time.Duration))
		case kindTristate:
			clause.SetValue(f.ptr.(*Tristate))
		}
	}
}

// helpWithEnv appends the environment variable name so --help documents both ways to set a value.
func (f *field) helpWithEnv() string {
	return f.help + " (env: " + EnvName(f.name) + ")"
}

// Resolve applies YAML and environment values beneath any flags the user set explicitly, in
// precedence order flag > environment > YAML > default. It must be called after kingpin parsing.
func (r *Registry) Resolve(configPath string) error {
	// Marking flag-set fields first means later sources can simply skip them, rather than every
	// source needing to know about every other.
	for _, f := range r.fields {
		if f.setByFlag {
			f.source = SourceFlag
		}
	}
	if configPath != "" {
		if err := r.applyYAML(configPath); err != nil {
			return err
		}
	}
	return r.applyEnv()
}

func (r *Registry) applyEnv() error {
	for _, f := range r.fields {
		if f.setByFlag {
			continue
		}
		raw, ok := os.LookupEnv(EnvName(f.name))
		if !ok {
			continue
		}
		if err := f.set(raw); err != nil {
			return fmt.Errorf("environment variable %s: %w", EnvName(f.name), err)
		}
		f.source = SourceEnv
	}
	return nil
}

func (r *Registry) applyYAML(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read config file: %w", err)
	}

	var document map[string]any
	if err := yaml.Unmarshal(data, &document); err != nil {
		return fmt.Errorf("parse config file %s: %w", path, err)
	}

	flat := map[string]any{}
	flatten("", document, flat)

	// Sorted so that an invalid file reports the same first error every run, which makes the
	// failure reproducible rather than depending on map iteration order.
	keys := make([]string, 0, len(flat))
	for key := range flat {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	for _, key := range keys {
		f, known := r.byName[key]
		if !known {
			// Strict decoding. A typo in a configuration file otherwise fails silently, and the
			// operator concludes the setting does not work.
			return fmt.Errorf("config file %s: unknown setting %q", path, key)
		}
		if f.noYAML {
			return fmt.Errorf("config file %s: %q can only be set on the command line", path, key)
		}
		if f.setByFlag {
			continue
		}
		if err := f.setFromYAML(flat[key]); err != nil {
			return fmt.Errorf("config file %s: %q: %w", path, key, err)
		}
		f.source = SourceYAML
	}
	return nil
}

// flatten turns nested YAML into dotted keys matching flag names, so one registry serves both.
func flatten(prefix string, node map[string]any, out map[string]any) {
	for key, value := range node {
		full := key
		if prefix != "" {
			full = prefix + "." + key
		}
		if nested, ok := value.(map[string]any); ok {
			flatten(full, nested, out)
			continue
		}
		out[full] = value
	}
}

func (f *field) setFromYAML(value any) error {
	if list, ok := value.([]any); ok {
		if f.kind != kindStrings {
			return fmt.Errorf("expected a single value, got a list")
		}
		target := f.ptr.(*[]string)
		*target = nil
		for _, item := range list {
			*target = append(*target, fmt.Sprint(item))
		}
		return nil
	}
	if value == nil {
		return fmt.Errorf("value is empty")
	}
	return f.set(fmt.Sprint(value))
}

// set parses a string into the field, which is how environment variables and scalar YAML values
// both reach the same validation.
func (f *field) set(raw string) error {
	raw = strings.TrimSpace(raw)
	switch f.kind {
	case kindString:
		*f.ptr.(*string) = raw
	case kindStrings:
		parts := strings.Split(raw, ",")
		values := make([]string, 0, len(parts))
		for _, part := range parts {
			if trimmed := strings.TrimSpace(part); trimmed != "" {
				values = append(values, trimmed)
			}
		}
		*f.ptr.(*[]string) = values
	case kindInt:
		parsed, err := strconv.Atoi(raw)
		if err != nil {
			return fmt.Errorf("invalid integer %q", raw)
		}
		*f.ptr.(*int) = parsed
	case kindFloat:
		parsed, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			return fmt.Errorf("invalid number %q", raw)
		}
		*f.ptr.(*float64) = parsed
	case kindBool:
		parsed, err := strconv.ParseBool(raw)
		if err != nil {
			return fmt.Errorf("invalid boolean %q", raw)
		}
		*f.ptr.(*bool) = parsed
	case kindDuration:
		parsed, err := time.ParseDuration(raw)
		if err != nil {
			return fmt.Errorf("invalid duration %q", raw)
		}
		*f.ptr.(*time.Duration) = parsed
	case kindTristate:
		return f.ptr.(*Tristate).Set(raw)
	}
	return nil
}

// value renders the current value for the audit log.
func (f *field) value() string {
	switch f.kind {
	case kindString:
		return *f.ptr.(*string)
	case kindStrings:
		return strings.Join(*f.ptr.(*[]string), ",")
	case kindInt:
		return strconv.Itoa(*f.ptr.(*int))
	case kindFloat:
		return strconv.FormatFloat(*f.ptr.(*float64), 'g', -1, 64)
	case kindBool:
		return strconv.FormatBool(*f.ptr.(*bool))
	case kindDuration:
		return f.ptr.(*time.Duration).String()
	case kindTristate:
		return f.ptr.(*Tristate).String()
	}
	return ""
}

// Name and Source expose field metadata for auditing and tests.
func (f *field) Name() string   { return f.name }
func (f *field) Source() Source { return f.source }
func (f *field) Value() string  { return f.value() }
func (f *field) Pointer() any   { return f.ptr }
