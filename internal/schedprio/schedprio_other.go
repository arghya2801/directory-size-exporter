//go:build !linux

package schedprio

import (
	"fmt"
	"runtime"
)

const supported = false

// applyToCurrentThread is a no-op everywhere but Linux.
//
// Windows has thread priority and a background-mode flag, and other Unixes have setpriority, but
// none of them offer the block-layer idle class that makes this control worth having. Rather than
// apply half the policy and report success, the whole thing is declined so the caller can say so
// plainly in the startup audit.
func applyToCurrentThread(Priority) error {
	return fmt.Errorf("%w (GOOS=%s)", ErrUnsupported, runtime.GOOS)
}
