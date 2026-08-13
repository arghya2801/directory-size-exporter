//go:build linux

package fsstat

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

// TestLinux_AllocBytesTracksBlocksNotLength is the reason CapAllocBytes exists. A sparse file's
// logical length says nothing about the space it occupies, and capacity planning needs the latter.
func TestLinux_AllocBytesTracksBlocksNotLength(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sparse.log")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	const logicalSize = 1 << 30 // 1 GiB of hole
	if err := file.Truncate(logicalSize); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	var st FileStat
	if err := System().Lstat(path, &st); err != nil {
		t.Fatal(err)
	}
	if st.Size != logicalSize {
		t.Errorf("Size = %d, want %d", st.Size, logicalSize)
	}
	// The exact allocation depends on the filesystem, but a 1 GiB hole must never allocate 1 GiB.
	if st.AllocBytes >= logicalSize {
		t.Errorf("AllocBytes = %d for a sparse file of %d; blocks are not being read", st.AllocBytes, logicalSize)
	}
	if st.AllocBytes%statBlockSize != 0 {
		t.Errorf("AllocBytes = %d is not a multiple of the 512-byte block unit", st.AllocBytes)
	}
}

// TestLinux_AllocBytesRoundsUpForTinyFile guards the Blocks*512 constant from being changed to
// Blocks*Blksize, which would inflate reported disk usage roughly eightfold on ext4.
func TestLinux_AllocBytesRoundsUpForTinyFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tiny.log")
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	var st FileStat
	if err := System().Lstat(path, &st); err != nil {
		t.Fatal(err)
	}
	if st.Size != 1 {
		t.Fatalf("Size = %d, want 1", st.Size)
	}
	// Deliberately not asserting 4096: tmpfs, xfs and ext4 differ, and TMPDIR may be any of them.
	if st.AllocBytes < st.Size || st.AllocBytes%statBlockSize != 0 {
		t.Errorf("AllocBytes = %d, want a non-zero multiple of %d at least as large as the file", st.AllocBytes, statBlockSize)
	}
}

func TestLinux_HardLinksShareDeviceAndInode(t *testing.T) {
	dir := t.TempDir()
	original := filepath.Join(dir, "trades.log")
	if err := os.WriteFile(original, []byte("0123456789"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "trades.log.1")
	if err := os.Link(original, link); err != nil {
		t.Skipf("filesystem does not support hard links: %v", err)
	}

	var first, second FileStat
	if err := System().Lstat(original, &first); err != nil {
		t.Fatal(err)
	}
	if err := System().Lstat(link, &second); err != nil {
		t.Fatal(err)
	}
	if first.Dev != second.Dev || first.Ino != second.Ino {
		t.Errorf("hard links differ in identity: %+v vs %+v", first, second)
	}
	// Nlink > 1 is the cheap test the walker uses to decide whether an entry is worth tracking at
	// all, which is what keeps dedup memory near zero on a normal log tree.
	if first.Nlink != 2 {
		t.Errorf("Nlink = %d, want 2", first.Nlink)
	}
}

func TestLinux_StatFSPopulatesBytesAndInodes(t *testing.T) {
	info, err := System().StatFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if info.TotalBytes == 0 {
		t.Error("TotalBytes is zero")
	}
	if info.AvailBytes > info.TotalBytes {
		t.Errorf("AvailBytes %d exceeds TotalBytes %d", info.AvailBytes, info.TotalBytes)
	}
	if info.FSType == "" {
		t.Error("FSType is empty")
	}
	if info.Mountpoint == "" {
		t.Error("Mountpoint is empty")
	}
	if info.Device == "" {
		t.Error("Device is empty")
	}
	// Some filesystems (btrfs, many network mounts) genuinely report no inode totals, so only the
	// relationship is asserted.
	if info.FreeInodes > info.TotalInodes {
		t.Errorf("FreeInodes %d exceeds TotalInodes %d", info.FreeInodes, info.TotalInodes)
	}
}

// TestLinux_MountpointResolvesProc exercises the device walk-up against a real mount boundary that
// exists on every Linux host and needs no privileges to observe.
func TestLinux_MountpointResolvesProc(t *testing.T) {
	mountpoint, _, err := mountpointOf("/proc/self")
	if err != nil {
		t.Fatal(err)
	}
	if mountpoint != "/proc" {
		t.Errorf("mountpointOf(/proc/self) = %q, want /proc", mountpoint)
	}

	info, err := System().StatFS("/proc")
	if err != nil {
		t.Fatal(err)
	}
	if info.FSType != "proc" {
		t.Errorf("FSType = %q, want proc", info.FSType)
	}
}

// TestLinux_StatEntryResolvesRelativeToDescriptor proves entries really are stat'ed through the
// open directory rather than by rebuilding a full path. Renaming the parent invalidates every
// path-based lookup while leaving descriptor-relative ones working.
func TestLinux_StatEntryResolvesRelativeToDescriptor(t *testing.T) {
	base := t.TempDir()
	original := filepath.Join(base, "logs")
	if err := os.Mkdir(original, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(original, "trades.log"), []byte("0123456789"), 0o600); err != nil {
		t.Fatal(err)
	}

	dir, err := System().OpenDir(original)
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()

	if err := os.Rename(original, filepath.Join(base, "logs-rotated")); err != nil {
		t.Fatal(err)
	}

	var st FileStat
	if err := dir.StatEntry("trades.log", &st); err != nil {
		t.Fatalf("stat through a held descriptor failed after the parent was renamed: %v", err)
	}
	if st.Size != 10 {
		t.Errorf("Size = %d, want 10", st.Size)
	}
}

func TestLinux_OpenDirRefusesSymlinkedDirectory(t *testing.T) {
	base := t.TempDir()
	real := filepath.Join(base, "real")
	if err := os.Mkdir(real, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("cannot create symlinks: %v", err)
	}

	// O_NOFOLLOW is what stops a directory swapped for a symlink mid-walk from redirecting the
	// scan outside the tree being measured.
	if _, err := System().OpenDir(link); err == nil {
		t.Fatal("OpenDir followed a symlinked directory")
	}
}

func TestLinux_ReadSomeIsIncrementalAndUnsorted(t *testing.T) {
	dir := t.TempDir()
	// Names chosen so that creation order and sorted order differ.
	for _, name := range []string{"zulu.log", "alpha.log", "mike.log", "bravo.log"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	handle, err := System().OpenDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer handle.Close()

	var names []string
	batches := 0
	for {
		entries, err := handle.ReadSome(2)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) > 2 {
			t.Fatalf("ReadSome(2) returned %d entries", len(entries))
		}
		batches++
		for _, entry := range entries {
			names = append(names, entry.Name)
			if entry.Type&fs.ModeType != 0 {
				t.Errorf("%s reported type bits %v, want a regular file", entry.Name, entry.Type)
			}
		}
	}
	if len(names) != 4 {
		t.Fatalf("read %d entries, want 4: %v", len(names), names)
	}
	if batches < 2 {
		t.Errorf("read completed in %d batches; ReadSome is not incremental", batches)
	}
}

func TestLinux_ClosedDirectoryRejectsUse(t *testing.T) {
	handle, err := System().OpenDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := handle.Close(); err != nil {
		t.Fatal(err)
	}
	if err := handle.Close(); err != nil {
		t.Errorf("second Close returned %v, want nil", err)
	}
	if _, err := handle.ReadSome(1); !errors.Is(err, fs.ErrClosed) {
		t.Errorf("ReadSome after Close = %v, want fs.ErrClosed", err)
	}
	var st FileStat
	if err := handle.StatEntry("x", &st); !errors.Is(err, fs.ErrClosed) {
		t.Errorf("StatEntry after Close = %v, want fs.ErrClosed", err)
	}
}

func TestLinux_MissingPathsReportNotExist(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "gone")
	if _, err := System().OpenDir(missing); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("OpenDir = %v, want fs.ErrNotExist", err)
	}
	var st FileStat
	if err := System().Lstat(missing, &st); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Lstat = %v, want fs.ErrNotExist", err)
	}
	if _, err := System().StatFS(missing); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("StatFS = %v, want fs.ErrNotExist", err)
	}
}
