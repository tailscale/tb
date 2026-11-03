// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

//go:build unix

package guest

import (
	"os"
	"runtime/pprof"
)

// dumpGoroutines writes all goroutine stacks to a temporary file, for
// debugging a stuck teardown, and returns its name.
func dumpGoroutines() string {
	f, err := os.CreateTemp("", "mantsana-goroutines-*.txt")
	if err != nil {
		return "(error: " + err.Error() + ")"
	}
	defer f.Close()
	pprof.Lookup("goroutine").WriteTo(f, 2)
	return f.Name()
}
