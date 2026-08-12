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

## Post-review fixes

A review of the branch against `main` found three defects, all fixed and all mutation-tested
(the fix was reverted and the new test confirmed to fail):

1. **`abandoned_scan_workers` reported the live worker count**, so it equalled `--scan.concurrency`
   during every normal scan. The shipped alert fires at `>= concurrency`, so on any host whose scan
   cycle outlasts the alert window — the intended workload — it would have fired continuously and
   been silenced, taking the real signal with it. Now sampled only after the pool closes.
   Mutation: `abandoned workers = 4 during a healthy scan`.
2. **`supervise` blocked forever at shutdown under default settings.** `--scan.timeout` defaults to
   `0`, so no hard timeout is derived and its timer channel is nil. Workers exit on root-context
   cancellation leaving queues undrained, so the latch never fired and the wait never ended. The
   cancelled path now has its own deadline. Mutation: `ScanAll never returned after cancellation`.
3. **`DIR_EXPORTER_CONFIG_FILE` was silently ignored.** `Resolve` loaded YAML before the environment
   pass, so the variable naming the file was read too late. It is now resolved first.
   Mutation: `scan.batch-size = 1024, want 77`.

The review's lower-severity items were also addressed: filesystem timeout counts are pruned when a
target is retired; derived settings are audited as `origin=derived` rather than `default`;
`--scan.stale-after` must exceed `--scan.interval` or the series would flap every cycle.

**Readiness was redefined while fixing it.** It now tracks completion of the first scan *cycle*,
not success of every target. Requiring every target would let one permanently unreadable directory
hold the exporter at 503 forever while nineteen others reported fine — under an orchestrator, the
process would never enter service. It stays one-way, so a glob picking up a new directory does not
drop the exporter out of service; an unscanned target is already visible by having no size series.

**The audit attribute is keyed `origin`, not `source`.** promslog reserves `source` for the code
location and silently renames colliding attributes, so the key differed between the unit test's
plain handler and the real binary. Caught by a smoke test, not by the unit test.

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

## Out of scope, confirmed with the owner

- **Per-target scan tuning is not wanted.** One `scan.interval` and one `scan.timeout` for every
  target is the intended behaviour, not a shortfall. The plan had floated per-target
  interval/timeout in YAML; the owner confirmed a single global value is correct for this
  workload. **Do not build a per-target scheduler unless that changes.**
- **Custom per-target labels are not wanted.** `target_path` is the only per-target label.

## Design decisions worth knowing

- **kingpin bool flags.** kingpin treats booleans as valueless, so `--flag=true` would fail with
  "unexpected true". `Registry.NormalizeArgs` rewrites `=true`/`=false` into `--flag`/`--no-flag`
  so both spellings work; do not remove it without also fixing the docs, which use `=true`.
- **Windows is dev parity only.** No allocated-block or inode support by design.
- **`FSInfo.Device` is `major:minor`,** not `/dev/sda1`, to avoid parsing `/proc/self/mountinfo`.

## Possible follow-ups

- Raw `getdents64` parsing to remove the remaining per-entry allocation in the standard library.
- Ship the alert rules from `README.md` as a packaged rules file.
- Release automation: version ldflags are wired to `prometheus/common/version` but nothing sets them.
