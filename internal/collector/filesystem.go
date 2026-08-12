package collector

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/local/directory-size-exporter/internal/fsstat"
)

// FilesystemCache holds capacity information for the filesystems containing the targets.
//
// It is refreshed on the scan cycle rather than during collection, because statfs on a hung mount
// blocks uninterruptibly and a scrape must never be able to hang. Collection only reads memory.
type FilesystemCache struct {
	fs      fsstat.FS
	timeout time.Duration
	logger  *slog.Logger

	mu       sync.RWMutex
	byTarget map[string]fsEntry
	timeouts map[string]uint64
}

type fsEntry struct {
	info      fsstat.FSInfo
	available bool
}

// NewFilesystemCache returns a cache that abandons any statfs exceeding timeout.
func NewFilesystemCache(fs fsstat.FS, timeout time.Duration, logger *slog.Logger) *FilesystemCache {
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &FilesystemCache{
		fs: fs, timeout: timeout, logger: logger,
		byTarget: map[string]fsEntry{}, timeouts: map[string]uint64{},
	}
}

// Refresh re-reads filesystem capacity for every target, dropping any target no longer configured.
func (c *FilesystemCache) Refresh(ctx context.Context, targets []string) {
	next := make(map[string]fsEntry, len(targets))
	for _, target := range targets {
		if ctx.Err() != nil {
			return
		}
		info, err := c.statfsWithTimeout(ctx, target)
		if err != nil {
			// Retaining the previous reading would be wrong here in a way it is not for directory
			// sizes: capacity is cheap to re-read, and a stale free-space figure feeds directly
			// into "hours until the volume fills". Better to publish nothing.
			next[target] = fsEntry{}
			continue
		}
		next[target] = fsEntry{info: info, available: true}
	}

	c.mu.Lock()
	c.byTarget = next
	c.mu.Unlock()
}

// errStatfsTimeout marks a capacity query abandoned because the mount stopped responding.
var errStatfsTimeout = context.DeadlineExceeded

// statfsWithTimeout runs the query on its own goroutine and gives up after the timeout.
//
// The goroutine is deliberately abandoned rather than waited on. A statfs against a dead NFS
// server sits in uninterruptible sleep and will never return, so joining it would wedge the scan
// cycle permanently. The leak is bounded by the number of hung mounts.
func (c *FilesystemCache) statfsWithTimeout(ctx context.Context, target string) (fsstat.FSInfo, error) {
	type result struct {
		info fsstat.FSInfo
		err  error
	}
	done := make(chan result, 1) // buffered so the abandoned goroutine can always exit
	go func() {
		info, err := c.fs.StatFS(target)
		done <- result{info: info, err: err}
	}()

	timer := time.NewTimer(c.timeout)
	defer timer.Stop()
	select {
	case got := <-done:
		if got.err != nil {
			c.logger.Warn("Filesystem capacity query failed", "target", target, "err", got.err)
		}
		return got.info, got.err
	case <-timer.C:
		c.mu.Lock()
		c.timeouts[target]++
		c.mu.Unlock()
		c.logger.Error("Filesystem capacity query timed out; the mount appears hung",
			"target", target, "timeout", c.timeout)
		return fsstat.FSInfo{}, errStatfsTimeout
	case <-ctx.Done():
		return fsstat.FSInfo{}, ctx.Err()
	}
}

// collectFilesystems emits one series set per distinct filesystem, plus a join series per target.
func (c *Collector) collectFilesystems(ch chan<- prometheus.Metric) {
	c.fsCache.mu.RLock()
	byTarget := make(map[string]fsEntry, len(c.fsCache.byTarget))
	for target, entry := range c.fsCache.byTarget {
		byTarget[target] = entry
	}
	timeouts := make(map[string]uint64, len(c.fsCache.timeouts))
	for target, count := range c.fsCache.timeouts {
		timeouts[target] = count
	}
	c.fsCache.mu.RUnlock()

	d := c.descs
	// Several targets commonly live on one volume. Capacity is a property of the filesystem, so it
	// is emitted once per distinct label set; duplicating it per target would produce conflicting
	// duplicate series that Prometheus rejects outright.
	seen := make(map[[3]string]struct{}, len(byTarget))
	for target, entry := range byTarget {
		counter(ch, d.statfsTimeouts, float64(timeouts[target]), target)
		gauge(ch, d.fsUnavailable, boolValue(!entry.available), target)
		if !entry.available {
			continue
		}

		info := entry.info
		labels := [3]string{info.Mountpoint, info.Device, info.FSType}
		gauge(ch, d.mountpointInfo, 1, target, info.Mountpoint, info.Device, info.FSType)
		if _, duplicate := seen[labels]; duplicate {
			continue
		}
		seen[labels] = struct{}{}

		gauge(ch, d.fsSize, float64(info.TotalBytes), labels[0], labels[1], labels[2])
		gauge(ch, d.fsFree, float64(info.FreeBytes), labels[0], labels[1], labels[2])
		gauge(ch, d.fsAvail, float64(info.AvailBytes), labels[0], labels[1], labels[2])
		if c.caps.Has(fsstat.CapFSInodes) {
			gauge(ch, d.fsFiles, float64(info.TotalInodes), labels[0], labels[1], labels[2])
			gauge(ch, d.fsFilesFree, float64(info.FreeInodes), labels[0], labels[1], labels[2])
		}
	}
}
