//go:build linux

package schedprio

import (
	"fmt"

	"golang.org/x/sys/unix"
)

const supported = true

// ioprio_set encoding, from include/uapi/linux/ioprio.h. There is no wrapper in x/sys, so the
// syscall is issued directly.
const (
	ioprioWhoProcess = 1
	ioprioClassShift = 13

	ioprioClassBestEffort = 2
	ioprioClassIdle       = 3

	// Level 7 is the lowest priority within the best-effort class.
	ioprioLowestBestEffortLevel = 7
)

func applyToCurrentThread(p Priority) error {
	// Both calls below act on a thread id. On Linux, nice has been per-thread since 2.6.12, and
	// ioprio_set with IOPRIO_WHO_PROCESS applied to a tid likewise targets that single thread.
	// That is exactly what is wanted: deprioritising the whole process would slow the HTTP server
	// and the garbage collector too, making scrapes sluggish for no benefit.
	tid := unix.Gettid()

	if err := unix.Setpriority(unix.PRIO_PROCESS, tid, p.Nice); err != nil {
		return fmt.Errorf("set nice %d on thread %d: %w", p.Nice, tid, err)
	}

	class, ok := ioprioClass(p.IOClass)
	if !ok {
		return nil
	}
	level := 0
	if class == ioprioClassBestEffort {
		level = ioprioLowestBestEffortLevel
	}
	value := class<<ioprioClassShift | level
	if _, _, errno := unix.Syscall(unix.SYS_IOPRIO_SET, ioprioWhoProcess, uintptr(tid), uintptr(value)); errno != 0 {
		return fmt.Errorf("set I/O priority %s on thread %d: %w", p.IOClass, tid, errno)
	}
	return nil
}

func ioprioClass(class IOClass) (int, bool) {
	switch class {
	case IOClassIdle:
		return ioprioClassIdle, true
	case IOClassBestEffort:
		return ioprioClassBestEffort, true
	default:
		return 0, false
	}
}
