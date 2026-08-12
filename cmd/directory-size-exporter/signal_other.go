//go:build !unix

package main

import "os"

// notifyReload does nothing on platforms without SIGHUP. Reload is still reachable through
// POST /-/reload with --web.enable-lifecycle, which is how the development build exercises it.
func notifyReload(chan<- os.Signal) {}
