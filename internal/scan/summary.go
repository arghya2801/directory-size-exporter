package scan

import (
	"log/slog"
	"time"

	"github.com/local/directory-size-exporter/internal/state"
)

// logSummary emits exactly one line per target per scan.
//
// This is the only place scan errors are logged, and they appear as per-class COUNTS. Logging a
// line per failing entry would turn a single wrong mode bit on a ten-million-file tree into
// millions of log lines — enough to fill a disk on the very host whose disk usage is being
// monitored, and enough to bury every other message on the box. The per-class counters carry the
// same information in bounded space, and the metrics carry it in queryable form.
func (e *Engine) logSummary(target *targetScan, outcome state.Outcome) {
	errorCounts, errorTotal := target.errorSnapshot()

	attrs := []any{
		"target", target.path,
		"outcome", string(outcome),
		"duration_seconds", time.Since(target.startedAt).Round(time.Millisecond).Seconds(),
		"bytes", target.size.Load(),
		"files", target.files.Load(),
		"directories", target.dirs.Load(),
		"entries_scanned", target.entries.Load(),
		"stat_calls", target.statCalls.Load(),
		"errors", errorTotal,
	}
	if vanished := target.vanished.Load(); vanished > 0 {
		// Not an error: on a host doing continuous log rotation, entries disappearing between
		// being listed and being stat'ed is expected.
		attrs = append(attrs, "vanished_entries", vanished)
	}
	if skipped := target.skippedXdev.Load(); skipped > 0 {
		attrs = append(attrs, "skipped_other_filesystem", skipped)
	}
	if hardlinked := target.hardlinked.Load(); hardlinked > 0 {
		attrs = append(attrs, "hardlinked_files", hardlinked, "dedup_saved_bytes", target.dedupSaved.Load())
	}
	if target.inodesExhausted.Load() {
		attrs = append(attrs, "hardlink_tracking_exhausted", true)
	}
	for _, class := range AllErrorClasses {
		if count := errorCounts[class]; count > 0 {
			attrs = append(attrs, "errors_"+class, count)
		}
	}

	// A published measurement and a withheld one are different operational events, so they are
	// logged at different levels rather than requiring the reader to parse the outcome field.
	switch outcome {
	case state.OutcomeComplete:
		e.logger.Info("Directory scan completed", attrs...)
	case state.OutcomeCancelled:
		e.logger.Info("Directory scan cancelled by shutdown", attrs...)
	case state.OutcomeMissing:
		e.logger.Warn("Directory scan found no target; retaining the previous measurement", attrs...)
	default:
		e.logger.Warn("Directory scan did not complete; previous measurement retained", attrs...)
	}
}

// SummaryAttrs is exposed for tests that need to assert on the exact field set of a summary line
// without depending on slog's internal representation.
func SummaryAttrs(record slog.Record) map[string]any {
	attrs := make(map[string]any, record.NumAttrs())
	record.Attrs(func(attr slog.Attr) bool {
		attrs[attr.Key] = attr.Value.Any()
		return true
	})
	return attrs
}
