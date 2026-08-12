# Architecture

## Shape

```
cmd/directory-size-exporter   wiring only
  │
  ├── internal/config      flags + env + YAML, precedence and provenance, validation
  ├── internal/targets     glob expansion, symlink resolution, overlap detection
  ├── internal/fsstat      the only package that touches platform syscalls
  ├── internal/schedprio   per-thread CPU and I/O priority (Linux)
  ├── internal/scan        the walker and its worker pool
  ├── internal/state       what may be published, and when
  ├── internal/collector   translation into Prometheus metrics
  └── internal/server      HTTP surface
```

Data flows one way:

```
targets ──paths──▶ scan ──Result──▶ state ──Snapshot──▶ collector ──▶ server
                    │                                      ▲
                 fsstat ──────────────FSInfo────────────────┘
```

## Why the boundaries are where they are

### `state` is separate from `collector`

The rules deciding whether a measurement may be published are the highest-risk correctness surface
in the exporter. Keeping them in a package with no dependency on Prometheus, no filesystem access
and an injected clock means the entire decision table is a table-driven unit test.

The governing rule is one sentence: **the published measurement is replaced only by a scan that
completed without errors.** Everything else — partial, timeout, missing, cancelled — updates status
alone. A partial walk produces a systematically low number, and publishing it is indistinguishable
from the directory shrinking.

Two measurements are kept per target. The delta baseline moves only on a clean scan; the published
value is what the collector renders. They differ only under `--scan.publish-partial`, where an
operator can see a rough number without letting it corrupt growth data. Collapsing them into one
field would make that flag silently poison every capacity forecast.

### `scan` is separate from `fsstat`

The walker never imports `syscall` or `golang.org/x/sys`. It depends on the `fsstat.FS` interface,
so it is fully testable on any platform against `fsstat.NewFake` — including conditions that are
impractical to stage for real: a ten-million-entry directory, a read that blocks forever, a file
that vanishes between being listed and being stat'ed.

`fsstat` also owns the degradation policy: **a measurement whose capability is unavailable is never
reported.** It is not approximated and not reported as zero. Reporting logical size as disk usage on
a platform that cannot read allocated blocks would understate every sparse file while looking
exactly like a real measurement.

### `config` has one registry

Flags, environment variables, YAML keys, defaults and validation all come from a single field
registry. The failure this prevents is a setting that works as a flag but is silently ignored in
YAML — infuriating to debug, because the configuration file looks correct. A reflection test
asserts no `Config` field escapes the registry, since such a field would be unreachable from
`--help`, the environment and YAML while looking present in the source.

## The scan engine

### Custom walker, not `filepath.WalkDir`

`WalkDir` hands its callback a path string and nothing else. There is no way to obtain a directory
descriptor through it, so every entry must be stat'ed by full path — making the kernel re-resolve
every ancestor component for each of ten million files. `os.ReadDir` additionally sorts and fully
materialises each directory before any work begins.

The walker here opens a directory once, reads it incrementally and unsorted, and stats each entry
by **bare name** against the held descriptor. One component resolved per file instead of the whole
path.

### One pool across all targets

`--scan.concurrency` caps directories being read simultaneously across every target, not per
target. Per-target pools would multiply filesystem pressure by target count: twelve targets with a
limit of four would issue forty-eight concurrent walks.

Each worker keeps a private LIFO stack — depth-first, so the working set stays small and the
directory cache stays hot — and donates to a shared queue only when it holds plenty of work. **A
worker that cannot donate keeps the work rather than blocking.** No worker ever waits to enqueue,
which is precisely the deadlock a bounded channel with recursive sends would hit.

Directories are opened, drained and closed *before* their children are queued, so open descriptors
equal the worker count regardless of tree depth or queue length.

### The completion latch

Each target counts its outstanding directories. Capacity is reserved **before** children are
enqueued and released **after** a directory completes.

The reverse order leaves a window where a worker has taken the last queued directory but not yet
published its children. The count momentarily reads zero, the latch fires, and the target is
finalised as **complete** while holding a fraction of its size — which the retention rules would
then accept and publish. This is the single most dangerous failure mode in the codebase, worse than
anything the retention rules exist to prevent.

`TestEngine_LatchDoesNotFireEarly` is mutation-tested: reversing the two operations fails it
immediately with `size = 0, want 40`. **If that ordering is ever touched, re-run the mutation.**

### Cancellation and rate limiting are per batch

Both are checked once per directory read batch rather than per entry. `context.Err` takes a mutex,
and `rate.Limiter.Wait` takes another; calling either ten million times makes them the bottleneck
instead of the disk they exist to protect. The cost is bounded overshoot of roughly one batch.

### Hung mounts

Filesystem syscalls against a dead NFS server sit in uninterruptible sleep, so no context plumbing
can cancel them. `--scan.hard-timeout` abandons the target, releases the latch and lets the cycle
finish; workers are drained with a bound rather than waited on. Each cycle starts fresh workers, so
throughput recovers by itself, and leaked goroutines surface as
`dir_exporter_abandoned_scan_workers`.

The same reasoning applies to `statfs`, which is why filesystem capacity is refreshed on the scan
cycle and never during a scrape. Unlike directory sizes, a failed capacity read publishes
**nothing** rather than retaining the previous value: free space is cheap to re-read and feeds
directly into hours-until-full, so a stale figure is worse than an absent one.

### Atomics are per directory, not per file

Each directory accumulates into plain local fields and flushes once at the end. Adding to a shared
atomic per file would mean ten million contended writes to one cache line across every worker;
directories are a fraction of a percent of entries in a log tree.

Hard-link tracking touches a shared map only for files with more than one link, so a tree without
hard links allocates nothing. Dedup is scoped per target — a shared inode set would make per-target
sizes non-additive and order-dependent, so the same host could report different numbers after a
restart shuffled the scan order.

## Logging

Errors are summarised **by class into counters** and appear in exactly one log line per target per
scan. A single wrong permission bit on a ten-million-file tree would otherwise emit millions of
lines, filling the disk on the host whose disk usage is being monitored.
`TestScan_NeverLogsPerFileErrors` asserts exactly one record for two thousand failures.

## Testing

| Package | Approach |
|---|---|
| `state` | Pure logic, injected clock. The retention table transcribed as a test. |
| `scan` | Against `fsstat.NewFake`, so it runs identically on Linux and Windows. |
| `fsstat` | Portable capability tests, plus build-tagged tests for real syscall behaviour. |
| `config` | Reflection test for coverage; injected capability set covers both platform policies. |
| `collector` | `CollectAndLint` plus golden comparisons for absent-versus-zero. |
| `server` | Real `web.Serve` for TLS and auth, not a wrapped handler. |

`-race` requires cgo and runs only in Linux CI. A green local run on a machine without a C
toolchain is not evidence of race-freedom.

The CI matrix builds and vets `linux/amd64`, `linux/arm64` and `windows/amd64`. Build-tagged files
break silently on the platform you are not developing on, and here production is Linux while
development is commonly Windows.

## Known limitations

- **Per-target scan tuning is not implemented.** The YAML file configures targets and global
  settings; per-target intervals, timeouts and custom labels would need scheduler and collector
  changes.
- **Windows is development parity only.** Allocated-block size and inode identity would each need
  a file handle per file, which is prohibitive at scale and unnecessary since production is Linux.
- **No device-node names.** `FSInfo.Device` is the kernel device id (`major:minor`), not
  `/dev/sda1`. Resolving the name means parsing `/proc/self/mountinfo`, which races with concurrent
  mounts and shows the host's mount tree from inside a container.
