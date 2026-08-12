// Package config resolves the exporter's settings from flags, environment and a YAML file.
//
// Configurability is a first-class requirement here: an operator tuning a scan against a live
// trading host needs every knob reachable without a rebuild, and needs to be able to answer "why
// is it behaving this way" from the logs alone. Two things follow from that.
//
// First, every setting has a flag. A YAML-only or environment-only setting is invisible to
// --help, so the field registry is the single source of truth for flags, environment variables,
// YAML keys and validation at once, and a test asserts that no struct field escapes it.
//
// Second, every setting records where its value came from. Precedence is
// flag > environment > YAML > default, and the startup audit prints each value with its source.
package config

import (
	"fmt"
	"strings"
	"time"
)

// Source identifies where a setting's value came from.
type Source string

const (
	SourceDefault Source = "default"
	SourceYAML    Source = "yaml"
	SourceEnv     Source = "env"
	SourceFlag    Source = "flag"
	// SourceDerived marks a value computed from another setting during validation. Reporting these
	// as defaults would send an operator hunting through --help for a default that does not exist.
	SourceDerived Source = "derived"
)

// EnvPrefix prefixes every environment variable this exporter reads.
const EnvPrefix = "DIR_EXPORTER_"

// Tristate is a boolean that can also defer to what the platform supports.
//
// A plain boolean cannot express the difference between "I did not ask for this" and "I require
// this". That difference matters: on a platform that cannot read allocated blocks, a default-on
// boolean would silently publish nothing while an explicit request should refuse to start, so the
// operator learns at deploy time rather than from a missing panel weeks later.
type Tristate int

const (
	// TriAuto enables the feature when the platform supports it, and warns when it does not.
	TriAuto Tristate = iota
	// TriTrue requires the feature; startup fails if the platform cannot provide it.
	TriTrue
	// TriFalse disables the feature.
	TriFalse
)

func (t Tristate) String() string {
	switch t {
	case TriTrue:
		return "true"
	case TriFalse:
		return "false"
	default:
		return "auto"
	}
}

// Set implements kingpin.Value.
func (t *Tristate) Set(raw string) error {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "auto", "":
		*t = TriAuto
	case "true", "yes", "1", "on":
		*t = TriTrue
	case "false", "no", "0", "off":
		*t = TriFalse
	default:
		return fmt.Errorf("invalid value %q: want true, false or auto", raw)
	}
	return nil
}

// IsCumulative tells kingpin this is a single-valued flag.
func (t *Tristate) IsCumulative() bool { return false }

// Config is the complete set of settings. Every field must appear in the registry; a test enforces
// it by reflection so a field added here without a flag fails the build's test stage rather than
// becoming a setting nobody can reach.
type Config struct {
	// Targets.
	TargetPatterns         []string
	TargetsMax             int
	TargetsRetainVanished  time.Duration
	TargetsRefreshInterval time.Duration

	// Scan scheduling.
	ScanInterval           time.Duration
	ScanTimeout            time.Duration
	ScanHardTimeout        time.Duration
	ScanWorkerDrainTimeout time.Duration
	ScanStatfsTimeout      time.Duration

	// Scan engine.
	ScanConcurrency      int
	ScanBatchSize        int
	ScanShareThreshold   int
	ScanQueueLimit       int
	ScanRateLimit        float64
	ScanRateLimitBurst   int
	ScanMaxTrackedInodes int

	// Scan behaviour.
	ScanOneFilesystem  Tristate
	ScanDedupHardlinks Tristate
	ScanPublishPartial bool
	ScanStaleAfter     time.Duration

	// Host impact.
	ScanNice       int
	ScanIOPriority string

	// Collectors.
	CollectorFileCounts    bool
	CollectorDiskUsage     Tristate
	CollectorGrowth        bool
	CollectorFilesystem    Tristate
	CollectorSelf          bool
	CollectorScanHistogram bool

	// Web.
	WebTelemetryPath     string
	WebEnableLifecycle   bool
	WebReadyRequiresScan bool

	// Logging beyond level and format, which promslog owns.
	LogScanHeartbeat     time.Duration
	LogHeartbeatInterval time.Duration

	// ConfigFile is the YAML file to load. It is itself a flag, and cannot be set from YAML.
	ConfigFile string
}

// EnvName derives the environment variable for a flag name: scan.interval becomes
// DIR_EXPORTER_SCAN_INTERVAL.
func EnvName(flagName string) string {
	replaced := strings.NewReplacer(".", "_", "-", "_").Replace(flagName)
	return EnvPrefix + strings.ToUpper(replaced)
}
