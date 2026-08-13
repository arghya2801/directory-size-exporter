package scan

import (
	"sync"
	"time"
)

// startHeartbeat begins periodic progress logging for a running target and returns a stop function
// that is safe to call more than once.
//
// Without this, a walk over a multi-terabyte tree is a silent gap of many minutes in the log, and
// a slow scan is indistinguishable from a hung one. The heartbeat only starts after a threshold so
// that ordinary short scans stay quiet.
func (e *Engine) startHeartbeat(target *targetScan) func() {
	if e.cfg.HeartbeatAfter <= 0 {
		return func() {}
	}

	stop := make(chan struct{})
	var once sync.Once
	stopper := func() { once.Do(func() { close(stop) }) }

	go func() {
		delay := time.NewTimer(e.cfg.HeartbeatAfter)
		defer delay.Stop()
		select {
		case <-stop:
			return
		case <-delay.C:
		}

		ticker := time.NewTicker(e.cfg.HeartbeatEvery)
		defer ticker.Stop()
		for {
			e.logProgress(target)
			select {
			case <-stop:
				return
			case <-ticker.C:
			}
		}
	}()
	return stopper
}

func (e *Engine) logProgress(target *targetScan) {
	_, errorTotal := target.errorSnapshot()
	e.logger.Info("Directory scan still running",
		"target", target.path,
		"elapsed_seconds", time.Since(target.startedAt).Round(time.Second).Seconds(),
		"entries_scanned", target.entries.Load(),
		"directories_read", target.dirsRead.Load(),
		"queued_directories", target.pending.Load(),
		"bytes_so_far", target.size.Load(),
		"errors", errorTotal,
	)
}
