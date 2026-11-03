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
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/tailscale/tb/guestbd"
	"github.com/tailscale/tb/guestnet"
	"github.com/tailscale/tb/mantsana/guest"
	"github.com/tailscale/tb/mantsana/image"
	"github.com/tailscale/tb/mantsana/vzvm"
)

// cmdRestoreProbe tries restoring an image's warm snapshot with different
// disk attachments and file paths, to find what the framework requires.
func cmdRestoreProbe(args []string) error {
	fs := flag.NewFlagSet("restoreprobe", flag.ExitOnError)
	variants := fs.String("variants", "nbd,clone,samepath", "comma-separated variants: nbd (NBD of warm/disk.img), clone (disk-image attachment on a clone), samepath (clones at the paths used when saving), nbd-live (cold boot, save and restore all over one live NBD server)")
	fs.Parse(args)
	if fs.NArg() != 1 {
		return errors.New("usage: restoreprobe [flags] <image-name>")
	}
	im, err := (&image.Store{Root: *imagesDir}).Get(fs.Arg(0))
	if err != nil {
		return err
	}
	if im.Warm == nil {
		return errors.New("image has no warm snapshot")
	}
	for _, v := range strings.Split(*variants, ",") {
		t0 := time.Now()
		var err error
		if v == "nbd-live" {
			err = nbdLive(im)
		} else {
			err = restoreVariant(im, v)
		}
		log.Printf("variant %-8s: %v after %v", v, okOrErr(err), time.Since(t0).Round(time.Millisecond))
	}
	return nil
}

func restoreVariant(im *image.Image, variant string) error {
	dir := "/tmp/mantsana-restoreprobe"
	os.RemoveAll(dir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	defer os.RemoveAll(dir)

	spec := &guest.BootSpec{
		Name:         "restoreprobe",
		Manifest:     &im.Manifest,
		CPUs:         im.Warm.CPUs,
		MemoryBytes:  im.Warm.MemoryBytes,
		AuxPath:      filepath.Join(dir, image.AuxFile),
		RestoreState: im.WarmPath(image.StateFile),
		Logf:         log.Printf,
	}
	var cleanups []func()
	defer func() {
		for _, f := range cleanups {
			f()
		}
	}()
	clone := func(src, dst string) error {
		os.Remove(dst)
		if err := image.CloneFile(src, dst); err != nil {
			return err
		}
		cleanups = append(cleanups, func() { os.Remove(dst) })
		return nil
	}
	switch variant {
	case "nbd":
		sock := filepath.Join(dir, "nbd.sock")
		ln, err := net.Listen("unix", sock)
		if err != nil {
			return err
		}
		srv := guestbd.NewServer(guestbd.FileSource(im.WarmPath(image.DiskFile)), guestbd.WithSharedSnapshot(), guestbd.WithMaxMem(0))
		go srv.Serve(ln)
		cleanups = append(cleanups, func() { ln.Close(); srv.Close() })
		spec.NBDURL = "nbd+unix:///?socket=" + sock
		if err := clone(im.WarmPath(image.AuxFile), spec.AuxPath); err != nil {
			return err
		}
	case "clone":
		spec.DiskPath = filepath.Join(dir, image.DiskFile)
		if err := clone(im.WarmPath(image.DiskFile), spec.DiskPath); err != nil {
			return err
		}
		if err := clone(im.WarmPath(image.AuxFile), spec.AuxPath); err != nil {
			return err
		}
	case "samepath":
		// guest.Warm saves from <warm>.tmp and renames it into place.
		tmp := filepath.Clean(im.WarmPath("")) + ".tmp"
		if err := os.MkdirAll(tmp, 0o755); err != nil {
			return err
		}
		cleanups = append(cleanups, func() { os.RemoveAll(tmp) })
		spec.DiskPath = filepath.Join(tmp, image.DiskFile)
		spec.AuxPath = filepath.Join(tmp, image.AuxFile)
		spec.RestoreState = filepath.Join(tmp, image.StateFile)
		for _, f := range []string{image.DiskFile, image.AuxFile, image.StateFile} {
			if err := clone(im.WarmPath(f), filepath.Join(tmp, f)); err != nil {
				return err
			}
		}
	default:
		return fmt.Errorf("unknown variant %q", variant)
	}

	vmSide, link, err := guestnet.NewSocketPairLink()
	if err != nil {
		return err
	}
	defer vmSide.Close()
	n, err := guestnet.New(guestnet.Config{Link: link, GuestMAC: net.HardwareAddr(im.Manifest.MAC), Logf: log.Printf})
	if err != nil {
		link.Close()
		return err
	}
	defer n.Close()
	spec.Net = vmSide

	t0 := time.Now()
	m, err := (&vzvm.Backend{}).Boot(context.Background(), spec)
	if err != nil {
		return err
	}
	defer m.Stop()
	log.Printf("variant %s: restored and resumed in %v; dialing sshd", variant, time.Since(t0).Round(time.Millisecond))
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	for {
		dctx, dcancel := context.WithTimeout(ctx, 2*time.Second)
		c, err := n.DialGuest(dctx, 22)
		dcancel()
		if err == nil {
			c.Close()
			log.Printf("variant %s: sshd reachable %v after restore started", variant, time.Since(t0).Round(time.Millisecond))
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("restored, but sshd unreachable: %v", err)
		case <-m.Done():
			return fmt.Errorf("VM stopped: %v", m.Err())
		case <-time.After(200 * time.Millisecond):
		}
	}
}
