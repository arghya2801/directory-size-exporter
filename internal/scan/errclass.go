package scan

import (
	"errors"
	"io/fs"
	"syscall"
)

// Error classes.
//
// Grouping by class is what makes a permission storm survivable. A single wrong mode bit on a
// large tree produces millions of failures, and the exporter must summarise them into a handful of
// counters rather than emit a series or a log line per file.
const (
	ClassPermission   = "permission"
	ClassNotFound     = "not_found"
	ClassIO           = "io"
	ClassLoop         = "loop"
	ClassNameTooLong  = "name_too_long"
	ClassTooManyFiles = "too_many_files"
	ClassOther        = "other"
)

// AllErrorClasses lists every class, so counter series exist from process start rather than
// appearing only once the corresponding failure first occurs. An alert on a series that does not
// yet exist cannot fire.
var AllErrorClasses = []string{
	ClassPermission, ClassNotFound, ClassIO, ClassLoop,
	ClassNameTooLong, ClassTooManyFiles, ClassOther,
}

// classify maps an error to a class.
//
// Portable errors.Is targets are tried first so the common cases behave identically everywhere;
// the errno fallback then separates causes that share no portable sentinel. Descriptor exhaustion
// gets its own class deliberately: it means the exporter is misconfigured, not that the directory
// being measured has a problem, and conflating the two sends the operator to the wrong host.
func classify(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, fs.ErrPermission):
		return ClassPermission
	case errors.Is(err, fs.ErrNotExist):
		return ClassNotFound
	}

	var errno syscall.Errno
	if errors.As(err, &errno) {
		switch errno {
		case syscall.ELOOP:
			return ClassLoop
		case syscall.ENAMETOOLONG:
			return ClassNameTooLong
		case syscall.EMFILE, syscall.ENFILE:
			return ClassTooManyFiles
		case syscall.EIO, syscall.ENXIO, syscall.ENODEV, syscall.ENOTDIR, syscall.ESTALE:
			return ClassIO
		}
	}
	return ClassOther
}

// isVanished reports whether an error means the entry disappeared between being listed and being
// stat'ed.
//
// On a host doing continuous log rotation this is routine rather than a fault. Counting it as an
// error would mark every scan partial and leave a busy log directory permanently unable to publish
// a size — the failure mode is silent and total, so vanished entries are tracked separately.
// ESTALE is included because an NFS handle going stale under a concurrent rename means the same
// thing: the entry that was listed is no longer there.
func isVanished(err error) bool {
	if errors.Is(err, fs.ErrNotExist) {
		return true
	}
	var errno syscall.Errno
	return errors.As(err, &errno) && errno == syscall.ESTALE
}

// errorTally accumulates error counts by class for one scan.
type errorTally struct {
	counts map[string]int64
	total  int64
}

func (t *errorTally) add(err error) {
	class := classify(err)
	if class == "" {
		return
	}
	if t.counts == nil {
		t.counts = make(map[string]int64, 4)
	}
	t.counts[class]++
	t.total++
}

func (t *errorTally) merge(other *errorTally) {
	if other == nil || other.total == 0 {
		return
	}
	if t.counts == nil {
		t.counts = make(map[string]int64, len(other.counts))
	}
	for class, count := range other.counts {
		t.counts[class] += count
	}
	t.total += other.total
}

// snapshot returns a copy so the published result cannot be used to mutate the scan's tally.
func (t *errorTally) snapshot() map[string]int64 {
	out := make(map[string]int64, len(t.counts))
	for class, count := range t.counts {
		out[class] = count
	}
	return out
}
