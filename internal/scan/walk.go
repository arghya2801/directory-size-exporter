package scan

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"path/filepath"

	"github.com/local/directory-size-exporter/internal/fsstat"
)

// dirRef is one unit of queued work: a directory that has been discovered but not yet read.
//
// It carries only a path, not an open handle. Directories are opened by the worker that dequeues
// them and closed before their children are published, so the number of open descriptors at any
// instant equals the worker count — independent of tree depth and queue length. Holding handles in
// the queue instead would make descriptor exhaustion a function of tree shape.
type dirRef struct {
	path   string
	depth  int
	target *targetScan
}

// processDir reads one directory and returns its child directories.
//
// Everything on the per-entry path is deliberately allocation-free: the stat result is a reused
// struct, entries are stat'ed by bare name against the open handle, and totals accumulate in local
// fields that are flushed once at the end.
func (e *Engine) processDir(ctx context.Context, ref *dirRef, st *fsstat.FileStat, totals *dirTotals) []*dirRef {
	*totals = dirTotals{}
	target := ref.target

	dir, err := e.fs.OpenDir(ref.path)
	if err != nil {
		// A directory that vanished between being listed and being opened is log rotation, not a
		// fault. Anything else is a genuine error against this target.
		if isVanished(err) {
			totals.vanished++
		} else {
			totals.errors.add(err)
		}
		target.flush(totals)
		return nil
	}
	defer dir.Close()

	var children []*dirRef
	for {
		if target.cancelled.Load() || target.abandoned.Load() {
			break
		}
		entries, err := dir.ReadSome(e.cfg.BatchSize)
		if err != nil && !errors.Is(err, io.EOF) {
			totals.errors.add(err)
			break
		}
		if len(entries) == 0 {
			break
		}

		// One limiter acquisition covers the whole batch. Calling Wait per entry would serialise
		// every worker on the limiter's internal mutex ten million times, making the limiter the
		// bottleneck instead of the disk it is meant to protect.
		if e.limiter != nil {
			if waitErr := e.waitForTokens(ctx, len(entries)+1); waitErr != nil {
				break
			}
		}

		for i := range entries {
			child := e.processEntry(dir, ref, &entries[i], st, totals)
			if child != nil {
				children = append(children, child)
			}
		}
		if errors.Is(err, io.EOF) {
			break
		}
	}

	totals.dirs += int64(len(children))
	target.flush(totals)
	return children
}

// processEntry handles a single directory entry, returning a child directory to queue if the entry
// is one that should be descended into.
func (e *Engine) processEntry(dir fsstat.Dir, ref *dirRef, entry *fsstat.Entry, st *fsstat.FileStat, totals *dirTotals) *dirRef {
	totals.entries++

	// Symbolic links are never followed and never counted. Following them would double-count files
	// reachable by two paths and could walk straight out of the target into the rest of the disk.
	if entry.IsSymlink() {
		return nil
	}

	st.Reset()
	totals.statCalls++
	if err := dir.StatEntry(entry.Name, st); err != nil {
		if isVanished(err) {
			totals.vanished++
		} else {
			totals.errors.add(err)
		}
		return nil
	}

	// The directory read may report no type at all on some filesystems, so the stat result is
	// authoritative rather than the entry type.
	if st.Mode&fs.ModeSymlink != 0 {
		return nil
	}

	if st.IsDir() {
		if e.cfg.OneFilesystem && ref.target.hasRootDev && st.Dev != ref.target.rootDev {
			// A nested mount is a different filesystem with its own capacity. Counting it into
			// this target would attribute bytes to a volume that does not hold them.
			totals.skippedXdev++
			return nil
		}
		return &dirRef{path: filepath.Join(ref.path, entry.Name), depth: ref.depth + 1, target: ref.target}
	}

	// Sockets, devices, pipes and doors occupy no meaningful space in a log tree.
	if !st.IsRegular() {
		return nil
	}

	if !ref.target.shouldCount(st, e.cfg.DedupHardlinks, totals) {
		return nil
	}
	totals.size += st.Size
	totals.alloc += st.AllocBytes
	totals.files++
	return nil
}
