//go:build linux

package fsstat

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// platformCaps: Linux supports every capability. This is the production platform.
const platformCaps = CapAllocBytes | CapInode | CapFSBytes | CapFSInodes | CapMountpoint

// statBlockSize is the unit of st_blocks. POSIX fixes it at 512 bytes REGARDLESS of st_blksize,
// which is a reporting hint and is commonly 4096. Multiplying by st_blksize instead is a classic
// bug that inflates reported disk usage eightfold on a typical ext4 volume.
const statBlockSize = 512

type systemFS struct{}

func (systemFS) Caps() Capability { return platformCaps }

func (systemFS) OpenDir(path string) (Dir, error) {
	// O_NOFOLLOW refuses a final component that is a symbolic link, so a directory replaced by a
	// symlink between being listed and being opened is rejected (ELOOP) rather than silently
	// followed out of the tree being measured. O_CLOEXEC keeps descriptors out of child processes.
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, &fs.PathError{Op: "openat", Path: path, Err: err}
	}
	return &linuxDir{file: os.NewFile(uintptr(fd), path), fd: fd, path: path}, nil
}

func (systemFS) Lstat(path string, st *FileStat) error {
	var raw unix.Stat_t
	if err := unix.Lstat(path, &raw); err != nil {
		return &fs.PathError{Op: "lstat", Path: path, Err: err}
	}
	fillFileStat(st, &raw)
	return nil
}

type linuxDir struct {
	file *os.File
	fd   int
	path string

	// raw and buf are reused across the whole directory so that per-entry work allocates nothing.
	raw unix.Stat_t
	buf []Entry
}

func (d *linuxDir) Path() string { return d.path }

func (d *linuxDir) Close() error {
	if d.file == nil {
		return nil
	}
	err := d.file.Close()
	d.file = nil
	return err
}

func (d *linuxDir) ReadSome(n int) ([]Entry, error) {
	if d.file == nil {
		return nil, fs.ErrClosed
	}
	// ReadDir with a positive n reads incrementally and, unlike os.ReadDir, does not sort.
	//
	// This still allocates one os.DirEntry and one name string per entry inside the standard
	// library. Eliminating that would mean parsing the getdents64 buffer directly; the remaining
	// per-entry cost is small next to the stat syscall, so it is deliberately left alone.
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

func (d *linuxDir) StatEntry(name string, st *FileStat) error {
	if d.file == nil {
		return fs.ErrClosed
	}
	// Relative to the open descriptor, so the kernel resolves one component instead of walking the
	// full path from the root for every file in the tree.
	if err := unix.Fstatat(d.fd, name, &d.raw, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return &fs.PathError{Op: "fstatat", Path: filepath.Join(d.path, name), Err: err}
	}
	fillFileStat(st, &d.raw)
	return nil
}

func fillFileStat(st *FileStat, raw *unix.Stat_t) {
	st.Size = int64(raw.Size)
	st.AllocBytes = int64(raw.Blocks) * statBlockSize
	st.Dev = uint64(raw.Dev)
	st.Ino = uint64(raw.Ino)
	st.Nlink = uint64(raw.Nlink)
	st.Mode = fileModeFromSys(uint32(raw.Mode))
}

// fileModeFromSys converts st_mode into an fs.FileMode. The standard library does this internally
// but does not export it, and os.Lstat is exactly the full-path resolution being avoided here.
func fileModeFromSys(mode uint32) fs.FileMode {
	result := fs.FileMode(mode & 0o777)
	switch mode & unix.S_IFMT {
	case unix.S_IFBLK:
		result |= fs.ModeDevice
	case unix.S_IFCHR:
		result |= fs.ModeDevice | fs.ModeCharDevice
	case unix.S_IFDIR:
		result |= fs.ModeDir
	case unix.S_IFIFO:
		result |= fs.ModeNamedPipe
	case unix.S_IFLNK:
		result |= fs.ModeSymlink
	case unix.S_IFSOCK:
		result |= fs.ModeSocket
	case unix.S_IFREG:
		// Regular files carry no type bit.
	}
	if mode&unix.S_ISGID != 0 {
		result |= fs.ModeSetgid
	}
	if mode&unix.S_ISUID != 0 {
		result |= fs.ModeSetuid
	}
	if mode&unix.S_ISVTX != 0 {
		result |= fs.ModeSticky
	}
	return result
}

func (systemFS) StatFS(path string) (FSInfo, error) {
	var sfs unix.Statfs_t
	if err := unix.Statfs(path, &sfs); err != nil {
		return FSInfo{}, &fs.PathError{Op: "statfs", Path: path, Err: err}
	}
	// Bsize is the preferred I/O block size and is what the block counts are expressed in. Frsize
	// is the fragment size, used as a fallback on filesystems that leave Bsize unset.
	blockSize := int64(sfs.Bsize)
	if blockSize <= 0 {
		blockSize = int64(sfs.Frsize)
	}
	if blockSize <= 0 {
		return FSInfo{}, fmt.Errorf("statfs %s: filesystem reported no usable block size", path)
	}

	info := FSInfo{
		TotalBytes:  uint64(sfs.Blocks) * uint64(blockSize),
		FreeBytes:   uint64(sfs.Bfree) * uint64(blockSize),
		AvailBytes:  uint64(sfs.Bavail) * uint64(blockSize),
		TotalInodes: uint64(sfs.Files),
		FreeInodes:  uint64(sfs.Ffree),
		FSType:      fsTypeName(int64(sfs.Type)),
	}
	if mountpoint, dev, err := mountpointOf(path); err == nil {
		info.Mountpoint = mountpoint
		info.Device = fmt.Sprintf("%d:%d", unix.Major(dev), unix.Minor(dev))
	}
	return info, nil
}

// mountpointOf finds the mount point containing path by walking up until the device identifier
// changes. Every ancestor of a mount point lives on a different device by definition.
//
// This uses only stat syscalls. Parsing /proc/self/mountinfo would also work but races with
// concurrent mount activity and, inside a container, describes the host's mount tree rather than
// the one the process actually sees.
func mountpointOf(path string) (string, uint64, error) {
	var st unix.Stat_t
	if err := unix.Stat(path, &st); err != nil {
		return "", 0, &fs.PathError{Op: "stat", Path: path, Err: err}
	}
	dev := uint64(st.Dev)
	current := filepath.Clean(path)
	for {
		parent := filepath.Dir(current)
		if parent == current {
			// Reached the filesystem root without the device changing.
			return current, dev, nil
		}
		var parentStat unix.Stat_t
		if err := unix.Stat(parent, &parentStat); err != nil {
			// An unreadable parent means the boundary cannot be proven any higher up; the deepest
			// path confirmed to be on this device is the best available answer.
			return current, dev, nil
		}
		if uint64(parentStat.Dev) != dev {
			return current, dev, nil
		}
		current = parent
	}
}

// fsTypeMagic covers the filesystem types a log-bearing host realistically uses. Anything else is
// reported as its hex magic number, which is still a usable label and is honest about being
// unrecognised.
var fsTypeMagic = map[int64]string{
	0xadf5:     "adfs",
	0x9123683e: "btrfs",
	0x00c36400: "ceph",
	0xff534d42: "cifs",
	0x0000f15f: "ecryptfs",
	0x0000ef53: "ext2/ext3/ext4",
	0x00004006: "fat",
	0x01161970: "gfs2",
	0x00006969: "nfs",
	0x00003434: "nilfs",
	0x7461636F: "ocfs2",
	0x794c7630: "overlayfs",
	0x52654973: "reiserfs",
	0x73717368: "squashfs",
	0x01021994: "tmpfs",
	0x01021997: "v9fs",
	0x58465342: "xfs",
	0x2fc12fc1: "zfs",
	0x9fa0:     "proc",
	0x62656572: "sysfs",
	0x64626720: "debugfs",
	0x01021995: "ramfs",
	0x6969696d: "fuse",
}

func fsTypeName(magic int64) string {
	if name, ok := fsTypeMagic[magic]; ok {
		return name
	}
	return "0x" + strings.ToLower(fmt.Sprintf("%x", uint64(magic)))
}
