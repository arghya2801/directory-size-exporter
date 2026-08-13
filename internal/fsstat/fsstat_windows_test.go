//go:build windows

package fsstat

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

func TestWindows_StatFSPopulatesByteTotals(t *testing.T) {
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
	if info.Mountpoint == "" {
		t.Error("Mountpoint is empty; the volume root should be resolvable")
	}
	// Inode totals have no Windows equivalent, so they must stay zero rather than be invented.
	if info.TotalInodes != 0 || info.FreeInodes != 0 {
		t.Errorf("inode totals were populated on windows: %+v", info)
	}
}

// TestWindows_UnsupportedFieldsStayZero is the enforcement point for the degradation rule. These
// fields must never be filled with a plausible substitute, because a disk-usage metric equal to
// logical size looks like a real measurement and would misreport every sparse or compressed file.
func TestWindows_UnsupportedFieldsStayZero(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "trades.log")
	if err := os.WriteFile(path, []byte("0123456789"), 0o600); err != nil {
		t.Fatal(err)
	}

	var st FileStat
	if err := System().Lstat(path, &st); err != nil {
		t.Fatal(err)
	}
	if st.Size != 10 {
		t.Fatalf("Size = %d, want 10", st.Size)
	}
	if st.AllocBytes != 0 {
		t.Errorf("AllocBytes = %d, want 0 because CapAllocBytes is not advertised", st.AllocBytes)
	}
	if st.Dev != 0 || st.Ino != 0 || st.Nlink != 0 {
		t.Errorf("inode fields were populated without CapInode: %+v", st)
	}

	handle, err := System().OpenDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer handle.Close()

	var entryStat FileStat
	if err := handle.StatEntry("trades.log", &entryStat); err != nil {
		t.Fatal(err)
	}
	if entryStat.AllocBytes != 0 || entryStat.Ino != 0 {
		t.Errorf("StatEntry populated unsupported fields: %+v", entryStat)
	}
	if !entryStat.IsRegular() {
		t.Errorf("Mode = %v, want a regular file", entryStat.Mode)
	}
}

func TestWindows_ReadSomeIsIncremental(t *testing.T) {
	dir := t.TempDir()
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
		names = append(names, entriesNames(entries)...)
	}
	if len(names) != 4 {
		t.Fatalf("read %d entries, want 4: %v", len(names), names)
	}
	if batches < 2 {
		t.Errorf("read completed in %d batches; ReadSome is not incremental", batches)
	}
}

func TestWindows_OpenDirRejectsFilesAndMissingPaths(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "trades.log")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := System().OpenDir(file); err == nil {
		t.Error("OpenDir accepted a regular file")
	}
	if _, err := System().OpenDir(filepath.Join(dir, "gone")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("OpenDir on a missing path = %v, want fs.ErrNotExist", err)
	}
}

func entriesNames(entries []Entry) []string {
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name)
	}
	return names
}
