// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

//go:build darwin && arm64

// The mantsana command runs ephemeral macOS CI guest VMs on Apple Silicon
// Macs. See DESIGN.md.
//
// Build it on the Mac and ad hoc sign it with the virtualization
// entitlement:
//
//	go build ./cmd/mantsana && codesign --entitlements cmd/mantsana/mantsana.entitlements -s - ./mantsana
package main

import (
	"flag"
	"fmt"
	"log"
	"os"

	"github.com/tailscale/tb/mantsana/image"
)

var imagesDir = flag.String("images", image.DefaultStoreDir(), "directory of VM images (get one with cmd/oci-img-dl)")

var commands = map[string]func(args []string) error{
	"images":           cmdImages,
	"warm":             cmdWarm,
	"selftest":         cmdSelftest,
	"nbdprobe":         cmdNBDProbe,
	"restoreprobe":     cmdRestoreProbe,
	"entitlementprobe": cmdEntitlementProbe,
}

func usage() {
	fmt.Fprintf(os.Stderr, `usage: mantsana [flags] <command> [args]

Commands:
  images                              list images and whether their warm snapshots are usable here
  warm [flags] <image-name>           build an image's warm snapshot, so VMs restore in seconds
  selftest [flags] <image-name>       validate, cold boot, warm up and restore an image, with timings
  nbdprobe [flags] <image-name>       boot an image's disk through a logging NBD proxy (debugging)
  restoreprobe [flags] <image-name>   try restoring an image's warm snapshot several ways (debugging)
  entitlementprobe                    check this binary may use Virtualization.framework (no image needed)

Flags:
`)
	flag.PrintDefaults()
}

func main() {
	log.SetFlags(log.Ltime | log.Lmicroseconds)
	flag.Usage = usage
	flag.Parse()
	if flag.NArg() == 0 {
		usage()
		os.Exit(2)
	}
	cmd, ok := commands[flag.Arg(0)]
	if !ok {
		fmt.Fprintf(os.Stderr, "mantsana: unknown command %q\n", flag.Arg(0))
		usage()
		os.Exit(2)
	}
	if err := cmd(flag.Args()[1:]); err != nil {
		log.Fatalf("%s: %v", flag.Arg(0), err)
	}
}
