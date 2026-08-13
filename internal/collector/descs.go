package collector

import "github.com/prometheus/client_golang/prometheus"

// Namespace prefixes every metric this exporter publishes.
const Namespace = "dir_exporter"

var (
	targetLabel = []string{"target_path"}
	fsLabels    = []string{"mountpoint", "device", "fstype"}
)

// descs holds every metric descriptor in one place, so the published surface can be reviewed
// without reading the collection logic.
type descs struct {
	// Measurement. All of these are absent until a scan completes without errors.
	size       *prometheus.Desc
	diskUsage  *prometheus.Desc
	files      *prometheus.Desc
	dirs       *prometheus.Desc
	hardlinked *prometheus.Desc
	dedupSaved *prometheus.Desc
	dedupBroke *prometheus.Desc

	// Growth.
	delta         *prometheus.Desc
	deltaInterval *prometheus.Desc
	bytesAdded    *prometheus.Desc
	bytesRemoved  *prometheus.Desc

	// Status and freshness.
	success       *prometheus.Desc
	partial       *prometheus.Desc
	present       *prometheus.Desc
	lastGoodTime  *prometheus.Desc
	lastAttempt   *prometheus.Desc
	scanAge       *prometheus.Desc
	inProgress    *prometheus.Desc
	currentScan   *prometheus.Desc
	lastDuration  *prometheus.Desc
	scansTotal    *prometheus.Desc
	lastErrors    *prometheus.Desc
	errorsTotal   *prometheus.Desc
	vanishedTotal *prometheus.Desc

	// Scan cost.
	entriesTotal   *prometheus.Desc
	statCallsTotal *prometheus.Desc
	dirsReadTotal  *prometheus.Desc
	skippedXdev    *prometheus.Desc

	// Engine-wide.
	scansSkipped     *prometheus.Desc
	abandonedWorkers *prometheus.Desc
	rateLimitWait    *prometheus.Desc
	capability       *prometheus.Desc
	targetsResolved  *prometheus.Desc

	// Filesystem context.
	fsSize         *prometheus.Desc
	fsFree         *prometheus.Desc
	fsAvail        *prometheus.Desc
	fsFiles        *prometheus.Desc
	fsFilesFree    *prometheus.Desc
	mountpointInfo *prometheus.Desc
	statfsTimeouts *prometheus.Desc
	fsUnavailable  *prometheus.Desc

	// all is every descriptor above, for Describe. A collector that describes only part of what it
	// emits is registered unchecked, which silently forfeits the registry's duplicate-series and
	// label-consistency checks — exactly the mistakes that are easiest to make by hand.
	all []*prometheus.Desc
}

func newDescs() *descs {
	var all []*prometheus.Desc
	reg := func(name, help string, labels []string) *prometheus.Desc {
		pd := prometheus.NewDesc(Namespace+"_"+name, help, labels, nil)
		all = append(all, pd)
		return pd
	}

	d := &descs{
		size: reg("size_bytes",
			"Logical size of regular files in the directory, in bytes. Absent until a scan completes without errors.",
			targetLabel),
		diskUsage: reg("disk_usage_bytes",
			"Space allocated on disk for regular files in the directory, in bytes. Differs from size_bytes for sparse and compressed files.",
			targetLabel),
		files: reg("files",
			"Number of regular files in the directory.", targetLabel),
		dirs: reg("directories",
			"Number of subdirectories in the directory.", targetLabel),
		hardlinked: reg("hardlinked_files",
			"Number of files in the directory with more than one hard link.", targetLabel),
		dedupSaved: reg("hardlink_dedup_saved_bytes",
			"Bytes excluded from size_bytes because the file was already counted through another hard link within this target.",
			targetLabel),
		dedupBroke: reg("hardlink_tracking_exhausted",
			"Whether hard-link tracking hit its memory cap during the last scan, after which duplicates are counted again (1 for yes, 0 for no).",
			targetLabel),

		delta: reg("size_delta_bytes",
			"Signed change in size_bytes since the previous completed scan. Absent until two scans have completed.",
			targetLabel),
		deltaInterval: reg("size_delta_interval_seconds",
			"Seconds spanned by size_delta_bytes. Divide the delta by this, not by the scan interval, because failed scans are skipped.",
			targetLabel),
		bytesAdded: reg("bytes_added_total",
			"Cumulative bytes added to the directory, measured across completed scans.", targetLabel),
		bytesRemoved: reg("bytes_removed_total",
			"Cumulative bytes removed from the directory, measured across completed scans.", targetLabel),

		success: reg("last_scan_success",
			"Whether the most recent scan completed with no errors (1 for success, 0 for failure). Present from process start.",
			targetLabel),
		partial: reg("last_scan_partial",
			"Whether the most recent scan is known to be incomplete (1 for yes, 0 for no).", targetLabel),
		present: reg("target_present",
			"Whether the target existed at the most recent scan attempt (1 for yes, 0 for no).", targetLabel),
		lastGoodTime: reg("last_good_scan_timestamp_seconds",
			"Unix timestamp of the most recent scan that completed without errors.", targetLabel),
		lastAttempt: reg("last_scan_attempt_timestamp_seconds",
			"Unix timestamp of the most recent scan attempt, whatever its outcome.", targetLabel),
		scanAge: reg("scan_age_seconds",
			"Seconds since the most recent scan that completed without errors. Grows without bound while scans keep failing.",
			targetLabel),
		inProgress: reg("scan_in_progress",
			"Whether a scan of this target is running now (1 for yes, 0 for no).", targetLabel),
		currentScan: reg("current_scan_duration_seconds",
			"Seconds the in-progress scan of this target has been running, or 0 when idle.", targetLabel),
		lastDuration: reg("last_scan_duration_seconds",
			"Duration of the most recent scan attempt of this target.", targetLabel),
		scansTotal: reg("scans_total",
			"Total scans by outcome.", []string{"target_path", "result"}),
		lastErrors: reg("last_scan_errors",
			"Errors in the most recent scan, by class.", []string{"target_path", "class"}),
		errorsTotal: reg("scan_errors_total",
			"Cumulative scan errors by class.", []string{"target_path", "class"}),
		vanishedTotal: reg("scan_vanished_files_total",
			"Cumulative entries that disappeared between being listed and being stat'ed. Routine during log rotation; not an error.",
			targetLabel),

		entriesTotal: reg("scan_entries_total",
			"Cumulative directory entries examined.", targetLabel),
		statCallsTotal: reg("scan_stat_calls_total",
			"Cumulative stat calls issued.", targetLabel),
		dirsReadTotal: reg("scan_dirs_read_total",
			"Cumulative directories read.", targetLabel),
		skippedXdev: reg("scan_skipped_other_filesystem_total",
			"Cumulative directories skipped because they are on a different filesystem than the target root.",
			targetLabel),

		scansSkipped: reg("scan_skipped_total",
			"Total scan cycles skipped because a previous cycle was still running.", nil),
		abandonedWorkers: reg("abandoned_scan_workers",
			"Scan workers that did not exit, presumed stuck in an uninterruptible filesystem call. Sustained values at or above the concurrency limit mean the pool is dead.",
			nil),
		rateLimitWait: reg("scan_rate_limit_wait_seconds_total",
			"Cumulative time scan workers spent waiting for the filesystem rate limiter.", nil),
		capability: reg("platform_capability",
			"Whether a platform measurement capability is available (1 for yes, 0 for no). Metrics depending on an absent capability are not published at all.",
			[]string{"capability"}),
		targetsResolved: reg("targets_resolved",
			"Number of targets currently being monitored.", nil),

		fsSize: reg("filesystem_size_bytes",
			"Total size of the filesystem containing one or more targets, in bytes.", fsLabels),
		fsFree: reg("filesystem_free_bytes",
			"Free space on the filesystem, in bytes.", fsLabels),
		fsAvail: reg("filesystem_avail_bytes",
			"Space available to unprivileged users on the filesystem, in bytes.", fsLabels),
		fsFiles: reg("filesystem_files",
			"Total inodes on the filesystem.", fsLabels),
		fsFilesFree: reg("filesystem_files_free",
			"Free inodes on the filesystem.", fsLabels),
		mountpointInfo: reg("target_mountpoint_info",
			"Maps a target to the filesystem holding it. Join on mountpoint to express a directory as a share of its volume.",
			[]string{"target_path", "mountpoint", "device", "fstype"}),
		statfsTimeouts: reg("statfs_timeouts_total",
			"Cumulative filesystem capacity queries abandoned after timing out, which indicates a hung mount.",
			targetLabel),
		fsUnavailable: reg("filesystem_info_unavailable",
			"Whether filesystem capacity for this target could not be determined at the last refresh (1 for yes, 0 for no).",
			targetLabel),
	}
	d.all = all
	return d
}
