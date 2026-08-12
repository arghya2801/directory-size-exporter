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
targets:
  - /var/log/app
  - /data/logs/venue-*
scan:
  interval: 5m
  timeout: 30m
  rate-limit: 20000
  dedup-hardlinks: true
collector:
  file-counts: true
```

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
| `/-/ready` | 503 until every target has completed one scan, unless `--web.ready-requires-scan=false`. |
| `/-/reload` | Re-resolves targets. POST only, and off unless `--web.enable-lifecycle` is set. |

`SIGHUP` also triggers a reload.

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

`T` = `{target_path}`. `F` = `{mountpoint, device, fstype}`.

### Size

| Metric | Type | Labels | Enabled by |
|---|---|---|---|
| `dir_exporter_size_bytes` | gauge | T | always |
| `dir_exporter_disk_usage_bytes` | gauge | T | `--collector.disk-usage` (needs allocated-block support) |
| `dir_exporter_files` | gauge | T | `--collector.file-counts` |
| `dir_exporter_directories` | gauge | T | `--collector.file-counts` |
| `dir_exporter_hardlinked_files` | gauge | T | `--scan.dedup-hardlinks` |
| `dir_exporter_hardlink_dedup_saved_bytes` | gauge | T | `--scan.dedup-hardlinks` |
| `dir_exporter_hardlink_tracking_exhausted` | gauge | T | `--scan.dedup-hardlinks` |

`size_bytes` is logical length; `disk_usage_bytes` is space actually allocated. They differ for
sparse and compressed files, and `disk_usage_bytes` is the one to use for capacity planning.

### Growth (`--collector.growth`, on by default)

| Metric | Type | Labels |
|---|---|---|
| `dir_exporter_size_delta_bytes` | gauge (signed) | T |
| `dir_exporter_size_delta_interval_seconds` | gauge | T |
| `dir_exporter_bytes_added_total` | counter | T |
| `dir_exporter_bytes_removed_total` | counter | T |

The delta spans **completed** scans, which may be several intervals apart if scans failed in
between. Always divide by `size_delta_interval_seconds`, never by the configured scan interval.

### Filesystem context (`--collector.filesystem`)

| Metric | Type | Labels |
|---|---|---|
| `dir_exporter_filesystem_size_bytes` | gauge | F |
| `dir_exporter_filesystem_free_bytes` | gauge | F |
| `dir_exporter_filesystem_avail_bytes` | gauge | F |
| `dir_exporter_filesystem_files` | gauge | F (Linux) |
| `dir_exporter_filesystem_files_free` | gauge | F (Linux) |
| `dir_exporter_target_mountpoint_info` | gauge = 1 | `{target_path, mountpoint, device, fstype}` |
| `dir_exporter_statfs_timeouts_total` | counter | T |
| `dir_exporter_filesystem_info_unavailable` | gauge | T |

Capacity is emitted once per filesystem, not once per target. `target_mountpoint_info` carries the
join.

### Scan status

| Metric | Type | Labels |
|---|---|---|
| `dir_exporter_last_scan_success` | gauge | T |
| `dir_exporter_last_scan_partial` | gauge | T |
| `dir_exporter_target_present` | gauge | T |
| `dir_exporter_scan_age_seconds` | gauge | T |
| `dir_exporter_last_good_scan_timestamp_seconds` | gauge | T |
| `dir_exporter_last_scan_attempt_timestamp_seconds` | gauge | T |
| `dir_exporter_scan_in_progress` | gauge | T |
| `dir_exporter_current_scan_duration_seconds` | gauge | T |
| `dir_exporter_last_scan_duration_seconds` | gauge | T |
| `dir_exporter_scans_total` | counter | `{target_path, result}` |
| `dir_exporter_scan_duration_seconds` | histogram | `{result}` |
| `dir_exporter_scan_skipped_total` | counter | — |
| `dir_exporter_abandoned_scan_workers` | gauge | — |

`result` is one of `complete`, `partial`, `timeout`, `cancelled`, `missing`, `root_error`.

### Errors and scan cost

| Metric | Type | Labels |
|---|---|---|
| `dir_exporter_last_scan_errors` | gauge | `{target_path, class}` |
| `dir_exporter_scan_errors_total` | counter | `{target_path, class}` |
| `dir_exporter_scan_vanished_files_total` | counter | T |
| `dir_exporter_scan_entries_total` | counter | T |
| `dir_exporter_scan_stat_calls_total` | counter | T |
| `dir_exporter_scan_dirs_read_total` | counter | T |
| `dir_exporter_scan_skipped_other_filesystem_total` | counter | T |
| `dir_exporter_scan_rate_limit_wait_seconds_total` | counter | — |

`class` is one of `permission`, `not_found`, `io`, `loop`, `name_too_long`, `too_many_files`,
`other`. Files that disappear mid-scan are counted as *vanished*, not as errors — during log
rotation that is routine, and treating it as a fault would leave a busy directory permanently
unable to publish a size.

### Exporter and targets

`dir_exporter_build_info`, `dir_exporter_platform_capability{capability}`,
`dir_exporter_targets_resolved`, plus `go_*` and `process_*` (`--collector.self`, on by default).

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
