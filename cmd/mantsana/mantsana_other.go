// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

//go:build !(darwin && arm64)

// The mantsana command runs ephemeral macOS CI guest VMs on Apple Silicon
// Macs. It only runs there.
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "mantsana only runs on Apple Silicon Macs")
	os.Exit(1)
}
