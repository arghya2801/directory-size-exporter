// Package targets turns configured path patterns into the concrete directories to scan.
//
// Trade-log hosts rarely have a fixed set of directories: paths are dated, per-venue, or created
// by a rotation job. Requiring a restart for each new directory would mean the newest and most
// interesting data is the data nobody is watching, so patterns are re-resolved periodically.
package targets

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Options configures resolution.
type Options struct {
	// Max caps how many directories may be monitored. Every target multiplies the exporter's
	// series count and its filesystem load, and a careless glob such as /data/* can expand to
	// thousands, so exceeding this is a hard error rather than a warning.
	Max int
	// RetainVanished keeps a target that has stopped matching for this long before its series are
	// dropped. A directory briefly renamed by a rotation job would otherwise churn series on every
	// refresh. Zero drops immediately.
	RetainVanished time.Duration
	// Now is the clock, injectable for tests.
	Now func() time.Time
}

// Target is one resolved directory.
type Target struct {
	// Path is absolute, cleaned, and fully symlink-resolved.
	Path string
	// Pattern is the configured value that produced it.
	Pattern string
	// Overlaps lists other targets that contain this one. Overlapping targets are scanned twice
	// and their bytes counted twice, so the condition is surfaced rather than silently corrected:
	// it is occasionally intentional, and guessing which target to drop would be worse.
	Overlaps []string
	// Present is false while a target is being retained through its grace period.
	Present bool
}

// Resolution is the outcome of one pass.
type Resolution struct {
	Targets  []Target
	Added    []string
	Removed  []string
	Warnings []string
}

// Paths returns the resolved paths in order, which is what the scan engine and store consume.
func (r Resolution) Paths() []string {
	paths := make([]string, 0, len(r.Targets))
	for _, target := range r.Targets {
		paths = append(paths, target.Path)
	}
	return paths
}

// Resolver expands patterns and tracks what changed between passes.
type Resolver struct {
	mu       sync.Mutex
	patterns []string
	opts     Options
	// vanishedAt records when a previously-resolved target stopped matching.
	vanishedAt map[string]time.Time
	known      map[string]Target
}

// New returns a resolver over the given patterns.
func New(patterns []string, opts Options) *Resolver {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Resolver{
		patterns:   append([]string(nil), patterns...),
		opts:       opts,
		vanishedAt: map[string]time.Time{},
		known:      map[string]Target{},
	}
}

// SetPatterns replaces the configured patterns, for configuration reload.
func (r *Resolver) SetPatterns(patterns []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.patterns = append([]string(nil), patterns...)
}

// Resolve expands every pattern and returns the current target set.
func (r *Resolver) Resolve() (Resolution, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if len(r.patterns) == 0 {
		return Resolution{}, fmt.Errorf("at least one target path is required")
	}

	now := r.opts.Now()
	var result Resolution
	found := make(map[string]Target)

	for _, pattern := range r.patterns {
		matches, warning, err := expand(pattern)
		if err != nil {
			return Resolution{}, err
		}
		if warning != "" {
			result.Warnings = append(result.Warnings, warning)
		}
		for _, match := range matches {
			resolved, warning := resolveOne(match, pattern)
			if warning != "" {
				result.Warnings = append(result.Warnings, warning)
				continue
			}
			// First pattern to produce a path owns it, so overlapping patterns cannot create two
			// entries for the same directory and double-count it.
			if _, duplicate := found[resolved.Path]; !duplicate {
				found[resolved.Path] = resolved
			}
		}
	}

	// Carry forward targets that have stopped matching but are still inside their grace period, so
	// a directory briefly renamed by a rotation job does not churn its series away and back.
	for path, target := range r.known {
		if _, still := found[path]; still {
			delete(r.vanishedAt, path)
			continue
		}
		since, seen := r.vanishedAt[path]
		if !seen {
			since = now
			r.vanishedAt[path] = since
		}
		if r.opts.RetainVanished > 0 && now.Sub(since) < r.opts.RetainVanished {
			target.Present = false
			found[path] = target
			result.Warnings = append(result.Warnings, fmt.Sprintf(
				"target %s no longer matches its pattern; retaining for %s", path,
				r.opts.RetainVanished-now.Sub(since)))
		}
	}

	if r.opts.Max > 0 && len(found) > r.opts.Max {
		return Resolution{}, fmt.Errorf(
			"%d targets resolved, which exceeds the limit of %d; every target multiplies both series count and filesystem load, so widen the limit deliberately or narrow the patterns",
			len(found), r.opts.Max)
	}

	paths := make([]string, 0, len(found))
	for path := range found {
		paths = append(paths, path)
	}
	sort.Strings(paths)

	targets := make([]Target, 0, len(paths))
	for _, path := range paths {
		targets = append(targets, found[path])
	}
	markOverlaps(targets)
	result.Targets = targets

	for _, path := range paths {
		if _, existed := r.known[path]; !existed {
			result.Added = append(result.Added, path)
		}
	}
	for path := range r.known {
		if _, still := found[path]; !still {
			result.Removed = append(result.Removed, path)
			delete(r.vanishedAt, path)
		}
	}
	sort.Strings(result.Removed)

	r.known = found
	return result, nil
}

// expand turns one pattern into matching paths. A pattern with no wildcard is used literally, so
// a path that happens to contain bracket characters still works.
func expand(pattern string) ([]string, string, error) {
	if !hasMeta(pattern) {
		return []string{pattern}, "", nil
	}
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return nil, "", fmt.Errorf("invalid target pattern %q: %w", pattern, err)
	}
	if len(matches) == 0 {
		// A warning, not an error: a dated pattern legitimately matches nothing before the first
		// directory of the day exists, and failing startup over that would be worse than useless.
		return nil, fmt.Sprintf("target pattern %q matched no directories", pattern), nil
	}
	return matches, "", nil
}

func hasMeta(pattern string) bool {
	return strings.ContainsAny(pattern, `*?[`)
}

// resolveOne normalises a single path and rejects anything that is not a usable directory.
func resolveOne(path, pattern string) (Target, string) {
	trimmed := strings.TrimSpace(path)
	if trimmed == "" {
		return Target{}, "ignoring an empty target path"
	}

	absolute, err := filepath.Abs(trimmed)
	if err != nil {
		return Target{}, fmt.Sprintf("cannot resolve target %q: %v", trimmed, err)
	}

	// Symlinks are resolved here, once, rather than during the walk. A symlinked root that reaches
	// the scanner unresolved measures zero bytes while reporting success, which is the worst kind
	// of failure: it looks healthy. Resolving here also makes two patterns that reach the same
	// directory by different routes deduplicate correctly.
	resolved, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return Target{}, fmt.Sprintf("cannot resolve target %q: %v", absolute, err)
	}
	resolved = filepath.Clean(resolved)

	info, err := os.Stat(resolved)
	if err != nil {
		return Target{}, fmt.Sprintf("cannot stat target %q: %v", resolved, err)
	}
	if !info.IsDir() {
		return Target{}, fmt.Sprintf("ignoring target %q: not a directory", resolved)
	}
	return Target{Path: resolved, Pattern: pattern, Present: true}, ""
}

// markOverlaps records containment between targets. targets must be sorted by path.
func markOverlaps(targets []Target) {
	for i := range targets {
		for j := range targets {
			if i == j {
				continue
			}
			if isUnder(targets[i].Path, targets[j].Path) {
				targets[i].Overlaps = append(targets[i].Overlaps, targets[j].Path)
			}
		}
	}
}

// isUnder reports whether child sits inside parent, comparing whole path components so that
// /var/logger is not treated as living inside /var/log.
func isUnder(child, parent string) bool {
	if child == parent {
		return false
	}
	prefix := parent
	if !strings.HasSuffix(prefix, string(filepath.Separator)) {
		prefix += string(filepath.Separator)
	}
	return strings.HasPrefix(child, prefix)
}
