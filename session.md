# Session — Production Hardening for High-Volume Directory Monitoring

Full plan: `~/.claude/plans/i-have-made-a-dapper-canyon.md`

## Goal

Make the exporter safe, accurate, observable and operable for continuous per-directory size
monitoring on trading hosts carrying multi-TB / 10M+ file log trees, without disturbing the
latency-sensitive application sharing the host.

## Defects being fixed

| # | Defect | Location | Phase |
|---|---|---|---|
| D1 | Partial/timed-out scan overwrites a good size with a partial one | `collector.go:151-153` | P-1, P2 |
| D2 | Missing root caches `SizeBytes=0` | `collector.go:163-172` | P-1, P4 |
| D3 | **Symlinked target root reports 0 bytes with `last_scan_success=1`** (fails green) | `collector.go:174-176` | P-1 |
| D4 | Zero-seeded cache emits genuine `0` before the first scan completes | `collector.go:66-68` | P-1 |
| D5 | Sorted+materialized ReadDir, per-entry `Join` alloc, full-path `lstat` per file | `collector.go:163-196` | P3 |
| D6 | `ctx.Err()` per entry takes the context mutex | `collector.go:164` | P3 |
| D7 | Hard links double-counted; mounts crossed; overlapping targets rescanned | `collector.go:191,218-241` | P3 |
| D8 | `promslog.AddFlags` never called — no `--log.level`/`--log.format` | `main.go:39` | P5 |
| D9 | `WebConfigFile` hard-coded `""` — TLS/auth unreachable | `main.go:66-67` | P4 |
| D10 | `scan_in_progress` global not per-target; single-flight rejection invisible | `collector.go:64,134` | P3 |

## Checklist

### P-1 — Hotfix on current code (own commit) — **DONE**
- [x] Split `targetState` into `good` (error-free scans only) and `last` (every attempt)
- [x] Write the good value only when `LastScrapeSuccess == 1` (D1)
- [x] Delete zero-seeding loop; `Collect` emits size only when a good value exists (D4)
- [x] `EvalSymlinks` + `IsDir` check on the target root before walking (D3)
- [x] `failedScan` helper so an unresolvable/non-directory root reports an error, not zero bytes
- [x] Rewrote `TestMissingTargetIsReportedAsFailedScan` (it asserted the bug)
- [x] Added `TestFailedScanRetainsLastGoodSize`, `TestNoSizeSeriesBeforeFirstScan`,
      `TestSymlinkedTargetRootIsResolved`, `TestNonDirectoryTargetIsReportedAsFailedScan`
- [x] `go test ./...`, `go vet ./...`, cross-GOOS build all green

### P0 — Dependencies and CI — **DONE**
- [x] **Dependency gate cleared.** Proxy reachable; `go get` resolved every module the plan needs:
      `kingpin/v2 v2.4.0` (pulling `alecthomas/units` and `xhit/go-str2duration/v2`) and
      `yaml.v3 v3.0.1`. `x/time v0.15.0`, `x/sync v0.22.0`, `x/sys v0.47.0` are already in the
      module graph. All are now warm in the local module cache.
- [x] Reverted `go.mod`/`go.sum` afterwards — an unused requirement is noise that `go mod tidy`
      strips anyway. P5 adds kingpin/yaml and P1/P7 promote `x/sys` **when the imports land**.
- [x] `.github/workflows/ci.yml`: race job on Linux + cross-platform matrix
      (linux/amd64, linux/arm64, windows/amd64) building and vetting every GOOS, plus a
      `go mod tidy` drift check.

### P1 — `internal/fsstat` — **DONE**
- [x] `fsstat.go`: `FS` + `Dir` interfaces, `FileStat`, `FSInfo`, `Capability` bits,
      compile-time `var _ FS = systemFS{}` assertion so a missing platform stub fails at compile
      time on the offending GOOS rather than at link time
- [x] `fsstat_linux.go`: `openat(O_NOFOLLOW|O_DIRECTORY|O_CLOEXEC)`, `fstatat` against the held
      dirfd with a reused `unix.Stat_t`, `statfs`, `AllocBytes = Blocks * 512`, mountpoint via
      `Dev` walk-up, fstype magic table. Arch-portable conversions (builds on arm64).
- [x] `fsstat_windows.go`: `GetDiskFreeSpaceEx` + `GetVolumePathName` + `GetVolumeInformation`;
      `CapAllocBytes`/`CapInode` deliberately withheld
- [x] `fsstat_other.go`: caps 0, everything returns `ErrUnsupported` — still builds and runs
- [x] `fake.go`: in-memory `FS` with shuffled reads, synthetic mega-directories, per-path
      open/stat errors, before-open/read/stat hooks (for hung-mount tests), open-dir high-water
      mark, and bare-name assertion. Concurrency-safe.
- [x] Tests: portable (13), linux-only (9), windows-only (4). Windows suite green locally;
      linux + arm64 + darwin all type-check via `GOOS=... go vet`.
- [x] `x/sys` promoted to a direct dependency by `go mod tidy` as the import landed.

**Design note — no device-node name.** `FSInfo.Device` is the kernel device id (`major:minor`),
not `/dev/sda1`. Resolving a device name needs `/proc/self/mountinfo`, which races with concurrent
mounts and shows the *host's* mount tree from inside a container. `Mountpoint` is the intended join
key between a target and its filesystem, so the device name is not needed for any planned query.

### P2 — `internal/state` (test-first)
- [ ] `TestStore_RetentionTable` written **before** `store.go`
- [ ] `store.go`: lastGood replaced only on `complete`; delta spans only completed scans
- [ ] Nothing emitted before the first `complete`; monotonic added/removed counters
- [ ] `--scan.publish-partial` freezes delta+counters even when publishing
- [ ] Race test for concurrent Apply/Snapshot

### P3 — `internal/scan`
- [ ] `engine.go`: global pool, per-worker LIFO stacks, non-blocking bounded overflow deque
- [ ] **Increment-before-enqueue latch invariant** + 500× early-fire test
- [ ] Batched rate limiter (`WaitN` once per batch), per-batch cancellation check
- [ ] `--scan.hard-timeout` worker abandonment + `abandoned_scan_workers`
- [ ] `walk.go`: one-filesystem, hardlink dedup (`Nlink>1` only, bounded), vanished-file handling
- [ ] `errclass.go`, `progress.go` heartbeats, `summary.go` — **never per-file error logs**

### P4 — `internal/collector` + `internal/server`
- [ ] Descs, per-target collector, filesystem collector deduped by device ID
- [ ] Self collectors: `go_*`, `process_*`, `build_info`, `platform_capability`
- [ ] Landing page, `/-/healthy`, `/-/ready`, `/-/reload`
- [ ] TLS + basic auth via `--web.config.file`; acceptance tests prove it
- [ ] Replace the `/` → 404 assertion in `http_acceptance_test.go:49-51`

### P5 — `internal/config` + `internal/targets`
- [ ] kingpin flags + `DIR_EXPORTER_*` env + strict YAML; precedence `flag > env > yaml > default`
- [ ] `TestFlags_EveryConfigFieldHasAFlag` (reflection, prevents drift)
- [ ] Tri-state `true|false|auto` capability gating; `true` on unsupported ⇒ exit 2
- [ ] Startup audit lines: every setting with its source
- [ ] Resolver: globs, `EvalSymlinks`, dedup, overlap detection, re-resolution diff, `--targets.max`

### P6 — Wiring and docs
- [ ] `main.go` reduced to wiring; SIGHUP + `/-/reload`
- [ ] README + ARCHITECTURE.md rewrite
- [ ] Migration note for breaking metric changes + shipped alert rules

### P7 — `internal/schedprio`
- [ ] Linux `setpriority` + `ioprio_set` per scan-worker thread under `runtime.LockOSThread`
- [ ] `--scan.nice` (19), `--scan.io-priority` (idle); no-op elsewhere

## Verification (every phase)

```
go test ./...
go vet ./...
GOOS=linux   GOARCH=amd64 go build ./... && GOOS=linux   go vet ./...
GOOS=windows GOARCH=amd64 go build ./... && GOOS=windows go vet ./...
```

The cross-GOOS pair is mandatory — build-tagged files break silently otherwise, and dev is
Windows while production is Linux.

> **`-race` cannot run on this dev machine.** It requires cgo and there is no `gcc` in PATH, so
> `CGO_ENABLED=1 go test -race ./...` fails to build. Every race-sensitive test the plan calls for
> (`TestStore_ConcurrentApplyAndSnapshotIsRaceFree`, `TestEngine_PerTargetStatsAreNotCrossContaminated`,
> `TestCollector_ConcurrentGatherDuringScan`) must therefore be gated in **Linux CI**, which is the
> only place the detector will actually run. Do not treat a local green run as race-clean. Fixing
> this locally means installing a MinGW-w64 toolchain.

## Current state

P-1, P0 and P1 complete and verified. Next: P2 — `internal/state`, written test-first from the
FR3 retention table.
