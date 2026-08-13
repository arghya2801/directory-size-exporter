package schedprio

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"runtime"
	"syscall"
	"testing"
)

func TestParseIOClass(t *testing.T) {
	for _, testCase := range []struct {
		raw     string
		want    IOClass
		wantErr bool
	}{
		{"idle", IOClassIdle, false},
		{"IDLE", IOClassIdle, false},
		{" best-effort ", IOClassBestEffort, false},
		{"none", IOClassNone, false},
		{"", IOClassNone, false},
		{"realtime", IOClassNone, true},
	} {
		t.Run(testCase.raw, func(t *testing.T) {
			got, err := ParseIOClass(testCase.raw)
			if testCase.wantErr {
				if err == nil {
					t.Fatalf("ParseIOClass(%q) accepted an invalid class", testCase.raw)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != testCase.want {
				t.Errorf("ParseIOClass(%q) = %v, want %v", testCase.raw, got, testCase.want)
			}
		})
	}
}

func TestIOClassString(t *testing.T) {
	for class, want := range map[IOClass]string{
		IOClassIdle:       "idle",
		IOClassBestEffort: "best-effort",
		IOClassNone:       "none",
	} {
		if got := class.String(); got != want {
			t.Errorf("IOClass(%d).String() = %q, want %q", class, got, want)
		}
	}
}

// TestSupportedMatchesPlatform pins the degradation policy. Reporting support that does not exist
// would make the startup audit claim the scan is deprioritised when it is not, which is worse than
// reporting the limitation honestly.
func TestSupportedMatchesPlatform(t *testing.T) {
	if got, want := Supported(), runtime.GOOS == "linux"; got != want {
		t.Errorf("Supported() = %v on %s, want %v", got, runtime.GOOS, want)
	}
}

// TestWrappedErrnoIsRecognisedAsPermission verifies the assumption the Linux-only tests below
// depend on, and does it on every platform so it cannot rot unnoticed.
//
// setpriority(2) returns EACCES when a caller lacks CAP_SYS_NICE to raise priority, and reserves
// EPERM for an unrelated case. Both print as "permission denied", so a test asserting one specific
// errno reads as correct and fails against the other. fs.ErrPermission covers both, and the same
// mapping is what the scan error classifier relies on to group permission failures.
func TestWrappedErrnoIsRecognisedAsPermission(t *testing.T) {
	for name, errno := range map[string]syscall.Errno{
		"EACCES": syscall.EACCES,
		"EPERM":  syscall.EPERM,
	} {
		t.Run(name, func(t *testing.T) {
			wrapped := fmt.Errorf("set nice %d on thread %d: %w", 0, 1234, errno)
			if !errors.Is(wrapped, fs.ErrPermission) {
				t.Errorf("wrapped %s is not matched by fs.ErrPermission", name)
			}
		})
	}
}

func TestApplyToCurrentThread(t *testing.T) {
	// The goroutine must stay pinned: Go may otherwise migrate it and leave a deprioritised thread
	// behind serving unrelated work.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	err := ApplyToCurrentThread(Priority{Nice: 19, IOClass: IOClassIdle})
	if runtime.GOOS != "linux" {
		if !errors.Is(err, ErrUnsupported) {
			t.Fatalf("error = %v, want ErrUnsupported on %s", err, runtime.GOOS)
		}
		return
	}
	if err != nil {
		// Lowering priority never needs privilege; raising it does. A failure here is a real bug.
		t.Fatalf("applying idle priority failed: %v", err)
	}
}

func TestApplyToCurrentThread_NoneLeavesIOPriorityAlone(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("scheduling priority is only implemented on Linux")
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	// Nice 19 specifically, not an arbitrary middle value. Go may hand this test the same OS
	// thread a previous test already lowered to 19, and moving a thread back UP the priority scale
	// requires CAP_SYS_NICE, which an unprivileged CI runner does not have. 19 is the floor, so it
	// is always a lowering or a no-op and never needs privilege.
	if err := ApplyToCurrentThread(Priority{Nice: 19, IOClass: IOClassNone}); err != nil {
		t.Fatalf("applying nice without an I/O class failed: %v", err)
	}
}

// TestApplyToCurrentThread_RaisingPriorityNeedsPrivilege documents the asymmetry that made the
// test above fragile, and confirms the engine's decision to treat this as a warning rather than a
// fatal error: an operator asking for a nice value below the process's current one gets a scan at
// normal priority, not no scan at all.
func TestApplyToCurrentThread_RaisingPriorityNeedsPrivilege(t *testing.T) {
	if runtime.GOOS != "linux" || os.Geteuid() == 0 {
		t.Skip("needs an unprivileged Linux process")
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	if err := ApplyToCurrentThread(Priority{Nice: 19}); err != nil {
		t.Fatalf("lowering priority should never need privilege: %v", err)
	}

	err := ApplyToCurrentThread(Priority{Nice: 0})
	if err == nil {
		t.Skip("this process may raise its own priority; nothing to assert")
	}
	// fs.ErrPermission rather than a specific errno. setpriority(2) returns EACCES for "tried to
	// raise priority without CAP_SYS_NICE" and reserves EPERM for a different case entirely, but
	// both print as "permission denied" — so asserting on one errno looks right and fails. The
	// portable sentinel covers both, and is the same mapping the scan error classifier relies on.
	if !errors.Is(err, fs.ErrPermission) {
		t.Errorf("raising priority failed with %v, want a permission error", err)
	}
}
