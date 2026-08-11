# Directory Size Exporter

A low-impact Prometheus exporter that periodically measures the logical size of regular files in configured directories. It scans in the background; Prometheus scrapes only read the last completed in-memory snapshot.

## Run

```text
directory-size-exporter-linux-amd64 \
  --path.target=/var/log/app \
  --path.target=/var/log/other \
  --scan.interval=5m \
  --scan.timeout=10m \
  --web.listen-address=:9115
```

The only HTTP endpoint is `GET /metrics`. It intentionally has no TLS or authentication configuration. Bind it to a private interface or protect the port with network policy/firewall rules.

Use `--path.target` once per directory. The older `--path.targets` flag remains available for compatibility but cannot represent paths containing commas.

Regular-file and subdirectory count metrics are disabled by default to keep the scrape payload smaller. Enable them with `--collector.file-counts=true` when needed.

## Behavior and metrics

- Targets are scanned serially, with at most one scan running at a time. The interval starts after a full scan completes, preventing a slow tree from causing continuous rescans.
- `dir_exporter_size_bytes` is logical file size, not allocated disk blocks; sparse files report their logical size.
- Symbolic links and non-regular files are excluded.
- `dir_exporter_last_scan_success` is `1` only when the scan completes with no errors.
- `dir_exporter_scan_errors` is a per-last-scan gauge; `dir_exporter_scan_errors_total` is process-lifetime cumulative.
- Before the first scan completes, the cached target reports a failed/unknown scan (`last_scan_success=0`, timestamp `0`).

Prometheus may scrape this endpoint more frequently than scans. For example, scrape every minute and scan every five minutes: each scrape receives the same cached value until the next completed scan, while `dir_exporter_last_scan_timestamp_seconds` records its freshness. Set a per-target `scan.timeout` appropriate to the directory and monitor `dir_exporter_scan_duration_seconds`, `dir_exporter_scan_in_progress`, and scan errors.

## Verify

```text
go test ./...
go test -race ./...
go vet ./...
```
