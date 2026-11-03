// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

//go:build darwin && arm64

package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/tailscale/tb/guestbd"
	"github.com/tailscale/tb/guestnet"
	"github.com/tailscale/tb/mantsana/guest"
	"github.com/tailscale/tb/mantsana/image"
	"github.com/tailscale/tb/mantsana/vzvm"
)

// nbdLive cold boots the image's pristine disk over NBD, saves its state,
// and restores it over the same NBD server, whose shared snapshot still
// holds the first boot's writes. It checks whether a state saved with an
// NBD attachment restores with one.
func nbdLive(im *image.Image) error {
	dir := "/tmp/mantsana-restoreprobe"
	os.RemoveAll(dir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	sock := filepath.Join(dir, "nbd.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		return err
	}
	defer ln.Close()
	srv := guestbd.NewServer(guestbd.FileSource(im.DiskPath()), guestbd.WithSharedSnapshot(), guestbd.WithMaxMem(0))
	go srv.Serve(ln)
	defer srv.Close()
	aux := filepath.Join(dir, image.AuxFile)
	if err := image.CloneFile(im.AuxPath(), aux); err != nil {
		return err
	}
	state := filepath.Join(dir, image.StateFile)

	boot := func(restore string) (guest.Machine, *guestnet.Net, func(), error) {
		vmSide, link, err := guestnet.NewSocketPairLink()
		if err != nil {
			return nil, nil, nil, err
		}
		n, err := guestnet.New(guestnet.Config{Link: link, GuestMAC: net.HardwareAddr(im.Manifest.MAC), Logf: log.Printf})
		if err != nil {
			vmSide.Close()
			link.Close()
			return nil, nil, nil, err
		}
		m, err := (&vzvm.Backend{}).Boot(context.Background(), &guest.BootSpec{
			Name:         "nbd-live",
			Manifest:     &im.Manifest,
			CPUs:         im.Manifest.CPUs,
			MemoryBytes:  im.Manifest.MemoryBytes,
			NBDURL:       "nbd+unix:///?socket=" + sock,
			AuxPath:      aux,
			Net:          vmSide,
			RestoreState: restore,
			Logf:         log.Printf,
		})
		vmSide.Close()
		if err != nil {
			n.Close()
			return nil, nil, nil, err
		}
		return m, n, func() { m.Stop(); <-m.Done(); n.Close() }, nil
	}
	waitSSH := func(n *guestnet.Net, t0 time.Time) error {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		for {
			dctx, dcancel := context.WithTimeout(ctx, 2*time.Second)
			c, err := n.DialGuest(dctx, 22)
			dcancel()
			if err == nil {
				c.Close()
				log.Printf("nbd-live: sshd reachable after %v", time.Since(t0).Round(time.Millisecond))
				return nil
			}
			if ctx.Err() != nil {
				return err
			}
			time.Sleep(200 * time.Millisecond)
		}
	}

	t0 := time.Now()
	m, n, stop, err := boot("")
	if err != nil {
		return fmt.Errorf("cold boot: %w", err)
	}
	if err := waitSSH(n, t0); err != nil {
		stop()
		return err
	}
	time.Sleep(10 * time.Second)
	t1 := time.Now()
	if err := m.SaveState(state); err != nil {
		stop()
		return fmt.Errorf("save: %w", err)
	}
	log.Printf("nbd-live: saved in %v", time.Since(t1).Round(time.Millisecond))
	stop()

	t2 := time.Now()
	_, n, stop, err = boot(state)
	if err != nil {
		return err
	}
	defer stop()
	log.Printf("nbd-live: restored and resumed in %v", time.Since(t2).Round(time.Millisecond))
	return waitSSH(n, t2)
}
