package fsstat

import (
	"fmt"
	"hash/fnv"
	"io"
	"io/fs"
	"math/rand"
	"path"
	"sort"
	"sync"
)

// FakeFS is an in-memory FS for tests.
//
// It lives in the production package rather than a _test.go file because the walker in
// internal/scan is tested against it, and because it is the only way to exercise conditions that
// are impractical to stage on a real filesystem: a ten-million-entry directory, a read that blocks
// forever, a file that vanishes between being listed and being stat'ed, or a permission error on a
// specific subtree. It also lets the walker's behaviour be verified on Windows, where the
// production platform's capabilities do not exist.
//
// All methods are safe for concurrent use, because the walker it serves is concurrent.
type FakeFS struct {
	mu    sync.Mutex
	caps  Capability
	nodes map[string]*fakeNode
	infos map[string]FSInfo

	openErrs map[string]error
	statErrs map[string]error

	openDirs    int
	maxOpenDirs int
	statCalls   int64
	readCalls   int64
	// nonBareStatNames records any name passed to StatEntry that contains a path separator. The
	// whole point of the Dir interface is that entries are stat'ed relative to an open directory;
	// a joined path here means that optimisation has been silently lost.
	nonBareStatNames []string

	beforeOpen func(path string) error
	beforeRead func(path string) error
	beforeStat func(dir, name string) error

	shuffleSeed int64
	shuffle     bool
}

type fakeNode struct {
	stat     FileStat
	children []string
	// synthetic generates that many file entries on demand instead of storing their names, so a
	// directory with millions of entries costs nothing to construct.
	synthetic     int
	syntheticSize int64
}

const syntheticNameFormat = "synthetic-%08d"

// NewFake returns an empty FakeFS advertising the given capabilities. Passing a reduced set is how
// platform-degradation behaviour is tested from any machine.
func NewFake(caps Capability) *FakeFS {
	f := &FakeFS{
		caps:     caps,
		nodes:    make(map[string]*fakeNode),
		infos:    make(map[string]FSInfo),
		openErrs: make(map[string]error),
		statErrs: make(map[string]error),
	}
	f.nodes["/"] = &fakeNode{stat: FileStat{Mode: fs.ModeDir | 0o755}}
	return f
}

func (f *FakeFS) Caps() Capability { return f.caps }

// --- construction -------------------------------------------------------------------------

// AddDir adds a directory, creating any missing ancestors.
func (f *FakeFS) AddDir(p string) *FakeFS {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.addNodeLocked(p, &fakeNode{stat: FileStat{Mode: fs.ModeDir | 0o755}})
	return f
}

// AddFile adds a regular file of the given logical size. Allocated size is rounded up to a 512-byte
// boundary so that tests exercising disk usage see a value that differs from the logical size in
// the same direction a real filesystem would.
func (f *FakeFS) AddFile(p string, size int64) *FakeFS {
	alloc := (size + statBlockSizeFake - 1) / statBlockSizeFake * statBlockSizeFake
	return f.AddFileStat(p, FileStat{Size: size, AllocBytes: alloc, Mode: 0o644, Nlink: 1})
}

const statBlockSizeFake = 512

// AddFileStat adds a file with a fully specified stat result, for tests that need particular
// inode, link-count or allocation values.
func (f *FakeFS) AddFileStat(p string, st FileStat) *FakeFS {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.addNodeLocked(p, &fakeNode{stat: st})
	return f
}

// AddSymlink adds a symbolic link, which the walker must never follow or count.
func (f *FakeFS) AddSymlink(p string) *FakeFS {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.addNodeLocked(p, &fakeNode{stat: FileStat{Mode: fs.ModeSymlink | 0o777}})
	return f
}

// AddSyntheticFiles makes dir report count generated files of the given size, without storing a
// name per file. This is what makes a ten-million-entry cancellation test cheap.
func (f *FakeFS) AddSyntheticFiles(dir string, count int, size int64) *FakeFS {
	f.mu.Lock()
	defer f.mu.Unlock()
	node := f.nodes[clean(dir)]
	if node == nil {
		node = &fakeNode{stat: FileStat{Mode: fs.ModeDir | 0o755}}
		f.addNodeLocked(dir, node)
	}
	node.synthetic = count
	node.syntheticSize = size
	return f
}

// addNodeLocked inserts a node and links it into its parent, creating ancestors as directories.
// It walks all the way to the root so that a tree can be built by naming leaves alone.
func (f *FakeFS) addNodeLocked(p string, node *fakeNode) {
	p = clean(p)
	f.nodes[p] = node
	for child := p; ; {
		parent := path.Dir(child)
		if parent == child { // reached the root
			return
		}
		parentNode := f.nodes[parent]
		if parentNode == nil {
			parentNode = &fakeNode{stat: FileStat{Mode: fs.ModeDir | 0o755}}
			f.nodes[parent] = parentNode
		}
		if name := path.Base(child); !contains(parentNode.children, name) {
			parentNode.children = append(parentNode.children, name)
		}
		child = parent
	}
}

// SetOpenError makes OpenDir fail for a path, modelling a permission-denied or vanished subtree.
func (f *FakeFS) SetOpenError(p string, err error) *FakeFS {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.openErrs[clean(p)] = err
	return f
}

// SetStatError makes StatEntry fail for a path. Using fs.ErrNotExist here models a file that was
// rotated away between being listed and being stat'ed, which must not count as an error.
func (f *FakeFS) SetStatError(p string, err error) *FakeFS {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.statErrs[clean(p)] = err
	return f
}

// SetFSInfo sets the filesystem description returned for a path.
func (f *FakeFS) SetFSInfo(p string, info FSInfo) *FakeFS {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.infos[clean(p)] = info
	return f
}

// ShuffleReads makes directory reads return entries in a deterministic but non-sorted order, so a
// test can prove the walker does not depend on ordering the way the old sorted implementation did.
func (f *FakeFS) ShuffleReads(seed int64) *FakeFS {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.shuffle, f.shuffleSeed = true, seed
	return f
}

// BeforeOpen, BeforeRead and BeforeStat install hooks that run before the corresponding operation.
// Returning an error fails the operation; blocking inside one models a hung mount, on which the
// real syscalls are uninterruptible.
func (f *FakeFS) BeforeOpen(fn func(path string) error) *FakeFS {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.beforeOpen = fn
	return f
}

func (f *FakeFS) BeforeRead(fn func(path string) error) *FakeFS {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.beforeRead = fn
	return f
}

func (f *FakeFS) BeforeStat(fn func(dir, name string) error) *FakeFS {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.beforeStat = fn
	return f
}

// --- observation --------------------------------------------------------------------------

// StatCalls reports how many times an entry was stat'ed, the dominant per-file cost of a real scan.
func (f *FakeFS) StatCalls() int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.statCalls
}

// ReadCalls reports how many directory read batches were issued.
func (f *FakeFS) ReadCalls() int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.readCalls
}

// MaxOpenDirs reports the high-water mark of simultaneously open directories. The walker opens,
// drains and closes each directory before descending, so this must stay at or below the configured
// concurrency no matter how deep the tree is.
func (f *FakeFS) MaxOpenDirs() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.maxOpenDirs
}

// OpenDirs reports how many directories are open right now.
func (f *FakeFS) OpenDirs() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.openDirs
}

// NonBareStatNames returns every name passed to StatEntry that contained a path separator.
// A non-empty result means the walker built a full path per file instead of stat'ing relative to
// the open directory.
func (f *FakeFS) NonBareStatNames() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.nonBareStatNames...)
}

// --- FS implementation --------------------------------------------------------------------

func (f *FakeFS) OpenDir(p string) (Dir, error) {
	p = clean(p)
	f.mu.Lock()
	hook := f.beforeOpen
	f.mu.Unlock()
	if hook != nil {
		if err := hook(p); err != nil {
			return nil, err
		}
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.openErrs[p]; err != nil {
		return nil, &fs.PathError{Op: "opendir", Path: p, Err: err}
	}
	node := f.nodes[p]
	if node == nil {
		return nil, &fs.PathError{Op: "opendir", Path: p, Err: fs.ErrNotExist}
	}
	if node.stat.Mode&fs.ModeSymlink != 0 {
		return nil, &fs.PathError{Op: "opendir", Path: p, Err: fs.ErrInvalid}
	}
	if !node.stat.Mode.IsDir() {
		return nil, &fs.PathError{Op: "opendir", Path: p, Err: fs.ErrInvalid}
	}

	names := append([]string(nil), node.children...)
	if f.shuffle {
		// Seeded from the path so the order is deterministic per directory across runs, while
		// still differing from the sorted order the old implementation produced.
		hasher := fnv.New64a()
		_, _ = hasher.Write([]byte(p))
		source := rand.New(rand.NewSource(f.shuffleSeed ^ int64(hasher.Sum64())))
		source.Shuffle(len(names), func(i, j int) { names[i], names[j] = names[j], names[i] })
	} else {
		sort.Strings(names)
	}

	f.openDirs++
	if f.openDirs > f.maxOpenDirs {
		f.maxOpenDirs = f.openDirs
	}
	return &fakeDir{fs: f, path: p, names: names, synthetic: node.synthetic}, nil
}

func (f *FakeFS) Lstat(p string, st *FileStat) error {
	p = clean(p)
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.statErrs[p]; err != nil {
		return &fs.PathError{Op: "lstat", Path: p, Err: err}
	}
	node := f.nodes[p]
	if node == nil {
		return &fs.PathError{Op: "lstat", Path: p, Err: fs.ErrNotExist}
	}
	*st = node.stat
	return nil
}

func (f *FakeFS) StatFS(p string) (FSInfo, error) {
	p = clean(p)
	f.mu.Lock()
	defer f.mu.Unlock()
	// Walk up to the nearest configured filesystem, mirroring how a real mount point covers every
	// path beneath it.
	for current := p; ; {
		if info, ok := f.infos[current]; ok {
			return info, nil
		}
		parent := path.Dir(current)
		if parent == current {
			break
		}
		current = parent
	}
	return FSInfo{}, &fs.PathError{Op: "statfs", Path: p, Err: fs.ErrNotExist}
}

// --- fake directory handle ------------------------------------------------------------------

type fakeDir struct {
	fs        *FakeFS
	path      string
	names     []string
	synthetic int

	cursor int
	buf    []Entry
	closed bool
}

func (d *fakeDir) Path() string { return d.path }

func (d *fakeDir) Close() error {
	if d.closed {
		return nil
	}
	d.closed = true
	d.fs.mu.Lock()
	d.fs.openDirs--
	d.fs.mu.Unlock()
	return nil
}

func (d *fakeDir) ReadSome(n int) ([]Entry, error) {
	if d.closed {
		return nil, fs.ErrClosed
	}
	d.fs.mu.Lock()
	hook := d.fs.beforeRead
	d.fs.readCalls++
	d.fs.mu.Unlock()
	if hook != nil {
		if err := hook(d.path); err != nil {
			return nil, err
		}
	}

	total := len(d.names) + d.synthetic
	if d.cursor >= total {
		return nil, io.EOF
	}
	end := d.cursor + n
	if end > total {
		end = total
	}
	d.buf = d.buf[:0]
	for i := d.cursor; i < end; i++ {
		if i < len(d.names) {
			d.buf = append(d.buf, d.entryFor(d.names[i]))
			continue
		}
		d.buf = append(d.buf, Entry{Name: fmt.Sprintf(syntheticNameFormat, i-len(d.names))})
	}
	d.cursor = end
	return d.buf, nil
}

func (d *fakeDir) entryFor(name string) Entry {
	d.fs.mu.Lock()
	defer d.fs.mu.Unlock()
	node := d.fs.nodes[path.Join(d.path, name)]
	if node == nil {
		return Entry{Name: name}
	}
	return Entry{Name: name, Type: node.stat.Mode.Type()}
}

func (d *fakeDir) StatEntry(name string, st *FileStat) error {
	if d.closed {
		return fs.ErrClosed
	}
	d.fs.mu.Lock()
	hook := d.fs.beforeStat
	d.fs.statCalls++
	if containsSeparator(name) {
		d.fs.nonBareStatNames = append(d.fs.nonBareStatNames, name)
	}
	d.fs.mu.Unlock()
	if hook != nil {
		if err := hook(d.path, name); err != nil {
			return err
		}
	}

	full := path.Join(d.path, name)
	d.fs.mu.Lock()
	defer d.fs.mu.Unlock()
	if err := d.fs.statErrs[full]; err != nil {
		return &fs.PathError{Op: "fstatat", Path: full, Err: err}
	}
	if node := d.fs.nodes[full]; node != nil {
		*st = node.stat
		return nil
	}
	// A synthetic entry has no stored node; synthesise its stat from the parent's configuration.
	if parent := d.fs.nodes[d.path]; parent != nil && parent.synthetic > 0 {
		size := parent.syntheticSize
		*st = FileStat{
			Size:       size,
			AllocBytes: (size + statBlockSizeFake - 1) / statBlockSizeFake * statBlockSizeFake,
			Mode:       0o644,
			Nlink:      1,
		}
		return nil
	}
	return &fs.PathError{Op: "fstatat", Path: full, Err: fs.ErrNotExist}
}

// --- helpers ------------------------------------------------------------------------------

// clean normalises to forward-slash form so tests read identically on Windows and Linux.
func clean(p string) string {
	p = path.Clean(replaceSeparators(p))
	if p == "." {
		return "/"
	}
	return p
}

func replaceSeparators(p string) string {
	out := []byte(p)
	for i := range out {
		if out[i] == '\\' {
			out[i] = '/'
		}
	}
	return string(out)
}

func containsSeparator(name string) bool {
	for i := 0; i < len(name); i++ {
		if name[i] == '/' || name[i] == '\\' {
			return true
		}
	}
	return false
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
