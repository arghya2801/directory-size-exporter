// Package fsstat is the exporter's only window onto platform filesystem APIs.
//
// It exists to keep every build-tagged file in one place. The directory walker in internal/scan
// must never import syscall or golang.org/x/sys directly; it depends on the FS interface here and
// is therefore fully testable on any platform against NewFake.
//
// The governing rule is that a measurement whose underlying capability is unavailable is NEVER
// REPORTED. It is not approximated, and it is not reported as zero. Callers must consult Caps
// before reading a capability-dependent field, because an absent metric is unambiguous while a
// wrong one is indistinguishable from a real measurement. Reporting logical size as "disk usage"
// on a platform that cannot read allocated blocks would silently understate a sparse file and
// overstate nothing — a bug that only surfaces as a wrong capacity forecast months later.
package fsstat

import (
	"errors"
	"io/fs"
	"strings"
)

// ErrUnsupported is returned by operations that the current platform cannot perform.
var ErrUnsupported = errors.New("fsstat: operation not supported on this platform")

// Capability is a bit set describing what the current platform can actually measure.
type Capability uint32

const (
	// CapAllocBytes means allocated (on-disk) size is readable, distinct from logical size.
	CapAllocBytes Capability = 1 << iota
	// CapInode means Dev, Ino and Nlink are populated, enabling hard-link dedup and the
	// one-filesystem boundary check.
	CapInode
	// CapFSBytes means filesystem total/free/available bytes are readable.
	CapFSBytes
	// CapFSInodes means filesystem inode totals are readable.
	CapFSInodes
	// CapMountpoint means the mount point containing a path can be resolved.
	CapMountpoint
)

// Has reports whether every bit in want is present in c.
func (c Capability) Has(want Capability) bool { return c&want == want }

// String renders the set as a stable, comma-separated list for logs and error messages.
func (c Capability) String() string {
	if c == 0 {
		return "none"
	}
	var names []string
	for _, known := range []struct {
		bit  Capability
		name string
	}{
		{CapAllocBytes, "alloc_bytes"},
		{CapInode, "inode"},
		{CapFSBytes, "fs_bytes"},
		{CapFSInodes, "fs_inodes"},
		{CapMountpoint, "mountpoint"},
	} {
		if c.Has(known.bit) {
			names = append(names, known.name)
		}
	}
	return strings.Join(names, ",")
}

// AllCapabilities lists every capability with its metric label, so callers can publish a complete
// capability report rather than only the bits that happen to be set.
var AllCapabilities = []struct {
	Bit   Capability
	Label string
}{
	{CapAllocBytes, "alloc_bytes"},
	{CapInode, "inode"},
	{CapFSBytes, "fs_bytes"},
	{CapFSInodes, "fs_inodes"},
	{CapMountpoint, "mountpoint"},
}

// FileStat is a reusable stat result. Callers own the struct and pass the same one for every entry
// in a directory, so walking a ten-million-file tree performs no per-entry allocation.
type FileStat struct {
	// Size is the logical file size in bytes; always valid.
	Size int64
	// AllocBytes is the space actually allocated on disk. Valid only with CapAllocBytes.
	// A sparse file reports far less here than in Size; a small file usually reports more.
	AllocBytes int64
	// Dev, Ino and Nlink are valid only with CapInode.
	Dev, Ino, Nlink uint64
	// Mode carries at least the file-type bits; always valid.
	Mode fs.FileMode
}

// IsRegular reports whether the entry is a regular file, the only kind whose size is counted.
func (s *FileStat) IsRegular() bool { return s.Mode.IsRegular() }

// IsDir reports whether the entry is a directory.
func (s *FileStat) IsDir() bool { return s.Mode.IsDir() }

// Reset zeroes the struct so a reused FileStat cannot leak a previous entry's values into a
// partially-filled result.
func (s *FileStat) Reset() { *s = FileStat{} }

// Entry is one directory entry as returned by ReadSome. Name is a bare name, never a path.
type Entry struct {
	Name string
	// Type holds only the file-type bits of the mode, and may be zero on filesystems that do not
	// report a type from the directory read. Callers that need certainty must stat the entry.
	Type fs.FileMode
}

// IsDir reports whether the directory read already identified this entry as a directory.
func (e Entry) IsDir() bool { return e.Type.IsDir() }

// IsSymlink reports whether the directory read already identified this entry as a symbolic link.
func (e Entry) IsSymlink() bool { return e.Type&fs.ModeSymlink != 0 }

// FSInfo describes the filesystem containing a path.
//
// There is deliberately no device-node name here. Resolving one requires parsing /proc mount
// tables, which races with concurrent mounts and reports the host's view rather than the
// container's. Device is instead the kernel's device identifier, which is always correct and
// always available. Mountpoint is the intended join key between a target and its filesystem.
type FSInfo struct {
	TotalBytes, FreeBytes, AvailBytes uint64
	// TotalInodes and FreeInodes are valid only with CapFSInodes.
	TotalInodes, FreeInodes uint64
	// Mountpoint is valid only with CapMountpoint; otherwise it is empty.
	Mountpoint string
	// Device identifies the underlying device ("major:minor" on Linux, the volume root on
	// Windows). Empty when the platform cannot determine it.
	Device string
	// FSType is the filesystem type name, or a hex magic number when unrecognised. Empty when the
	// platform cannot determine it.
	FSType string
}

// Dir is an open directory handle.
//
// Implementations hold a descriptor for the lifetime of the handle so that entries can be stat'ed
// by bare name relative to it. That is what keeps per-entry cost at one syscall with no path
// construction: resolving "/a/b/c/d/file.log" makes the kernel walk four directory entries, while
// resolving "file.log" against an already-open descriptor walks one.
type Dir interface {
	// ReadSome returns up to n entries in directory order. It NEVER sorts: sorting a directory
	// holding two million log files costs O(n log n) CPU and materialises the whole slice before
	// any useful work begins.
	//
	// The returned slice is owned by the Dir and is invalidated by the next call to ReadSome or
	// Close. Callers must not retain it.
	//
	// It returns io.EOF once the directory is exhausted.
	ReadSome(n int) ([]Entry, error)

	// StatEntry stats a child by BARE NAME, relative to this directory. Passing a name containing
	// a path separator is a programming error. st is caller-owned and overwritten in place.
	StatEntry(name string, st *FileStat) error

	// Path returns the directory's path, for log and error messages only.
	Path() string

	// Close releases the handle. It is safe to call more than once.
	Close() error
}

// FS is the seam between the walker and the operating system. System returns the real
// implementation; NewFake returns an in-memory one for tests.
type FS interface {
	// OpenDir opens a directory without following a symbolic link in its final component, so a
	// directory swapped for a symlink mid-walk is refused rather than silently followed outside
	// the tree being measured.
	OpenDir(path string) (Dir, error)
	// StatFS describes the filesystem containing path.
	StatFS(path string) (FSInfo, error)
	// Lstat stats a path without following a final symbolic link.
	Lstat(path string, st *FileStat) error
	// Caps reports what this implementation can measure.
	Caps() Capability
}

// Compile-time proof that the build-tagged file for this GOOS defines the whole surface. Without
// these, a platform missing a stub fails at link time with a message that does not name the
// platform, or worse compiles on the developer's machine and breaks only in production.
var (
	_ FS         = systemFS{}
	_ Capability = platformCaps
)

// System returns the FS backed by real filesystem syscalls.
func System() FS { return systemFS{} }

// Caps reports the capabilities of the real filesystem implementation. It is a package-level
// convenience for startup validation, which runs before any FS is constructed.
func Caps() Capability { return platformCaps }
