//go:build unix

package main

import (
	"os"
	"os/signal"
	"syscall"
)

// notifyReload subscribes to SIGHUP, the conventional reload signal for Prometheus exporters.
func notifyReload(ch chan<- os.Signal) {
	signal.Notify(ch, syscall.SIGHUP)
}
