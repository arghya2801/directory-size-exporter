//go:build !linux && !windows

package fsstat

import (
	"fmt"
	"runtime"
)

// platformCaps is empty on platforms with no implementation. Every capability-dependent metric is
// therefore withheld rather than guessed, and the exporter still builds, runs, and reports plain
// logical sizes — which is the one measurement that needs no platform support.
const platformCaps Capability = 0

type systemFS struct{}

func (systemFS) Caps() Capability { return platformCaps }

func (systemFS) OpenDir(path string) (Dir, error) {
	return nil, unsupported("OpenDir", path)
}

func (systemFS) StatFS(path string) (FSInfo, error) {
	return FSInfo{}, unsupported("StatFS", path)
}

func (systemFS) Lstat(path string, st *FileStat) error {
	return unsupported("Lstat", path)
}

func unsupported(op, path string) error {
	return fmt.Errorf("%s %s on %s: %w", op, path, runtime.GOOS, ErrUnsupported)
}
