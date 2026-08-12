package fsstat

import (
	"errors"
	"io"
	"io/fs"
	"runtime"
	"sync"
	"testing"
)

func TestCapabilityHasAndString(t *testing.T) {
	set := CapAllocBytes | CapFSBytes
	if !set.Has(CapAllocBytes) || !set.Has(CapAllocBytes|CapFSBytes) {
		t.Error("Has failed for present capabilities")
	}
	if set.Has(CapInode) || set.Has(CapAllocBytes|CapInode) {
		t.Error("Has succeeded for an absent capability")
	}
	if got, want := set.String(), "alloc_bytes,fs_bytes"; got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
	if got := Capability(0).String(); got != "none" {
		t.Errorf("empty String() = %q, want \"none\"", got)
	}
}

// TestCapabilities_MatchPlatform pins the degradation policy. If a capability is ever claimed on a
// platform that cannot honour it, a metric starts reporting a plausible but wrong number, which is
// far harder to notice than a metric that is simply absent.
func TestCapabilities_MatchPlatform(t *testing.T) {
	caps := Caps()
	if caps != System().Caps() {
		t.Fatal("package-level Caps disagrees with the FS implementation")
	}
	switch runtime.GOOS {
	case "linux":
		for _, want := range []Capability{CapAllocBytes, CapInode, CapFSBytes, CapFSInodes, CapMountpoint} {
			if !caps.Has(want) {
				t.Errorf("linux is missing capability %v", want)
			}
		}
	case "windows":
		if !caps.Has(CapFSBytes) {
			t.Error("windows should support filesystem byte totals")
		}
		// Both would require a handle per file, which is prohibitive at scale. Claiming them would
		// make disk-usage metrics silently equal logical size.
		if caps.Has(CapAllocBytes) {
			t.Error("windows must not claim allocated-block support")
		}
		if caps.Has(CapInode) {
			t.Error("windows must not claim inode support")
		}
	default:
		if caps != 0 {
			t.Errorf("unimplemented platform %s claims capabilities %v", runtime.GOOS, caps)
		}
	}
}

func TestFake_WalksTreeAndReportsSizes(t *testing.T) {
	fake := NewFake(CapAllocBytes|CapInode).
		AddFile("/logs/trades.log", 10).
		AddFile("/logs/nested/fills.log", 5).
		AddSymlink("/logs/archive")

	dir, err := fake.OpenDir("/logs")
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()

	found := map[string]fs.FileMode{}
	for {
		entries, err := dir.ReadSome(2)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			found[entry.Name] = entry.Type
		}
	}
	if len(found) != 3 {
		t.Fatalf("expected 3 entries, got %d: %v", len(found), found)
	}
	if !found["nested"].IsDir() {
		t.Error("nested was not reported as a directory")
	}
	if found["archive"]&fs.ModeSymlink == 0 {
		t.Error("archive was not reported as a symlink")
	}

	var st FileStat
	if err := dir.StatEntry("trades.log", &st); err != nil {
		t.Fatal(err)
	}
	if st.Size != 10 {
		t.Errorf("size = %d, want 10", st.Size)
	}
	// Allocation rounds up to a block boundary, as a real filesystem would for a small file.
	if st.AllocBytes != 512 {
		t.Errorf("alloc = %d, want 512", st.AllocBytes)
	}
}

func TestFake_OpenDirRefusesSymlinkAndMissingPaths(t *testing.T) {
	fake := NewFake(0).AddSymlink("/logs/archive").AddFile("/logs/trades.log", 1)

	for _, testCase := range []struct {
		name string
		path string
		want error
	}{
		{"symlink", "/logs/archive", fs.ErrInvalid},
		{"regular file", "/logs/trades.log", fs.ErrInvalid},
		{"missing", "/logs/nope", fs.ErrNotExist},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if _, err := fake.OpenDir(testCase.path); !errors.Is(err, testCase.want) {
				t.Fatalf("OpenDir(%s) error = %v, want %v", testCase.path, err, testCase.want)
			}
		})
	}
}

func TestFake_RecordsNonBareStatNames(t *testing.T) {
	fake := NewFake(0).AddFile("/logs/nested/fills.log", 5)
	dir, err := fake.OpenDir("/logs")
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()

	var st FileStat
	_ = dir.StatEntry("nested/fills.log", &st)
	if got := fake.NonBareStatNames(); len(got) != 1 || got[0] != "nested/fills.log" {
		t.Fatalf("joined path was not recorded: %v", got)
	}
	_ = dir.StatEntry("nested", &st)
	if got := fake.NonBareStatNames(); len(got) != 1 {
		t.Fatalf("bare name was wrongly recorded: %v", got)
	}
}

func TestFake_SyntheticEntriesAreGeneratedNotStored(t *testing.T) {
	const count = 100_000
	fake := NewFake(0).AddSyntheticFiles("/logs", count, 8)
	dir, err := fake.OpenDir("/logs")
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()

	var seen int
	var st FileStat
	for {
		entries, err := dir.ReadSome(1024)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			if err := dir.StatEntry(entry.Name, &st); err != nil {
				t.Fatalf("stat %s: %v", entry.Name, err)
			}
			if st.Size != 8 {
				t.Fatalf("synthetic size = %d, want 8", st.Size)
			}
			seen++
		}
	}
	if seen != count {
		t.Fatalf("read %d synthetic entries, want %d", seen, count)
	}
	if got := fake.StatCalls(); got != count {
		t.Fatalf("StatCalls = %d, want %d", got, count)
	}
}

func TestFake_ShuffledReadsDifferFromSortedOrder(t *testing.T) {
	names := []string{"a.log", "b.log", "c.log", "d.log", "e.log", "f.log", "g.log", "h.log"}
	build := func(shuffle bool) []string {
		fake := NewFake(0)
		for _, name := range names {
			fake.AddFile("/logs/"+name, 1)
		}
		if shuffle {
			fake.ShuffleReads(42)
		}
		dir, err := fake.OpenDir("/logs")
		if err != nil {
			t.Fatal(err)
		}
		defer dir.Close()
		entries, err := dir.ReadSome(len(names))
		if err != nil {
			t.Fatal(err)
		}
		order := make([]string, 0, len(entries))
		for _, entry := range entries {
			order = append(order, entry.Name)
		}
		return order
	}

	sorted, shuffled := build(false), build(true)
	if len(shuffled) != len(names) {
		t.Fatalf("shuffled read returned %d entries, want %d", len(shuffled), len(names))
	}
	if equalOrder(sorted, shuffled) {
		t.Fatal("shuffled order matched sorted order, so ordering assumptions would go untested")
	}
	// Shuffling must not lose or duplicate entries.
	counts := map[string]int{}
	for _, name := range shuffled {
		counts[name]++
	}
	for _, name := range names {
		if counts[name] != 1 {
			t.Errorf("entry %s appeared %d times", name, counts[name])
		}
	}
}

func TestFake_TracksOpenDirectoryHighWaterMark(t *testing.T) {
	fake := NewFake(0).AddFile("/logs/a/x.log", 1).AddFile("/logs/b/y.log", 1)
	first, err := fake.OpenDir("/logs/a")
	if err != nil {
		t.Fatal(err)
	}
	second, err := fake.OpenDir("/logs/b")
	if err != nil {
		t.Fatal(err)
	}
	if got := fake.MaxOpenDirs(); got != 2 {
		t.Fatalf("MaxOpenDirs = %d, want 2", got)
	}
	first.Close()
	second.Close()
	if got := fake.OpenDirs(); got != 0 {
		t.Fatalf("OpenDirs after close = %d, want 0", got)
	}
	// Close must be idempotent; the walker's error paths can reach it twice.
	first.Close()
	if got := fake.OpenDirs(); got != 0 {
		t.Fatalf("OpenDirs after double close = %d, want 0", got)
	}
}

func TestFake_HooksAndErrorsAreHonoured(t *testing.T) {
	sentinel := errors.New("boom")
	fake := NewFake(0).
		AddFile("/logs/trades.log", 1).
		AddFile("/logs/rotated.log", 1).
		SetOpenError("/logs/locked", sentinel).
		SetStatError("/logs/rotated.log", fs.ErrNotExist)
	fake.AddDir("/logs/locked")

	if _, err := fake.OpenDir("/logs/locked"); !errors.Is(err, sentinel) {
		t.Fatalf("open error = %v, want %v", err, sentinel)
	}

	dir, err := fake.OpenDir("/logs")
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()

	var st FileStat
	if err := dir.StatEntry("rotated.log", &st); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("stat error = %v, want fs.ErrNotExist", err)
	}

	var statted []string
	fake.BeforeStat(func(_, name string) error {
		statted = append(statted, name)
		return nil
	})
	if err := dir.StatEntry("trades.log", &st); err != nil {
		t.Fatal(err)
	}
	if len(statted) != 1 || statted[0] != "trades.log" {
		t.Fatalf("BeforeStat hook did not fire as expected: %v", statted)
	}
}

// TestFake_IsSafeForConcurrentUse matters because the walker this fake serves runs many workers
// against a single FS. A data race here would surface as flaky failures in unrelated packages.
func TestFake_IsSafeForConcurrentUse(t *testing.T) {
	fake := NewFake(0)
	for i := 0; i < 16; i++ {
		fake.AddSyntheticFiles("/logs/"+string(rune('a'+i)), 256, 4)
	}

	var group sync.WaitGroup
	for i := 0; i < 16; i++ {
		group.Add(1)
		go func(index int) {
			defer group.Done()
			dir, err := fake.OpenDir("/logs/" + string(rune('a'+index)))
			if err != nil {
				t.Error(err)
				return
			}
			defer dir.Close()
			var st FileStat
			for {
				entries, err := dir.ReadSome(32)
				if errors.Is(err, io.EOF) {
					return
				}
				if err != nil {
					t.Error(err)
					return
				}
				for _, entry := range entries {
					if err := dir.StatEntry(entry.Name, &st); err != nil {
						t.Error(err)
						return
					}
				}
			}
		}(i)
	}
	group.Wait()

	if got, want := fake.StatCalls(), int64(16*256); got != want {
		t.Fatalf("StatCalls = %d, want %d", got, want)
	}
	if got := fake.MaxOpenDirs(); got > 16 {
		t.Fatalf("MaxOpenDirs = %d, want at most 16", got)
	}
}

func TestFake_StatFSResolvesNearestMount(t *testing.T) {
	want := FSInfo{TotalBytes: 1 << 40, AvailBytes: 1 << 20, Mountpoint: "/logs", FSType: "xfs"}
	fake := NewFake(CapFSBytes).AddFile("/logs/deep/nested/trades.log", 1).SetFSInfo("/logs", want)

	got, err := fake.StatFS("/logs/deep/nested")
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("StatFS = %+v, want %+v", got, want)
	}
	if _, err := fake.StatFS("/elsewhere"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("unconfigured path error = %v, want fs.ErrNotExist", err)
	}
}

func equalOrder(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
