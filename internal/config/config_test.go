package config

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/alecthomas/kingpin/v2"

	"github.com/local/directory-size-exporter/internal/fsstat"
)

// parse builds a registry, parses the given arguments, and resolves every source.
func parse(t *testing.T, args []string) (*Registry, error) {
	t.Helper()
	registry := NewRegistry()
	app := kingpin.New("test", "test").Terminate(nil)
	app.Writer(io.Discard)
	registry.RegisterFlags(app)
	if _, err := app.Parse(registry.NormalizeArgs(args)); err != nil {
		return nil, err
	}
	return registry, registry.Resolve(registry.Config().ConfigFile)
}

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestEveryConfigFieldHasAFlag is the guard against the configuration surface drifting.
//
// A field added to Config without a registry entry becomes a setting nobody can reach: it is
// missing from --help, from the environment, and from YAML, while looking perfectly present in the
// source. Reflection catches that here rather than in production.
func TestEveryConfigFieldHasAFlag(t *testing.T) {
	registry := NewRegistry()
	cfg := registry.Config()

	registered := map[uintptr]string{}
	for _, f := range registry.Fields() {
		registered[reflect.ValueOf(f.Pointer()).Pointer()] = f.Name()
	}

	value := reflect.ValueOf(cfg).Elem()
	for i := 0; i < value.NumField(); i++ {
		name := value.Type().Field(i).Name
		address := value.Field(i).Addr().Pointer()
		if _, ok := registered[address]; !ok {
			t.Errorf("Config.%s has no registered flag; it would be unreachable from --help, the environment and YAML", name)
		}
	}
	if got, want := len(registered), value.NumField(); got != want {
		t.Errorf("registry covers %d fields but Config has %d", got, want)
	}
}

func TestDefaultsAreApplied(t *testing.T) {
	registry, err := parse(t, []string{"--path.target=/data/logs"})
	if err != nil {
		t.Fatal(err)
	}
	cfg := registry.Config()

	if cfg.ScanInterval != 5*time.Minute {
		t.Errorf("scan.interval = %s, want 5m", cfg.ScanInterval)
	}
	// Gentle by design: the exporter shares a host with a latency-sensitive application.
	if cfg.ScanConcurrency != 2 {
		t.Errorf("scan.concurrency = %d, want 2", cfg.ScanConcurrency)
	}
	if cfg.ScanNice != 19 || cfg.ScanIOPriority != "idle" {
		t.Errorf("scheduling defaults = nice %d / %s, want 19 / idle", cfg.ScanNice, cfg.ScanIOPriority)
	}
	// A partial reading is systematically low and reads as freed space.
	if cfg.ScanPublishPartial {
		t.Error("scan.publish-partial defaults to true")
	}
	if cfg.WebEnableLifecycle {
		t.Error("web.enable-lifecycle defaults to true; an unauthenticated reload endpoint must be opt-in")
	}
	// The histogram costs a fixed ~90 series regardless of target count, which on a small
	// deployment exceeds everything else the exporter emits put together.
	if cfg.CollectorScanHistogram {
		t.Error("collector.scan-histogram defaults to true; it should be opt-in on cardinality grounds")
	}
}

func TestPrecedence_FlagBeatsEnvBeatsYAMLBeatsDefault(t *testing.T) {
	configPath := writeConfig(t, `
targets:
  - /data/logs
scan:
  interval: 11m
  concurrency: 3
  batch-size: 64
collector:
  file-counts: true
`)
	t.Setenv(EnvName("scan.interval"), "22m")
	t.Setenv(EnvName("scan.concurrency"), "4")

	registry, err := parse(t, []string{
		"--config.file=" + configPath,
		"--scan.interval=33m",
	})
	if err != nil {
		t.Fatal(err)
	}
	cfg := registry.Config()

	for _, testCase := range []struct {
		name   string
		got    any
		want   any
		source Source
	}{
		{"scan.interval", cfg.ScanInterval, 33 * time.Minute, SourceFlag},
		{"scan.concurrency", cfg.ScanConcurrency, 4, SourceEnv},
		{"scan.batch-size", cfg.ScanBatchSize, 64, SourceYAML},
		{"collector.file-counts", cfg.CollectorFileCounts, true, SourceYAML},
		{"scan.queue-limit", cfg.ScanQueueLimit, 65536, SourceDefault},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if testCase.got != testCase.want {
				t.Errorf("value = %v, want %v", testCase.got, testCase.want)
			}
			if source := sourceOf(t, registry, testCase.name); source != testCase.source {
				t.Errorf("source = %s, want %s", source, testCase.source)
			}
		})
	}
}

func sourceOf(t *testing.T, registry *Registry, name string) Source {
	t.Helper()
	for _, f := range registry.Fields() {
		if f.Name() == name {
			return f.Source()
		}
	}
	t.Fatalf("no field named %s", name)
	return ""
}

// TestBooleanFlagsAcceptBothSpellings covers a kingpin behaviour that is easy to trip over:
// a boolean flag is valueless, so --flag=true leaves "true" behind as a stray positional argument
// and parsing fails with "unexpected true". Both spellings are supported here because --flag=value
// is what most operators reach for.
func TestBooleanFlagsAcceptBothSpellings(t *testing.T) {
	for _, testCase := range []struct {
		args []string
		want bool
	}{
		{[]string{"--collector.file-counts=true"}, true},
		{[]string{"--collector.file-counts"}, true},
		{[]string{"--collector.file-counts=false"}, false},
		{[]string{"--no-collector.file-counts"}, false},
		{[]string{"--collector.file-counts=on"}, true},
		{[]string{"--collector.file-counts=0"}, false},
	} {
		t.Run(strings.Join(testCase.args, " "), func(t *testing.T) {
			registry, err := parse(t, append([]string{"--path.target=/x"}, testCase.args...))
			if err != nil {
				t.Fatalf("parsing %v failed: %v", testCase.args, err)
			}
			if got := registry.Config().CollectorFileCounts; got != testCase.want {
				t.Errorf("value = %v, want %v", got, testCase.want)
			}
		})
	}
}

func TestNormalizeArgs_LeavesNonBooleanFlagsAlone(t *testing.T) {
	registry := NewRegistry()
	args := []string{
		"--scan.interval=5m",             // duration, must survive verbatim
		"--collector.disk-usage=true",    // tristate, kingpin handles the value itself
		"--path.target=/data/logs=weird", // value containing an equals sign
		"--collector.file-counts=maybe",  // not boolean-looking, kingpin should report it
		"positional",
	}
	got := registry.NormalizeArgs(args)
	for i := range args {
		if got[i] != args[i] {
			t.Errorf("arg %d rewritten to %q, want %q", i, got[i], args[i])
		}
	}
}

// TestListenerSettingsAreFullyConfigurable covers the listener flags, which exporter-toolkit's own
// helper declares without environment support. Leaving them to it would make the listen address —
// among the first things anyone configures — the one setting unreachable from the environment or a
// config file, silently contradicting every other setting.
func TestListenerSettingsAreFullyConfigurable(t *testing.T) {
	t.Run("default", func(t *testing.T) {
		registry, err := parse(t, []string{"--path.target=/x"})
		if err != nil {
			t.Fatal(err)
		}
		if got := registry.Config().WebListenAddresses; len(got) != 1 || got[0] != ":9115" {
			t.Errorf("listen addresses = %v, want [:9115]", got)
		}
	})

	t.Run("flag, repeatable", func(t *testing.T) {
		registry, err := parse(t, []string{
			"--path.target=/x",
			"--web.listen-address=127.0.0.1:9115",
			"--web.listen-address=[::1]:9115",
		})
		if err != nil {
			t.Fatal(err)
		}
		got := registry.Config().WebListenAddresses
		if len(got) != 2 || got[0] != "127.0.0.1:9115" || got[1] != "[::1]:9115" {
			t.Errorf("listen addresses = %v, want both entries", got)
		}
	})

	t.Run("environment", func(t *testing.T) {
		t.Setenv(EnvName("web.listen-address"), "0.0.0.0:9999")
		registry, err := parse(t, []string{"--path.target=/x"})
		if err != nil {
			t.Fatal(err)
		}
		if got := registry.Config().WebListenAddresses; len(got) != 1 || got[0] != "0.0.0.0:9999" {
			t.Errorf("listen addresses = %v, want [0.0.0.0:9999]", got)
		}
	})

	t.Run("yaml", func(t *testing.T) {
		configPath := writeConfig(t, "targets:\n  - /x\nweb:\n  listen-address:\n    - 10.0.0.1:9200\n  config.file: /etc/web.yml\n")
		registry, err := parse(t, []string{"--config.file=" + configPath})
		if err != nil {
			t.Fatal(err)
		}
		cfg := registry.Config()
		if got := cfg.WebListenAddresses; len(got) != 1 || got[0] != "10.0.0.1:9200" {
			t.Errorf("listen addresses = %v, want [10.0.0.1:9200]", got)
		}
		if cfg.WebConfigFile != "/etc/web.yml" {
			t.Errorf("web config file = %q, want /etc/web.yml", cfg.WebConfigFile)
		}
	})

	t.Run("exposed to the toolkit after validation", func(t *testing.T) {
		resolved, err := validate(t, []string{"--path.target=/x", "--web.listen-address=:9300"}, allCaps)
		if err != nil {
			t.Fatal(err)
		}
		if resolved.Web == nil || len(*resolved.Web.WebListenAddresses) != 1 {
			t.Fatalf("toolkit config not populated: %+v", resolved.Web)
		}
		if (*resolved.Web.WebListenAddresses)[0] != ":9300" {
			t.Errorf("toolkit listen address = %v, want :9300", *resolved.Web.WebListenAddresses)
		}
	})
}

func TestValidate_SystemdSocketIsLinuxOnly(t *testing.T) {
	_, err := validate(t, []string{"--path.target=/x", "--web.systemd-socket=true"}, allCaps)
	if runtime.GOOS == "linux" {
		if err != nil {
			t.Fatalf("systemd socket rejected on linux: %v", err)
		}
		return
	}
	// Failing at startup beats an opaque socket error after startup appears to have succeeded.
	if err == nil || !strings.Contains(err.Error(), "systemd-socket") {
		t.Fatalf("error = %v, want a refusal naming the flag", err)
	}
}

func TestEnvNameDerivation(t *testing.T) {
	for flagName, want := range map[string]string{
		"scan.interval":           "DIR_EXPORTER_SCAN_INTERVAL",
		"collector.file-counts":   "DIR_EXPORTER_COLLECTOR_FILE_COUNTS",
		"targets.retain-vanished": "DIR_EXPORTER_TARGETS_RETAIN_VANISHED",
		"web.telemetry-path":      "DIR_EXPORTER_WEB_TELEMETRY_PATH",
	} {
		if got := EnvName(flagName); got != want {
			t.Errorf("EnvName(%q) = %q, want %q", flagName, got, want)
		}
	}
}

func TestYAML_TargetsAcceptListAndAlias(t *testing.T) {
	configPath := writeConfig(t, "targets:\n  - /data/logs/a\n  - /data/logs/b\n")
	registry, err := parse(t, []string{"--config.file=" + configPath})
	if err != nil {
		t.Fatal(err)
	}
	got := registry.Config().TargetPatterns
	if len(got) != 2 || got[0] != "/data/logs/a" || got[1] != "/data/logs/b" {
		t.Fatalf("targets = %v, want both entries", got)
	}
}

// TestYAML_UnknownSettingIsRejected matters because a typo that is silently ignored leads the
// operator to conclude the setting does not work, which is a much longer debugging session than a
// startup error.
func TestYAML_UnknownSettingIsRejected(t *testing.T) {
	configPath := writeConfig(t, "scan:\n  intervall: 5m\n")
	_, err := parse(t, []string{"--path.target=/data/logs", "--config.file=" + configPath})
	if err == nil {
		t.Fatal("an unknown YAML setting was accepted")
	}
	if !strings.Contains(err.Error(), "unknown setting") || !strings.Contains(err.Error(), "scan.intervall") {
		t.Errorf("error = %v, want one naming the unknown key", err)
	}
}

func TestYAML_InvalidValueNamesTheSetting(t *testing.T) {
	configPath := writeConfig(t, "scan:\n  interval: not-a-duration\n")
	_, err := parse(t, []string{"--path.target=/data/logs", "--config.file=" + configPath})
	if err == nil {
		t.Fatal("an invalid duration was accepted")
	}
	if !strings.Contains(err.Error(), "scan.interval") {
		t.Errorf("error = %v, want one naming the setting", err)
	}
}

func TestYAML_ConfigFileCannotBeSetFromYAML(t *testing.T) {
	configPath := writeConfig(t, "config:\n  file: /somewhere/else.yml\n")
	_, err := parse(t, []string{"--path.target=/data/logs", "--config.file=" + configPath})
	if err == nil || !strings.Contains(err.Error(), "command line") {
		t.Fatalf("error = %v, want a refusal to set config.file from YAML", err)
	}
}

// TestEnv_ConfigFileIsHonoured covers a setting that has to be resolved before every other one,
// because it names the file the others come from. Reading it in the normal environment pass would
// happen after the decision to load a file, so the variable would be accepted and then silently
// ignored — leaving an exporter that starts up looking healthy with none of its config applied.
func TestEnv_ConfigFileIsHonoured(t *testing.T) {
	configPath := writeConfig(t, "targets:\n  - /data/logs\nscan:\n  batch-size: 77\n")
	t.Setenv(EnvName("config.file"), configPath)

	registry, err := parse(t, []string{})
	if err != nil {
		t.Fatal(err)
	}
	if got := registry.Config().ScanBatchSize; got != 77 {
		t.Fatalf("scan.batch-size = %d, want 77; the config file named by the environment was not loaded", got)
	}
	if got := registry.Config().TargetPatterns; len(got) != 1 || got[0] != "/data/logs" {
		t.Errorf("targets = %v, want the values from the config file", got)
	}
	if source := sourceOf(t, registry, "config.file"); source != SourceEnv {
		t.Errorf("config.file source = %s, want %s", source, SourceEnv)
	}
}

func TestEnv_ConfigFileFlagStillWins(t *testing.T) {
	fromFlag := writeConfig(t, "scan:\n  batch-size: 11\n")
	fromEnv := writeConfig(t, "scan:\n  batch-size: 22\n")
	t.Setenv(EnvName("config.file"), fromEnv)

	registry, err := parse(t, []string{"--path.target=/x", "--config.file=" + fromFlag})
	if err != nil {
		t.Fatal(err)
	}
	if got := registry.Config().ScanBatchSize; got != 11 {
		t.Errorf("scan.batch-size = %d, want 11 from the flag-named file", got)
	}
}

func TestEnv_MissingConfigFileFromEnvIsAnError(t *testing.T) {
	// Silently continuing would start an exporter with none of its intended configuration.
	t.Setenv(EnvName("config.file"), filepath.Join(t.TempDir(), "absent.yml"))
	if _, err := parse(t, []string{"--path.target=/x"}); err == nil {
		t.Fatal("a missing config file named by the environment was ignored")
	}
}

func TestEnv_InvalidValueIsRejected(t *testing.T) {
	t.Setenv(EnvName("scan.concurrency"), "many")
	_, err := parse(t, []string{"--path.target=/data/logs"})
	if err == nil || !strings.Contains(err.Error(), "DIR_EXPORTER_SCAN_CONCURRENCY") {
		t.Fatalf("error = %v, want one naming the environment variable", err)
	}
}

func TestEnv_TargetsAcceptCommaSeparatedList(t *testing.T) {
	t.Setenv(EnvName("path.target"), "/data/a, /data/b")
	registry, err := parse(t, []string{})
	if err != nil {
		t.Fatal(err)
	}
	got := registry.Config().TargetPatterns
	if len(got) != 2 || got[0] != "/data/a" || got[1] != "/data/b" {
		t.Fatalf("targets = %v, want two trimmed entries", got)
	}
}

// --- validation and capability gating ---------------------------------------------------------

func validate(t *testing.T, args []string, caps fsstat.Capability) (*Resolved, error) {
	t.Helper()
	registry, err := parse(t, args)
	if err != nil {
		return nil, err
	}
	return registry.Validate(caps, slog.New(slog.DiscardHandler))
}

const allCaps = fsstat.CapAllocBytes | fsstat.CapInode | fsstat.CapFSBytes | fsstat.CapFSInodes | fsstat.CapMountpoint

// TestTristate_AutoDisablesQuietlyAndTrueFailsLoudly is the whole reason the tristate exists. The
// injectable capability set means one machine covers both platform policies.
func TestTristate_AutoDisablesQuietlyAndTrueFailsLoudly(t *testing.T) {
	base := []string{"--path.target=/data/logs"}

	t.Run("auto with capability enables", func(t *testing.T) {
		resolved, err := validate(t, base, allCaps)
		if err != nil {
			t.Fatal(err)
		}
		if !resolved.DiskUsage {
			t.Error("disk usage was not enabled despite the capability being present")
		}
	})

	t.Run("auto without capability disables and explains", func(t *testing.T) {
		resolved, err := validate(t, base, fsstat.CapFSBytes)
		if err != nil {
			t.Fatalf("auto should degrade, not fail: %v", err)
		}
		if resolved.DiskUsage {
			t.Error("disk usage was enabled without the capability")
		}
		if _, explained := resolved.Disabled["collector.disk-usage"]; !explained {
			t.Error("the disabled feature was not recorded, so a missing metric is unexplainable")
		}
	})

	t.Run("true without capability fails startup", func(t *testing.T) {
		_, err := validate(t, append(base, "--collector.disk-usage=true"), fsstat.CapFSBytes)
		if err == nil {
			t.Fatal("an impossible requirement was accepted")
		}
		if !strings.Contains(err.Error(), "alloc_bytes") {
			t.Errorf("error = %v, want one naming the missing capability", err)
		}
	})

	t.Run("false without capability is fine", func(t *testing.T) {
		resolved, err := validate(t, append(base, "--collector.disk-usage=false"), 0)
		if err != nil {
			t.Fatal(err)
		}
		if resolved.DiskUsage {
			t.Error("disk usage was enabled despite being explicitly disabled")
		}
	})
}

func TestValidate_RejectsBadValues(t *testing.T) {
	for _, testCase := range []struct {
		name string
		args []string
		want string
	}{
		{"no targets", []string{}, "path.target"},
		{"zero interval", []string{"--path.target=/x", "--scan.interval=0"}, "scan.interval"},
		{"negative rate limit", []string{"--path.target=/x", "--scan.rate-limit=-1"}, "rate-limit"},
		{"zero concurrency", []string{"--path.target=/x", "--scan.concurrency=0"}, "concurrency"},
		{"relative telemetry path", []string{"--path.target=/x", "--web.telemetry-path=metrics"}, "telemetry-path"},
		{"bad io priority", []string{"--path.target=/x", "--scan.io-priority=fastest"}, "io-priority"},
		{"nice out of range", []string{"--path.target=/x", "--scan.nice=42"}, "nice"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := validate(t, testCase.args, allCaps)
			if err == nil {
				t.Fatal("invalid configuration was accepted")
			}
			if !strings.Contains(err.Error(), testCase.want) {
				t.Errorf("error = %v, want one mentioning %q", err, testCase.want)
			}
		})
	}
}

// TestValidate_HardTimeoutMustExceedScanTimeout guards a setting that would otherwise abandon
// targets that are merely slow, discarding work about to finish.
func TestValidate_HardTimeoutMustExceedScanTimeout(t *testing.T) {
	_, err := validate(t, []string{
		"--path.target=/x", "--scan.timeout=10m", "--scan.hard-timeout=5m",
	}, allCaps)
	if err == nil || !strings.Contains(err.Error(), "hard-timeout") {
		t.Fatalf("error = %v, want a refusal", err)
	}

	registry, err := parse(t, []string{"--path.target=/x", "--scan.timeout=10m"})
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := registry.Validate(allCaps, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	// Derived rather than left at zero, so a stuck target is always eventually released.
	if resolved.ScanHardTimeout != 20*time.Minute {
		t.Errorf("derived hard timeout = %s, want 20m", resolved.ScanHardTimeout)
	}
	// Reported as derived, not as a default: an operator tracing why a target was abandoned would
	// otherwise go looking through --help for a 20m default that does not exist.
	if source := sourceOf(t, registry, "scan.hard-timeout"); source != SourceDerived {
		t.Errorf("scan.hard-timeout source = %s, want %s", source, SourceDerived)
	}
}

// TestValidate_HardTimeoutAlwaysHasAValue is the guard for the failure mode the hard timeout
// exists to prevent.
//
// It is the only thing that reclaims a scan whose worker is stuck in an uninterruptible filesystem
// call, and a stuck scan blocks the cycle permanently rather than merely failing — no further scan
// ever runs, and the abandoned-worker gauge never moves because it is only written once a cycle
// completes. Deriving it solely from scan.timeout left the DEFAULT configuration, where
// scan.timeout is disabled, with no protection at all.
func TestValidate_HardTimeoutAlwaysHasAValue(t *testing.T) {
	for _, testCase := range []struct {
		name string
		args []string
		want time.Duration
	}{
		{"defaults, no scan timeout", nil, defaultHardTimeoutBackstop},
		{"derived from scan timeout", []string{"--scan.timeout=10m"}, 20 * time.Minute},
		{"explicit wins", []string{"--scan.timeout=10m", "--scan.hard-timeout=45m"}, 45 * time.Minute},
		{"explicit without a scan timeout", []string{"--scan.hard-timeout=2h"}, 2 * time.Hour},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			resolved, err := validate(t, append([]string{"--path.target=/x"}, testCase.args...), allCaps)
			if err != nil {
				t.Fatal(err)
			}
			if resolved.ScanHardTimeout != testCase.want {
				t.Errorf("hard timeout = %s, want %s", resolved.ScanHardTimeout, testCase.want)
			}
			// Whatever the route, it must never end up unbounded.
			if resolved.ScanHardTimeout <= 0 {
				t.Error("hard timeout is disabled; a stuck worker would stall scanning forever")
			}
		})
	}
}

// TestValidate_StaleAfterMustExceedScanInterval catches a setting that makes the size series flap
// in and out on every cycle, which reads as an exporter fault rather than as the staleness signal
// it is meant to be.
func TestValidate_StaleAfterMustExceedScanInterval(t *testing.T) {
	for _, testCase := range []struct {
		name    string
		args    []string
		wantErr bool
	}{
		{"below the interval", []string{"--scan.interval=5m", "--scan.stale-after=2m"}, true},
		{"equal to the interval", []string{"--scan.interval=5m", "--scan.stale-after=5m"}, true},
		{"above the interval", []string{"--scan.interval=5m", "--scan.stale-after=20m"}, false},
		{"disabled", []string{"--scan.interval=5m", "--scan.stale-after=0"}, false},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := validate(t, append([]string{"--path.target=/x"}, testCase.args...), allCaps)
			if testCase.wantErr && err == nil {
				t.Fatal("a stale-after that flaps the series was accepted")
			}
			if !testCase.wantErr && err != nil {
				t.Fatalf("valid configuration rejected: %v", err)
			}
		})
	}
}

func TestAudit_LogsEverySettingWithItsSource(t *testing.T) {
	configPath := writeConfig(t, "targets:\n  - /data/logs\nscan:\n  batch-size: 64\n")
	t.Setenv(EnvName("scan.concurrency"), "4")
	registry, err := parse(t, []string{"--config.file=" + configPath, "--scan.interval=33m"})
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := registry.Validate(allCaps, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}

	handler := &captureHandler{}
	registry.LogAudit(slog.New(handler), resolved)

	sources := map[string]string{}
	for _, record := range handler.records {
		if record.Message != "Setting resolved" {
			continue
		}
		var name, origin string
		record.Attrs(func(attr slog.Attr) bool {
			switch attr.Key {
			case "name":
				name = attr.Value.String()
			case "origin":
				origin = attr.Value.String()
			}
			return true
		})
		if origin == "" {
			// promslog renames attributes that collide with its own reserved keys, so an audit key
			// that survives a plain handler can still be mangled in the real binary.
			t.Errorf("setting %s logged no origin; check the attribute key does not collide", name)
		}
		sources[name] = origin
	}

	// Every setting must be present, so the audit answers "why is it behaving this way" without
	// anyone reading the source or guessing which of three mechanisms won.
	if len(sources) != len(registry.Fields()) {
		t.Errorf("audit logged %d settings, want %d", len(sources), len(registry.Fields()))
	}
	for name, want := range map[string]string{
		"scan.interval":    string(SourceFlag),
		"scan.concurrency": string(SourceEnv),
		"scan.batch-size":  string(SourceYAML),
		"scan.queue-limit": string(SourceDefault),
	} {
		if sources[name] != want {
			t.Errorf("audit source for %s = %s, want %s", name, sources[name], want)
		}
	}
}

type captureHandler struct{ records []slog.Record }

func (h *captureHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *captureHandler) WithAttrs([]slog.Attr) slog.Handler       { return h }
func (h *captureHandler) WithGroup(string) slog.Handler            { return h }

func (h *captureHandler) Handle(_ context.Context, record slog.Record) error {
	h.records = append(h.records, record.Clone())
	return nil
}
