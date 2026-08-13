package scan

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/local/directory-size-exporter/internal/fsstat"
)

// inodeKey identifies a file uniquely within a filesystem. Both halves are needed: inode numbers
// are only unique per device, so a target spanning two mounts would otherwise collide.
type inodeKey struct {
	dev uint64
	ino uint64
}

// dirTotals accumulates one directory's contribution in plain, unsynchronised fields.
//
// This exists purely to keep atomics off the per-file path. Adding to a shared atomic once per
// file means ten million contended writes to one cache line across every worker; accumulating
// locally and flushing once per directory reduces that by roughly three orders of magnitude,
// because directories are a fraction of a percent of entries in a log tree.
type dirTotals struct {
	size        int64
	alloc       int64
	files       int64
	dirs        int64
	entries     int64
	statCalls   int64
	vanished    int64
	skippedXdev int64
	hardlinked  int64
	dedupSaved  int64
	errors      errorTally
}

// targetScan accumulates one target's in-flight scan.
type targetScan struct {
	path      string
	startedAt time.Time

	// rootDev anchors the one-filesystem check. A directory whose device differs from this one is
	// a separate mount and is not part of the target being measured.
	rootDev    uint64
	hasRootDev bool

	size, alloc, files, dirs     atomic.Int64
	entries, statCalls, dirsRead atomic.Int64
	vanished, skippedXdev        atomic.Int64
	hardlinked, dedupSaved       atomic.Int64

	errMu  sync.Mutex
	errors errorTally

	// inodes is populated only for files with more than one link. On a normal log tree that is no
	// files at all, so hard-link dedup costs nothing in memory or lock contention until a tree
	// actually contains hard links.
	inodeMu         sync.Mutex
	inodes          map[inodeKey]struct{}
	maxTracked      int
	inodesExhausted atomic.Bool

	// pending counts directories queued or in flight for this target. It is incremented BEFORE a
	// child is enqueued and decremented AFTER a directory completes; see completeDir.
	pending  atomic.Int64
	done     chan struct{}
	doneOnce sync.Once

	// ctx carries this target's deadline. Workers consult it once per read batch — never per
	// entry, where taking the context's mutex ten million times would be both a cost and a
	// contention point shared across every worker.
	//
	// It is deliberately the context itself rather than a flag some other goroutine sets from it.
	// A flag makes cancellation latency depend on that goroutine being scheduled, which on a busy
	// or single-core host let a cancelled walk run on for tens of batches past its deadline.
	ctx context.Context

	// abandoned marks a target given up on after the hard timeout, so workers discard its
	// remaining queue without performing any I/O.
	abandoned atomic.Bool
}

// stopping reports whether this target should stop being walked.
func (t *targetScan) stopping() bool {
	return t.abandoned.Load() || (t.ctx != nil && t.ctx.Err() != nil)
}

func newTargetScan(path string, startedAt time.Time, maxTracked int) *targetScan {
	return &targetScan{
		path:       path,
		startedAt:  startedAt,
		maxTracked: maxTracked,
		done:       make(chan struct{}),
	}
}

// addPending reserves capacity for directories about to be enqueued.
//
// It MUST be called before the corresponding push. Enqueuing first and reserving afterwards leaves
// a window in which a worker has taken the last queued directory but not yet published its
// children, so pending momentarily reads zero, the latch fires, and the target is finalised as
// COMPLETE holding a fraction of its true size. That is strictly worse than a failed scan: the
// retention rules would accept it and publish it.
func (t *targetScan) addPending(n int64) { t.pending.Add(n) }

// completeDir records that one directory finished, closing the latch when the last one lands.
func (t *targetScan) completeDir() {
	if t.pending.Add(-1) == 0 {
		t.doneOnce.Do(func() { close(t.done) })
	}
}

// finish releases the latch regardless of outstanding work, for a target abandoned after the hard
// timeout because a worker is stuck in an uninterruptible syscall.
func (t *targetScan) finish() {
	t.doneOnce.Do(func() { close(t.done) })
}

// flush folds one directory's locally accumulated totals into the target.
func (t *targetScan) flush(totals *dirTotals) {
	if totals.size != 0 {
		t.size.Add(totals.size)
	}
	if totals.alloc != 0 {
		t.alloc.Add(totals.alloc)
	}
	if totals.files != 0 {
		t.files.Add(totals.files)
	}
	if totals.dirs != 0 {
		t.dirs.Add(totals.dirs)
	}
	if totals.entries != 0 {
		t.entries.Add(totals.entries)
	}
	if totals.statCalls != 0 {
		t.statCalls.Add(totals.statCalls)
	}
	if totals.vanished != 0 {
		t.vanished.Add(totals.vanished)
	}
	if totals.skippedXdev != 0 {
		t.skippedXdev.Add(totals.skippedXdev)
	}
	if totals.hardlinked != 0 {
		t.hardlinked.Add(totals.hardlinked)
	}
	if totals.dedupSaved != 0 {
		t.dedupSaved.Add(totals.dedupSaved)
	}
	if totals.errors.total != 0 {
		t.errMu.Lock()
		t.errors.merge(&totals.errors)
		t.errMu.Unlock()
	}
	t.dirsRead.Add(1)
}

// shouldCount decides whether a file's bytes count toward the target, applying hard-link dedup.
//
// Dedup is scoped to a single target on purpose. Sharing one inode set across targets would make
// per-target sizes non-additive and, worse, order-dependent: whichever target happened to be
// scanned first would claim the shared bytes, so the same host could report different numbers
// after a restart shuffled the scan order.
func (t *targetScan) shouldCount(st *fsstat.FileStat, dedup bool, totals *dirTotals) bool {
	if !dedup || st.Nlink <= 1 {
		return true
	}
	totals.hardlinked++

	key := inodeKey{dev: st.Dev, ino: st.Ino}
	t.inodeMu.Lock()
	if _, seen := t.inodes[key]; seen {
		t.inodeMu.Unlock()
		totals.dedupSaved += st.Size
		return false
	}
	if t.maxTracked > 0 && len(t.inodes) >= t.maxTracked {
		t.inodeMu.Unlock()
		// Tracking is capped so a hard-link-heavy tree cannot exhaust memory. Past the cap
		// duplicates are counted again, which overstates the size — so the condition is surfaced
		// as its own metric rather than silently degrading the number.
		t.inodesExhausted.Store(true)
		return true
	}
	if t.inodes == nil {
		t.inodes = make(map[inodeKey]struct{}, 1024)
	}
	t.inodes[key] = struct{}{}
	t.inodeMu.Unlock()
	return true
}

// errorSnapshot returns a copy of the accumulated error counts.
func (t *targetScan) errorSnapshot() (map[string]int64, int64) {
	t.errMu.Lock()
	defer t.errMu.Unlock()
	return t.errors.snapshot(), t.errors.total
}

// recordError attributes an error that occurred outside any directory's local accumulation, such
// as a failure to open the target root.
func (t *targetScan) recordError(err error) {
	t.errMu.Lock()
	t.errors.add(err)
	t.errMu.Unlock()
}
