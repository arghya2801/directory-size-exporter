# Directory Size Exporter

A Prometheus exporter that measures the size of specific directories.

`node_exporter` reports filesystem-level capacity, which answers "is this volume filling" but not
"what is filling it". This exporter answers the second question, and is built for hosts where the
directories in question hold millions of files and the machine is also running something
latency-sensitive.

## Design commitments

Three properties drive most of the design, and are worth knowing before tuning anything.

**A failed scan never publishes a number.** A partial walk, a timeout, or a directory that briefly
vanished all produce a systematically low result. Publishing one is indistinguishable from the
directory genuinely shrinking, so `dir_exporter_size_bytes` is replaced only by a scan that
completed with no errors. Everything else updates status metrics alone, and the previous
measurement keeps being served with `dir_exporter_scan_age_seconds` showing how stale it is.

**Nothing is published before there is something to publish.** Until the first scan completes, the
size series is absent rather than zero. An absent series is unambiguous in PromQL; a zero looks
exactly like a real measurement and fires capacity alerts on every restart.

**A measurement the platform cannot make is not approximated.** Where allocated-block sizes are
unavailable, `dir_exporter_disk_usage_bytes` is not published at all rather than being filled in
with logical size. `dir_exporter_platform_capability` reports what the running binary can do.

## Running

```
directory-size-exporter \
  --path.target=/var/log/app \
  --path.target='/data/logs/venue-*' \
  --scan.interval=5m \
  --scan.timeout=30m \
  --scan.rate-limit=20000 \
  --collector.file-counts=true \
  --web.listen-address=:9115
```

Glob patterns are expanded and re-expanded every `--targets.refresh-interval`, so directories
created after startup are picked up without a restart.

Configuration can come from flags, environment variables (`DIR_EXPORTER_SCAN_INTERVAL` and so on),
or a YAML file, with precedence **flag > environment > YAML > default**. Every setting is available
through all three, and the startup log prints each value with the source it came from.

```yaml
# --config.file=/etc/directory-size-exporter.yml
#
# Note the nesting under "path:" rather than a top-level "targets:" list: "targets:" is also
# the prefix for the targets.* settings, and YAML cannot have one key be both a list and a map.
path:
  target:
    - /var/log/app
    - /data/logs/venue-*
scan:
  interval: 5m
  timeout: 30m
  rate-limit: 20000
  dedup-hardlinks: true
collector:
  file-counts: true
web:
  listen-address:
    - :9115
```

The listen address, `--web.config.file` and `--web.systemd-socket` are declared by this exporter
rather than taken from exporter-toolkit's flag helper, which offers no environment support. That
would otherwise have left the listen address — among the first things anyone configures — as the
one setting unreachable from the environment or a config file.

Boolean flags accept both `--collector.file-counts` and `--collector.file-counts=true`, and both
`--no-collector.file-counts` and `--collector.file-counts=false`.

Capability-dependent settings are three-valued — `true`, `false` or `auto` (the default). `auto`
enables the feature where the platform supports it and logs why it did not otherwise; `true`
refuses to start if the platform cannot deliver it, so a mis-built binary is caught at deploy time
rather than by a panel that has been empty for a month.

### Endpoints

| Path | Purpose |
|---|---|
| `/metrics` | Metrics. Configurable with `--web.telemetry-path`. |
| `/` | Landing page. |
| `/-/healthy` | 200 while running; 503 once shutdown begins, so a load balancer can drain. |
| `/-/ready` | 503 until the first scan cycle finishes, unless `--web.ready-requires-scan=false`. |
| `/-/reload` | Re-resolves targets. POST only, and off unless `--web.enable-lifecycle` is set. |

`SIGHUP` also triggers a reload.

Readiness tracks the **cycle**, not each target, and never returns to 503 once satisfied. Tying it
to every target succeeding would let one permanently unreadable directory hold the whole exporter
out of service while every other target reported fine; re-evaluating it per request would drop the
exporter out of service each time a glob picked up a new directory. Per-target state is already
visible — an unscanned target simply has no `dir_exporter_size_bytes` series.

TLS and authentication come from `--web.config.file`, in the
[standard exporter-toolkit format](https://github.com/prometheus/exporter-toolkit/blob/master/docs/web-configuration.md).
There is no built-in fallback: without that file the endpoint is plain HTTP with no authentication,
so bind it to a private interface or restrict it with firewall rules.

## Impact on the host

The exporter reads only metadata, never file contents, so it does not evict application data from
the page cache. The residual cost is dentry and inode cache pressure — a ten-million-entry walk can
evict the inodes the application has cached — plus the I/O the walk itself issues.

Mitigations, roughly in order of effect:

1. **`--scan.io-priority` (default `idle`) and `--scan.nice` (default `19`)**, applied per scan
   worker thread on Linux. This is the only control that changes queueing *position* rather than
   just average rate: a rate-limited scan request still sits ahead of a latency-critical read that
   arrives just after it. The HTTP server and Go runtime threads keep normal priority.
2. **`--scan.concurrency` (default `2`)**, a hard cap across all targets, not per target.
3. **`--scan.rate-limit`**, in filesystem operations per second. Unlimited by default; around
   `20000` is a reasonable starting point. Tune against
   `dir_exporter_scan_rate_limit_wait_seconds_total`.
4. Host-side, consider lowering `vm.vfs_cache_pressure` so the kernel favours retaining inodes.

Scans are single-flight and the interval starts after a cycle ends, so a tree that takes longer to
walk than the interval will not queue up overlapping scans. Rejections are counted in
`dir_exporter_scan_skipped_total`.

### Hung mounts

Filesystem calls against a dead NFS server are uninterruptible; no timeout in user space can cancel
them. `--scan.hard-timeout` gives up on the target, releases the cycle and lets the exporter keep
running, abandoning the stuck worker rather than waiting for it. Watch
`dir_exporter_abandoned_scan_workers`: sustained values at or above `--scan.concurrency` mean the
worker pool is effectively dead and the exporter needs restarting once the mount is fixed.

## Metrics

Labels are spelled out in full below. Three appear repeatedly:

- **`target_path`** — the resolved absolute path of a monitored directory. One series per target.
- **`mountpoint`, `device`, `fstype`** — identify a *filesystem*, not a directory. Several targets
  usually share one, so these series appear once per filesystem, not once per target. `device` is
  the kernel device id (`8:1` on Linux, the volume root on Windows), not `/dev/sda1`.
- **`result`** and **`class`** — enumerated values, listed under their tables.

### Size

| Metric | Type | Labels | What it tells you | Enabled by |
|---|---|---|---|---|
| `dir_exporter_size_bytes` | gauge | `target_path` | Total logical size of all regular files under the directory. The headline number. **Absent**, not zero, until a scan completes cleanly. | always |
| `dir_exporter_disk_usage_bytes` | gauge | `target_path` | Space the files actually occupy on disk. Lower than `size_bytes` for sparse or compressed files, slightly higher for many tiny ones. **Use this for capacity planning.** | `--collector.disk-usage` (needs allocated-block support, i.e. Linux) |
| `dir_exporter_files` | gauge | `target_path` | How many regular files. A sudden jump with flat bytes means many small files — often a rotation misconfiguration. | `--collector.file-counts` |
| `dir_exporter_directories` | gauge | `target_path` | How many subdirectories. Mostly useful for spotting runaway directory creation. | `--collector.file-counts` |
| `dir_exporter_hardlinked_files` | gauge | `target_path` | Files with more than one hard link. Zero on most log trees; non-zero means an archiving scheme is in play. | `--scan.dedup-hardlinks` |
| `dir_exporter_hardlink_dedup_saved_bytes` | gauge | `target_path` | Bytes excluded from `size_bytes` because that file was already counted via another link. Shows how much dedup is changing the number. | `--scan.dedup-hardlinks` |
| `dir_exporter_hardlink_tracking_exhausted` | gauge 0/1 | `target_path` | `1` means the dedup memory cap was hit and duplicates are being counted again, so the size now **overstates** reality. Alert on this. | `--scan.dedup-hardlinks` |

### Growth (`--collector.growth`, on by default)

| Metric | Type | Labels | What it tells you |
|---|---|---|---|
| `dir_exporter_size_delta_bytes` | gauge (signed) | `target_path` | Change in size since the previous **completed** scan. Negative means the directory shrank. Absent until two scans have completed. |
| `dir_exporter_size_delta_interval_seconds` | gauge | `target_path` | The time the delta above spans. Divide the delta by this to get bytes/second. |
| `dir_exporter_bytes_added_total` | counter | `target_path` | Cumulative growth since the exporter started. `rate()` over this gives a smoothed growth rate. |
| `dir_exporter_bytes_removed_total` | counter | `target_path` | Cumulative shrinkage. Compare against `bytes_added_total` to see whether rotation is keeping up with writes. |

The delta spans **completed** scans, which may be several intervals apart if scans failed in
between. Always divide by `size_delta_interval_seconds`, never by the configured scan interval — a
run of failed scans would otherwise make growth look several times faster than it is.

### Filesystem context (`--collector.filesystem`)

| Metric | Type | Labels | What it tells you |
|---|---|---|---|
| `dir_exporter_filesystem_size_bytes` | gauge | `mountpoint`, `device`, `fstype` | Total capacity of the volume holding the target. |
| `dir_exporter_filesystem_free_bytes` | gauge | `mountpoint`, `device`, `fstype` | Free space including the root-reserved portion. |
| `dir_exporter_filesystem_avail_bytes` | gauge | `mountpoint`, `device`, `fstype` | Free space an unprivileged process can actually use. **This is the one to alert on** — it is what runs out first. |
| `dir_exporter_filesystem_files` | gauge | `mountpoint`, `device`, `fstype` | Total inodes. A volume can hit its inode limit with space to spare. Linux only. |
| `dir_exporter_filesystem_files_free` | gauge | `mountpoint`, `device`, `fstype` | Free inodes. Linux only. |
| `dir_exporter_target_mountpoint_info` | gauge = 1 | `target_path`, `mountpoint`, `device`, `fstype` | Maps a target to its filesystem. Carries no value itself — it exists so you can join directory metrics to volume metrics. |
| `dir_exporter_statfs_timeouts_total` | counter | `target_path` | Capacity queries abandoned after timing out. Rising means a hung mount. |
| `dir_exporter_filesystem_info_unavailable` | gauge 0/1 | `target_path` | `1` means capacity could not be read at the last refresh, so the volume metrics are missing for this target rather than stale. |

Capacity is emitted once per filesystem, not once per target — several targets commonly share a
volume, and duplicating capacity per target would create conflicting series.
`target_mountpoint_info` is the join key (see [Useful queries](#useful-queries)).

### Scan status

| Metric | Type | Labels | What it tells you |
|---|---|---|---|
| `dir_exporter_last_scan_success` | gauge 0/1 | `target_path` | Whether the most recent scan completed with no errors. Present from process start, so `0` means "not scanned yet or failed" while an *absent* series means the exporter is down. |
| `dir_exporter_last_scan_partial` | gauge 0/1 | `target_path` | `1` means the last scan is known to be incomplete, so the published size is older than the last attempt. |
| `dir_exporter_target_present` | gauge 0/1 | `target_path` | Whether the directory still existed at the last attempt. `0` with a retained size means it was deleted or unmounted. |
| `dir_exporter_scan_age_seconds` | gauge | `target_path` | Seconds since the last **successful** scan. Grows without bound while scans keep failing. **The single best staleness alert.** |
| `dir_exporter_last_good_scan_timestamp_seconds` | gauge | `target_path` | Unix time of the last successful scan — when the published size was actually measured. |
| `dir_exporter_last_scan_attempt_timestamp_seconds` | gauge | `target_path` | Unix time of the last attempt, successful or not. Compare with the above to see how long it has been failing. |
| `dir_exporter_scan_in_progress` | gauge 0/1 | `target_path` | Whether this target is being walked right now. |
| `dir_exporter_current_scan_duration_seconds` | gauge | `target_path` | How long the in-flight scan has been running, `0` when idle. Lets you distinguish a slow scan from a stalled one. |
| `dir_exporter_last_scan_duration_seconds` | gauge | `target_path` | How long the last scan took. Use this to size `--scan.timeout` and `--scan.interval`. |
| `dir_exporter_scans_total` | counter | `target_path`, `result` | Scans by outcome. `rate()` on `result="partial"` or `"timeout"` shows how often a target is failing. |
| `dir_exporter_scan_duration_seconds` | histogram | `result` | Distribution of scan durations. See the note below. |
| `dir_exporter_scan_skipped_total` | counter | — | Cycles rejected because one was already running. Expected to stay at `0`: the scan loop is sequential, so this only moves if something else drives a scan concurrently. Not a tuning signal — see below. |
| `dir_exporter_abandoned_scan_workers` | gauge | — | Workers that never exited, presumed stuck on a hung mount. Sustained values at or above `--scan.concurrency` mean the worker pool is dead and the exporter needs restarting. |

`result` is one of `complete`, `partial`, `timeout`, `cancelled`, `missing`, `root_error`.

**How often scans actually happen.** `--scan.interval` is the gap *after* a cycle ends, not a fixed
schedule, so the real period is `last_scan_duration_seconds + scan.interval`. A tree that takes
twenty minutes to walk on a five-minute interval is scanned every twenty-five minutes, not every
five — quietly, with nothing to flag it. If freshness matters, compare
`dir_exporter_last_scan_duration_seconds` against your configured interval, or alert on
`dir_exporter_scan_age_seconds`. `scan_skipped_total` does **not** report this; cycles are
sequential, so nothing is ever skipped.

The duration histogram is **off by default** (`--collector.scan-histogram`). It costs about 90
series — thirteen buckets plus sum and count, per outcome — and that cost is fixed regardless of
how many targets you monitor, so on a small deployment it is larger than everything else the
exporter emits combined. Scans also run every few minutes at most, which is far too few samples for
quantiles to mean much. `last_scan_duration_seconds` gives current duration per target and
`scans_total` gives outcome rates; reach for the histogram only when you specifically want duration
*distributions* over long windows. When enabled, buckets appear for an outcome the first time it
actually occurs.

### Errors and scan cost

| Metric | Type | Labels | What it tells you |
|---|---|---|---|
| `dir_exporter_last_scan_errors` | gauge | `target_path`, `class` | Errors in the most recent scan, grouped by kind. Resets each scan, so it reflects the current state rather than history. |
| `dir_exporter_scan_errors_total` | counter | `target_path`, `class` | Cumulative errors by kind. `rate()` shows whether a problem is ongoing or was a one-off. |
| `dir_exporter_scan_vanished_files_total` | counter | `target_path` | Files that disappeared between being listed and being measured. **Not an error** — this is normal log rotation. A sudden spike means unusually aggressive deletion. |
| `dir_exporter_scan_entries_total` | counter | `target_path` | Directory entries examined. Roughly the size of the job, useful for judging scan cost. |
| `dir_exporter_scan_stat_calls_total` | counter | `target_path` | Metadata lookups issued — the dominant per-file syscall cost. Divide by duration for an effective operations-per-second rate. |
| `dir_exporter_scan_dirs_read_total` | counter | `target_path` | Directories opened and read. |
| `dir_exporter_scan_skipped_other_filesystem_total` | counter | `target_path` | Directories skipped for being on a different filesystem. Non-zero means nested mounts exist under the target. |
| `dir_exporter_scan_rate_limit_wait_seconds_total` | counter | — | Time workers spent waiting for the rate limiter. Zero means `--scan.rate-limit` is not binding; rising steeply means it is throttling hard. |

`class` is one of `permission`, `not_found`, `io`, `loop`, `name_too_long`, `too_many_files`,
`other`. `too_many_files` means the exporter ran out of file descriptors — a problem with the
exporter's own limits, not with the directory.

### Exporter and targets

| Metric | Type | Labels | What it tells you |
|---|---|---|---|
| `dir_exporter_build_info` | gauge = 1 | `version`, `revision`, `branch`, `goversion`, … | Which build is running. Value is always 1; the information is in the labels. |
| `dir_exporter_platform_capability` | gauge 0/1 | `capability` | What this binary can actually measure on this host: `alloc_bytes`, `inode`, `fs_bytes`, `fs_inodes`, `mountpoint`. A `0` explains why a metric is missing. Assert these are `1` on production hosts to catch a mis-built binary. |
| `dir_exporter_targets_resolved` | gauge | — | How many directories are currently monitored. Watch for a glob quietly matching nothing, or matching far more than intended. |
| `go_*`, `process_*` | various | — | The exporter's own memory, goroutines and CPU (`--collector.self`, on by default). |

## Useful queries

Directory as a share of its volume:

```promql
dir_exporter_size_bytes
  * on(target_path) group_left(mountpoint) dir_exporter_target_mountpoint_info
  / on(mountpoint) group_left dir_exporter_filesystem_size_bytes
```

Hours until the volume fills at the current growth rate:

```promql
dir_exporter_filesystem_avail_bytes
  / on(mountpoint) group_right
    (sum by(mountpoint) (
      dir_exporter_size_delta_bytes / dir_exporter_size_delta_interval_seconds
        * on(target_path) group_left(mountpoint) dir_exporter_target_mountpoint_info
    ))
  / 3600
```

Suggested alerts:

```yaml
- alert: DirectorySizeStale
  expr: dir_exporter_scan_age_seconds > 3 * 300   # three scan intervals
  for: 10m
  annotations:
    summary: "{{ $labels.target_path }} has not completed a scan recently"

- alert: DirectoryScanPoolDead
  expr: dir_exporter_abandoned_scan_workers >= 2  # >= --scan.concurrency
  for: 15m
  annotations:
    summary: "Scan workers are stuck, most likely on a hung mount"

- alert: DirectoryNeverScanned
  expr: absent(dir_exporter_size_bytes) and on() dir_exporter_targets_resolved > 0
  for: 30m
```

Do not alert on `dir_exporter_size_bytes == 0`. The series is absent rather than zero when there is
nothing to report, so such a rule silently never fires.

## Logging

`--log.level` (`debug`, `info`, `warn`, `error`) and `--log.format` (`logfmt`, `json`) to stderr.

- One structured summary line per target per scan, with duration, bytes, entries, and error counts
  grouped by class.
- Progress lines while a scan exceeds `--log.scan-heartbeat`, so a slow scan is distinguishable
  from a hung one.
- At startup, every resolved setting with its source, every glob expansion, and the platform
  capabilities.

Per-file errors are **never** logged individually. A single wrong permission bit on a large tree
would otherwise emit millions of lines and fill the disk being monitored; the counts appear in the
per-scan summary and in the metrics instead.

## Building and testing

```
go build -o dist/directory-size-exporter ./cmd/directory-size-exporter
go test -race ./...
go vet ./...
```

`-race` needs cgo and therefore a C toolchain; on Windows without one it will not build, and the
race detector only runs in CI. Because the platform layer is build-tagged, always check both
targets before merging:

```
GOOS=linux   GOARCH=amd64 go build ./... && GOOS=linux   go vet ./...
GOOS=windows GOARCH=amd64 go build ./... && GOOS=windows go vet ./...
```

Production is Linux. The Windows build exists for development parity and does not support
allocated-block or inode-based features.

See `ARCHITECTURE.md` for how the packages fit together.
