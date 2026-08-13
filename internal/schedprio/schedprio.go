// Package schedprio lowers the CPU and I/O scheduling priority of scan worker threads.
//
// This is the highest-leverage control the exporter has over its impact on the host, and it works
// where a rate limit does not. A rate limit caps average operations per second but does nothing
// about queueing position: a scan request that arrives just before a latency-critical read from
// the trading application still sits ahead of it in the block layer's queue. Scheduling priority
// changes the ordering itself, which is the difference between a scan being invisible and being
// noticeable in tail latency.
//
// Priority is applied per thread rather than per process, so only the scan workers are
// deprioritised. The HTTP server, the runtime's own threads and the garbage collector keep normal
// priority, and a scrape stays fast even while a large walk is running.
package schedprio

import (
	"errors"
	"fmt"
	"strings"
)

// ErrUnsupported is returned on platforms with no scheduling-priority control.
var ErrUnsupported = errors.New("schedprio: not supported on this platform")

// IOClass is the I/O scheduling class for scan workers.
type IOClass int

const (
	// IOClassNone leaves the I/O priority untouched.
	IOClassNone IOClass = iota
	// IOClassBestEffort competes normally but at the lowest best-effort level.
	IOClassBestEffort
	// IOClassIdle only issues I/O when nothing else wants the device. This is the default: on a
	// host whose whole purpose is the application sharing it, the scan should always yield.
	IOClassIdle
)

// ParseIOClass converts the flag value.
func ParseIOClass(raw string) (IOClass, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "idle":
		return IOClassIdle, nil
	case "best-effort":
		return IOClassBestEffort, nil
	case "none", "":
		return IOClassNone, nil
	default:
		return IOClassNone, fmt.Errorf("invalid I/O priority class %q: want idle, best-effort or none", raw)
	}
}

func (c IOClass) String() string {
	switch c {
	case IOClassIdle:
		return "idle"
	case IOClassBestEffort:
		return "best-effort"
	default:
		return "none"
	}
}

// Priority is the scheduling policy to apply to a scan worker thread.
type Priority struct {
	// Nice is the CPU priority, from -20 (highest) to 19 (lowest).
	Nice int
	// IOClass is the block-layer scheduling class.
	IOClass IOClass
}

// Supported reports whether this platform can apply scheduling priority.
func Supported() bool { return supported }

// ApplyToCurrentThread lowers the priority of the calling OS thread.
//
// The caller MUST have pinned the goroutine to its thread with runtime.LockOSThread and must not
// unlock it, because Go may otherwise move the goroutine to a different thread and leave a
// deprioritised thread behind to serve unrelated work.
func ApplyToCurrentThread(p Priority) error { return applyToCurrentThread(p) }
