# Session — Production Hardening for High-Volume Directory Monitoring

Full plan: `~/.claude/plans/i-have-made-a-dapper-canyon.md`
Design rationale now lives in `ARCHITECTURE.md`; operator docs in `README.md`.

## Goal

Make the exporter safe, accurate, observable and operable for continuous per-directory size
monitoring on trading hosts carrying multi-TB / 10M+ file log trees, without disturbing the
latency-sensitive application sharing the host.

## Status: all phases complete

| Phase | Work | Commit |
|---|---|---|
| P-1 | Hotfix for the false-zero defects on the old code | `ea5e756` |
| P0 | Dependency gate + cross-GOOS CI matrix | `2dbbfea` |
| P1 | `internal/fsstat` platform boundary + `FakeFS` | `2dbbfea` |
| P2 | `internal/state` retention rules (test-first) | `5d6508e` |
| P3 | `internal/scan` bounded-parallel engine and walker | `6a33fcb` |
| P4 | `internal/collector` + `internal/server` | `d78489b` |
| P5 | `internal/config` + `internal/targets` | `1c151cc` |
| P6/P7 | `main.go` rewiring, `internal/schedprio`, docs | this commit |

## Defects fixed

| # | Defect | Fixed in |
|---|---|---|
| D1 | Partial/timed-out scan overwrote a good size | P-1, then structurally in P2 |
| D2 | Missing root cached `SizeBytes=0` | P-1, P4 |
| D3 | **Symlinked target root reported 0 bytes with `success=1`** (failed green) | P-1, P5 |
| D4 | Zero-seeded cache emitted a real `0` before the first scan | P-1, P2 |
| D5 | Sorted+materialised ReadDir, per-entry `Join`, full-path `lstat` | P3 |
| D6 | `ctx.Err()` per entry took the context mutex | P3 |
| D7 | Hard links double-counted; mounts crossed; overlaps rescanned | P3, P5 |
| D8 | `promslog.AddFlags` never called | P6 |
| D9 | `WebConfigFile` hard-coded `""` — TLS/auth unreachable | P4, P6 |
| D10 | `scan_in_progress` global, single-flight rejection invisible | P3, P4 |

## Verification

```
go test ./...
go vet ./...
GOOS=linux   GOARCH=amd64 go build ./... && GOOS=linux   go vet ./...
GOOS=linux   GOARCH=arm64 go vet ./...
GOOS=windows GOARCH=amd64 go build ./... && GOOS=windows go vet ./...
```

All green. Smoke-tested end to end: metrics, `/-/ready`, landing page, filesystem context and
`build_info` all served correctly, with `disk_usage_bytes` correctly **absent** on Windows.

> **`-race` cannot run on this dev machine.** It needs cgo and there is no `gcc` in PATH. Every
> race-sensitive test (`TestStore_ConcurrentApplyAndSnapshot`,
> `TestEngine_PerTargetTotalsAreNotCrossContaminated`, `TestCollector_ConcurrentGatherDuringScan`,
> `TestFake_IsSafeForConcurrentUse`) only exercises the detector in Linux CI. A green local run is
> **not** evidence of race-freedom. Fixing this locally means installing MinGW-w64.

## Things a future session must not undo

1. **The latch ordering in `scan/engine.go`.** Capacity is reserved *before* children are enqueued
   and released *after* a directory completes. Reversing it finalises a target as **complete**
   holding a fraction of its size, which the retention rules then publish as truth. Mutation-tested:
   reversing the two lines fails `TestEngine_LatchDoesNotFireEarly` on run 0 with `size = 0, want 40`.
2. **`published` vs `good` in `state/store.go`.** Two measurements per target. Collapsing them makes
   `--scan.publish-partial` silently corrupt every growth metric and capacity forecast.
3. **Absent, never zero.** No measurement is emitted before the first completed scan, and no
   capability-dependent metric is approximated. Both were real defects.
4. **No per-file error logging.** Enforced by `TestScan_NeverLogsPerFileErrors`.

## Known gaps, deliberately not implemented

- **Per-target scan tuning.** The plan mentioned per-target interval/timeout/labels in YAML. The
  YAML file configures targets and global settings only; per-target scheduling would need changes
  in both the engine (one cycle currently scans all targets) and the collector. **This is the one
  planned item not delivered.**
- **kingpin bool flags.** kingpin treats booleans as valueless, so `--flag=true` would fail with
  "unexpected true". `Registry.NormalizeArgs` rewrites `=true`/`=false` into `--flag`/`--no-flag`
  so both spellings work; do not remove it without also fixing the docs, which use `=true`.
- **Windows is dev parity only.** No allocated-block or inode support by design.
- **`FSInfo.Device` is `major:minor`,** not `/dev/sda1`, to avoid parsing `/proc/self/mountinfo`.

## Possible follow-ups

- Per-target scan tuning (above).
- Raw `getdents64` parsing to remove the remaining per-entry allocation in the standard library.
- Ship the alert rules from `README.md` as a packaged rules file.
- Release automation: version ldflags are wired to `prometheus/common/version` but nothing sets them.
