package targets

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type clock struct{ now time.Time }

func (c *clock) Now() time.Time          { return c.now }
func (c *clock) advance(d time.Duration) { c.now = c.now.Add(d) }
func newClock() *clock                   { return &clock{now: time.Unix(1_700_000_000, 0)} }

func mkdirs(t *testing.T, base string, names ...string) []string {
	t.Helper()
	paths := make([]string, 0, len(names))
	for _, name := range names {
		path := filepath.Join(base, name)
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatal(err)
		}
		// Resolve here too: on macOS and some Windows setups the temp dir itself is a symlink, so
		// the expected value must be compared after resolution or every test fails spuriously.
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil {
			t.Fatal(err)
		}
		paths = append(paths, resolved)
	}
	return paths
}

func TestResolver_ExpandsGlobsAndSortsResults(t *testing.T) {
	base := t.TempDir()
	want := mkdirs(t, base, "venue-a", "venue-b", "venue-c")
	mkdirs(t, base, "other")

	resolver := New([]string{filepath.Join(base, "venue-*")}, Options{Now: newClock().Now})
	got, err := resolver.Resolve()
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Targets) != 3 {
		t.Fatalf("resolved %d targets, want 3: %v", len(got.Targets), got.Paths())
	}
	for i, path := range got.Paths() {
		if path != want[i] {
			t.Errorf("target %d = %s, want %s", i, path, want[i])
		}
	}
	if len(got.Added) != 3 {
		t.Errorf("added = %v, want three entries on the first pass", got.Added)
	}
}

func TestResolver_LiteralPathNeedsNoGlob(t *testing.T) {
	base := t.TempDir()
	want := mkdirs(t, base, "logs")[0]

	resolver := New([]string{filepath.Join(base, "logs")}, Options{Now: newClock().Now})
	got, err := resolver.Resolve()
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Targets) != 1 || got.Targets[0].Path != want {
		t.Fatalf("targets = %v, want [%s]", got.Paths(), want)
	}
}

// TestResolver_NonMatchingGlobIsAWarning matters because a dated pattern legitimately matches
// nothing before the first directory of the day exists. Failing startup over that would take the
// exporter down every midnight.
func TestResolver_NonMatchingGlobIsAWarning(t *testing.T) {
	base := t.TempDir()
	mkdirs(t, base, "logs")

	resolver := New([]string{
		filepath.Join(base, "logs"),
		filepath.Join(base, "nothing-*"),
	}, Options{Now: newClock().Now})
	got, err := resolver.Resolve()
	if err != nil {
		t.Fatalf("a non-matching glob failed resolution: %v", err)
	}
	if len(got.Targets) != 1 {
		t.Errorf("targets = %v, want the one that matched", got.Paths())
	}
	if len(got.Warnings) != 1 || !strings.Contains(got.Warnings[0], "matched no directories") {
		t.Errorf("warnings = %v, want one about the empty glob", got.Warnings)
	}
}

// TestResolver_ResolvesSymlinkedTargets guards the failure that previously reported zero bytes
// while claiming success. Resolving once here also makes two routes to one directory deduplicate.
func TestResolver_ResolvesSymlinkedTargets(t *testing.T) {
	base := t.TempDir()
	real := mkdirs(t, base, "real")[0]
	link := filepath.Join(base, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("cannot create symlinks on this host: %v", err)
	}

	resolver := New([]string{link, real}, Options{Now: newClock().Now})
	got, err := resolver.Resolve()
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Targets) != 1 {
		t.Fatalf("targets = %v, want one after resolving the symlink", got.Paths())
	}
	if got.Targets[0].Path != real {
		t.Errorf("target = %s, want the resolved path %s", got.Targets[0].Path, real)
	}
}

func TestResolver_RejectsNonDirectories(t *testing.T) {
	base := t.TempDir()
	mkdirs(t, base, "logs")
	file := filepath.Join(base, "trades.log")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	resolver := New([]string{filepath.Join(base, "logs"), file}, Options{Now: newClock().Now})
	got, err := resolver.Resolve()
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Targets) != 1 {
		t.Errorf("targets = %v, want only the directory", got.Paths())
	}
	if len(got.Warnings) != 1 || !strings.Contains(got.Warnings[0], "not a directory") {
		t.Errorf("warnings = %v, want one about the non-directory", got.Warnings)
	}
}

func TestResolver_DetectsOverlappingTargets(t *testing.T) {
	base := t.TempDir()
	paths := mkdirs(t, base, "logs", "logs/venue-a", "logger")

	resolver := New([]string{paths[0], paths[1], paths[2]}, Options{Now: newClock().Now})
	got, err := resolver.Resolve()
	if err != nil {
		t.Fatal(err)
	}

	byPath := map[string]Target{}
	for _, target := range got.Targets {
		byPath[target.Path] = target
	}
	// Overlapping targets are scanned twice and counted twice. That is occasionally intentional,
	// so it is surfaced rather than silently corrected.
	nested := byPath[paths[1]]
	if len(nested.Overlaps) != 1 || nested.Overlaps[0] != paths[0] {
		t.Errorf("%s overlaps = %v, want [%s]", nested.Path, nested.Overlaps, paths[0])
	}
	// A sibling sharing a name prefix must not be mistaken for a child.
	if sibling := byPath[paths[2]]; len(sibling.Overlaps) != 0 {
		t.Errorf("%s overlaps = %v, want none; /logger is not inside /logs", sibling.Path, sibling.Overlaps)
	}
	if parent := byPath[paths[0]]; len(parent.Overlaps) != 0 {
		t.Errorf("%s overlaps = %v, want none", parent.Path, parent.Overlaps)
	}
}

func TestResolver_EnforcesMaxTargets(t *testing.T) {
	base := t.TempDir()
	mkdirs(t, base, "a", "b", "c")

	resolver := New([]string{filepath.Join(base, "*")}, Options{Max: 2, Now: newClock().Now})
	// A hard error, not a truncation: silently monitoring an arbitrary subset would be worse than
	// refusing to start, because nothing would reveal which directories were dropped.
	if _, err := resolver.Resolve(); err == nil {
		t.Fatal("resolution succeeded despite exceeding the target limit")
	} else if !strings.Contains(err.Error(), "exceeds the limit") {
		t.Errorf("error = %v, want one naming the limit", err)
	}
}

func TestResolver_ReportsAddedAndRemovedBetweenPasses(t *testing.T) {
	base := t.TempDir()
	mkdirs(t, base, "venue-a")
	resolver := New([]string{filepath.Join(base, "venue-*")}, Options{Now: newClock().Now})
	if _, err := resolver.Resolve(); err != nil {
		t.Fatal(err)
	}

	added := mkdirs(t, base, "venue-b")[0]
	got, err := resolver.Resolve()
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Added) != 1 || got.Added[0] != added {
		t.Errorf("added = %v, want [%s]", got.Added, added)
	}
	if len(got.Removed) != 0 {
		t.Errorf("removed = %v, want none", got.Removed)
	}
}

// TestResolver_RetainsVanishedTargetsThroughGracePeriod covers the rotation case: a directory
// briefly renamed would otherwise churn its series away and back on every refresh.
func TestResolver_RetainsVanishedTargetsThroughGracePeriod(t *testing.T) {
	base := t.TempDir()
	paths := mkdirs(t, base, "venue-a", "venue-b")
	clock := newClock()
	resolver := New([]string{filepath.Join(base, "venue-*")}, Options{
		RetainVanished: 10 * time.Minute,
		Now:            clock.Now,
	})
	if _, err := resolver.Resolve(); err != nil {
		t.Fatal(err)
	}

	if err := os.RemoveAll(paths[1]); err != nil {
		t.Fatal(err)
	}
	clock.advance(5 * time.Minute)
	got, err := resolver.Resolve()
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Targets) != 2 {
		t.Fatalf("targets = %v, want both retained inside the grace period", got.Paths())
	}
	for _, target := range got.Targets {
		if target.Path == paths[1] && target.Present {
			t.Error("a vanished target is still marked present")
		}
	}
	if len(got.Removed) != 0 {
		t.Errorf("removed = %v, want none during the grace period", got.Removed)
	}

	// The grace period runs from the pass that first noticed the target missing, not from the last
	// pass that saw it, so the refresh interval does not silently eat into the grace.
	clock.advance(11 * time.Minute)
	got, err = resolver.Resolve()
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Targets) != 1 || got.Targets[0].Path != paths[0] {
		t.Fatalf("targets = %v, want only the surviving one after the grace period", got.Paths())
	}
	// Dropping it is what garbage-collects its series; otherwise a deleted directory would report
	// its final size forever.
	if len(got.Removed) != 1 || got.Removed[0] != paths[1] {
		t.Errorf("removed = %v, want [%s]", got.Removed, paths[1])
	}
}

func TestResolver_ZeroGraceDropsImmediately(t *testing.T) {
	base := t.TempDir()
	paths := mkdirs(t, base, "venue-a", "venue-b")
	resolver := New([]string{filepath.Join(base, "venue-*")}, Options{Now: newClock().Now})
	if _, err := resolver.Resolve(); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(paths[1]); err != nil {
		t.Fatal(err)
	}

	got, err := resolver.Resolve()
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Targets) != 1 || got.Targets[0].Path != paths[0] {
		t.Fatalf("targets = %v, want only %s", got.Paths(), paths[0])
	}
	if len(got.Removed) != 1 {
		t.Errorf("removed = %v, want one", got.Removed)
	}
}

func TestResolver_RequiresAtLeastOnePattern(t *testing.T) {
	if _, err := New(nil, Options{Now: newClock().Now}).Resolve(); err == nil {
		t.Fatal("resolution succeeded with no patterns configured")
	}
}

func TestResolver_SetPatternsAppliesOnNextResolve(t *testing.T) {
	base := t.TempDir()
	paths := mkdirs(t, base, "venue-a", "venue-b")
	resolver := New([]string{paths[0]}, Options{Now: newClock().Now})
	if _, err := resolver.Resolve(); err != nil {
		t.Fatal(err)
	}

	// Configuration reload replaces the pattern set.
	resolver.SetPatterns([]string{paths[1]})
	got, err := resolver.Resolve()
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Targets) != 1 || got.Targets[0].Path != paths[1] {
		t.Fatalf("targets = %v, want [%s]", got.Paths(), paths[1])
	}
	if len(got.Removed) != 1 || got.Removed[0] != paths[0] {
		t.Errorf("removed = %v, want [%s]", got.Removed, paths[0])
	}
}

func TestResolver_DuplicatePatternsResolveOnce(t *testing.T) {
	base := t.TempDir()
	path := mkdirs(t, base, "logs")[0]

	resolver := New([]string{path, path, filepath.Join(base, "lo*")}, Options{Now: newClock().Now})
	got, err := resolver.Resolve()
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Targets) != 1 {
		t.Fatalf("targets = %v, want one; duplicates would double-count the same bytes", got.Paths())
	}
}
