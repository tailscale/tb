// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

// The oci-img-dl command downloads a macOS VM image published to an OCI
// registry (for example ghcr.io/cirruslabs/macos-tahoe-xcode:26.5) into a
// mantsana image directory. See package ocidl.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/tailscale/tb/cmd/oci-img-dl/ocidl"
	"github.com/tailscale/tb/mantsana/image"
)

var parallel = flag.Int("j", 8, "number of chunks to download at once")

func main() {
	log.SetFlags(log.Ltime)
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: oci-img-dl [-j N] <[registry/]repo[:tag|@digest]> <image-name | image-dir>\n\n")
		fmt.Fprintf(os.Stderr, "A bare image name is created in %s.\n\n", image.DefaultStoreDir())
		fmt.Fprintf(os.Stderr, "example: oci-img-dl ghcr.io/cirruslabs/macos-tahoe-xcode:26.5 tahoe-xcode-26.5\n\n")
		flag.PrintDefaults()
	}
	flag.Parse()
	if flag.NArg() != 2 {
		flag.Usage()
		os.Exit(2)
	}
	r, err := ocidl.ParseRef(flag.Arg(0))
	if err != nil {
		log.Fatal(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	dir := flag.Arg(1)
	if !strings.ContainsRune(dir, filepath.Separator) {
		dir = filepath.Join(image.DefaultStoreDir(), dir)
	}
	if err := ocidl.Download(ctx, r, dir, ocidl.Options{Parallel: *parallel, Logf: log.Printf}); err != nil {
		log.Fatal(err)
	}
}
