// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

//go:build darwin && arm64

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/tailscale/tb/mantsana/guest"
	"github.com/tailscale/tb/mantsana/image"
	"github.com/tailscale/tb/mantsana/vzvm"
)

// cmdWarm builds an image's warm snapshot: it boots the image once, waits
// for the guest to be usable, and saves the machine state, so later VMs
// restore in seconds instead of cold booting. Snapshots are only valid on
// this host and its current OS build, so run it once per image per Mac and
// again after macOS updates.
func cmdWarm(args []string) error {
	fs := flag.NewFlagSet("warm", flag.ExitOnError)
	readyTimeout := fs.Duration("ready-timeout", 5*time.Minute, "how long to wait for sshd in the guest")
	settle := fs.Duration("settle", 10*time.Second, "how long to let the guest settle before saving")
	cpus := fs.Int("cpus", 0, "CPUs for the snapshot; 0 means the image's. VMs restoring it must use the same")
	memGiB := fs.Int("mem-gib", 0, "memory in GiB for the snapshot; 0 means the image's. VMs restoring it must use the same")
	fs.Parse(args)
	if fs.NArg() != 1 {
		return errors.New("usage: warm [flags] <image-name>")
	}
	im, err := (&image.Store{Root: *imagesDir}).Get(fs.Arg(0))
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, *readyTimeout+5*time.Minute)
	defer cancel()

	t0 := time.Now()
	wi, err := guest.Warm(ctx, guest.WarmConfig{
		Config: guest.Config{
			Name:        "warm-" + im.Name,
			Image:       im,
			CPUs:        *cpus,
			MemoryBytes: uint64(*memGiB) << 30,
			Dir:         fmt.Sprintf("/tmp/mantsana-warm-%d", os.Getpid()),
			Backend:     &vzvm.Backend{},
			Logf:        log.Printf,
		},
		Settle: *settle,
	})
	if err != nil {
		return fmt.Errorf("%w\n(saving and restoring need the user's own launchd domain and an unlocked session; see DESIGN.md section 4.1)", err)
	}
	log.Printf("warm snapshot of %q built in %v for host OS %s, %d CPUs, %d MiB",
		im.Name, time.Since(t0).Round(time.Second), wi.HostOSBuild, wi.CPUs, wi.MemoryBytes>>20)
	return nil
}

// cmdImages lists the images in the store and whether each one's warm
// snapshot is usable on this host.
func cmdImages(args []string) error {
	if len(args) != 0 {
		return errors.New("usage: images")
	}
	ims, err := (&image.Store{Root: *imagesDir}).List()
	if err != nil {
		return err
	}
	host := (&vzvm.Backend{}).HostOSBuild()
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tCPUS\tMEMORY\tDISK\tWARM")
	for _, im := range ims {
		m := im.Manifest
		warm := "no"
		if ok, why := im.WarmUsable(host, m.CPUs, m.MemoryBytes); ok {
			warm = "yes (" + im.Warm.Created.Local().Format(time.DateTime) + ")"
		} else if im.Warm != nil {
			warm = "unusable: " + why
		}
		fmt.Fprintf(tw, "%s\t%d\t%d GiB\t%d GB\t%s\n", im.Name, m.CPUs, m.MemoryBytes>>30, m.DiskBytes/1e9, warm)
	}
	return tw.Flush()
}
