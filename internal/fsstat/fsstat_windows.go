//go:build windows

package fsstat

import (
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"golang.org/x/sys/windows"
)

// platformCaps deliberately omits CapAllocBytes and CapInode.
//
// NTFS can supply both — GetCompressedFileSize gives allocated size, and
// GetFileInformationByHandle gives a volume serial, file index and link count. Every one of them
// requires opening a HANDLE per file. At ten million files that cost is prohibitive, and the
// production platform is Linux, so the Windows build exists for development parity rather than
// for scanning real workloads.
//
// The consequence is enforced elsewhere: because CapAllocBytes is absent, disk-usage metrics are
// not emitted here at all. They are never approximated from logical size, which would silently
// misreport sparse and compressed files as if they occupied their full length.
const platformCaps = CapFSBytes | CapMountpoint

type systemFS struct{}

func (systemFS) Caps() Capability { return platformCaps }

func (systemFS) OpenDir(path string) (Dir, error) {
	// Windows has no O_NOFOLLOW. Checking for a reparse point first leaves a small window in which
	// the directory could be swapped, which is acceptable for a development-parity build.
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if info.Mode()&fs.ModeSymlink != 0 {
		return nil, &fs.PathError{Op: "opendir", Path: path, Err: windows.ERROR_CANT_ACCESS_FILE}
	}
	if !info.IsDir() {
		return nil, &fs.PathError{Op: "opendir", Path: path, Err: windows.ERROR_DIRECTORY}
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	return &windowsDir{file: file, path: path}, nil
}

func (systemFS) Lstat(path string, st *FileStat) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	fillFromFileInfo(st, info)
	return nil
}

type windowsDir struct {
	file *os.File
	path string
	buf  []Entry
}

func (d *windowsDir) Path() string { return d.path }

func (d *windowsDir) Close() error {
	if d.file == nil {
		return nil
	}
	err := d.file.Close()
	d.file = nil
	return err
}

func (d *windowsDir) ReadSome(n int) ([]Entry, error) {
	if d.file == nil {
		return nil, fs.ErrClosed
	}
	entries, err := d.file.ReadDir(n)
	if len(entries) == 0 {
		if err == nil {
			err = io.EOF
		}
		return nil, err
	}
	if cap(d.buf) < len(entries) {
		d.buf = make([]Entry, len(entries))
	}
	d.buf = d.buf[:len(entries)]
	for i, entry := range entries {
		d.buf[i] = Entry{Name: entry.Name(), Type: entry.Type()}
	}
	return d.buf, err
}

// StatEntry must still join the path, because Windows has no fstatat equivalent reachable through
// the standard library. Only Size and Mode are populated; AllocBytes, Dev, Ino and Nlink stay zero
// and callers are forbidden from reading them because platformCaps withholds the matching bits.
func (d *windowsDir) StatEntry(name string, st *FileStat) error {
	if d.file == nil {
		return fs.ErrClosed
	}
	info, err := os.Lstat(filepath.Join(d.path, name))
	if err != nil {
		return err
	}
	fillFromFileInfo(st, info)
	return nil
}

func fillFromFileInfo(st *FileStat, info fs.FileInfo) {
	st.Reset()
	st.Size = info.Size()
	st.Mode = info.Mode()
}

func (systemFS) StatFS(path string) (FSInfo, error) {
	pathPtr, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return FSInfo{}, err
	}
	var availToCaller, total, free uint64
	if err := windows.GetDiskFreeSpaceEx(pathPtr, &availToCaller, &total, &free); err != nil {
		return FSInfo{}, &fs.PathError{Op: "GetDiskFreeSpaceEx", Path: path, Err: err}
	}
	info := FSInfo{TotalBytes: total, FreeBytes: free, AvailBytes: availToCaller}

	// The volume root is the closest Windows analogue to a mount point, and doubles as the device
	// identifier. Inode counts have no equivalent, so CapFSInodes stays unset.
	volume := make([]uint16, windows.MAX_PATH+1)
	if err := windows.GetVolumePathName(pathPtr, &volume[0], uint32(len(volume))); err == nil {
		info.Mountpoint = windows.UTF16ToString(volume)
		info.Device = info.Mountpoint
		info.FSType = volumeFSType(&volume[0])
	}
	return info, nil
}

func volumeFSType(volumeRoot *uint16) string {
	name := make([]uint16, windows.MAX_PATH+1)
	err := windows.GetVolumeInformation(volumeRoot, nil, 0, nil, nil, nil, &name[0], uint32(len(name)))
	if err != nil {
		return ""
	}
	return windows.UTF16ToString(name)
}
