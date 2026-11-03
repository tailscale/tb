// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

//go:build darwin && arm64

package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/tailscale/tb/guestnet"
	"github.com/tailscale/tb/mantsana/guest"
	"github.com/tailscale/tb/mantsana/image"
	"github.com/tailscale/tb/mantsana/vzvm"
)

func cmdSelftest(args []string) error {
	fs := flag.NewFlagSet("selftest", flag.ExitOnError)
	validateOnly := fs.Bool("validate-only", false, "only validate configurations; don't boot")
	noSerial := fs.Bool("no-serial", false, "omit the serial console device")
	skipCold := fs.Bool("skip-cold", false, "skip the plain cold boot")
	skipWarm := fs.Bool("skip-warm", false, "skip building the warm snapshot; restore the existing one")
	readyTimeout := fs.Duration("ready-timeout", 5*time.Minute, "how long to wait for sshd in the guest")
	settle := fs.Duration("settle", 10*time.Second, "how long to let the guest settle before saving warm state")
	fs.Parse(args)
	if fs.NArg() != 1 {
		return errors.New("usage: selftest [flags] <image-name>")
	}
	s := &image.Store{Root: *imagesDir}
	im, err := s.Get(fs.Arg(0))
	if err != nil {
		return err
	}
	b := &vzvm.Backend{NoSerial: *noSerial}
	log.Printf("host OS build %s; image %q: %d CPUs, %d MiB, display %+v", b.HostOSBuild(), im.Name, im.Manifest.CPUs, im.Manifest.MemoryBytes>>20, im.Manifest.Display)

	if err := validateMatrix(im); err != nil {
		return err
	}
	if *validateOnly {
		return nil
	}

	// On a signal (including SIGHUP when the ssh session running us goes
	// away), stop waiting so the VM is stopped and cleaned up.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	dir := "/tmp/mantsana-selftest"
	cfg := guest.Config{
		Name:    "selftest",
		Slot:    1,
		Image:   im,
		Dir:     filepath.Join(dir, "s1"),
		Backend: b,
		Logf:    log.Printf,
	}

	if !*skipCold {
		log.Printf("=== cold boot over NBD")
		c := cfg
		c.Boot = guest.BootCold
		if err := bootAndProbe(ctx, c, *readyTimeout); err != nil {
			return fmt.Errorf("cold boot: %w", err)
		}
	}

	if !*skipWarm {
		log.Printf("=== warm-up (cold boot over NBD, then save state and persist the disk)")
		t0 := time.Now()
		wctx, cancel := context.WithTimeout(ctx, *readyTimeout+2*time.Minute)
		wi, err := guest.Warm(wctx, guest.WarmConfig{Config: cfg, Settle: *settle})
		cancel()
		if err != nil {
			return fmt.Errorf("warm-up: %w", err)
		}
		log.Printf("warm snapshot built in %v: %+v", time.Since(t0).Round(time.Millisecond), wi)
		if fi, err := os.Stat(im.WarmPath(image.StateFile)); err == nil {
			log.Printf("state file is %d MiB", fi.Size()>>20)
		}
	}

	log.Printf("=== restore over NBD")
	im, err = s.Get(im.Name) // reload warm info
	if err != nil {
		return err
	}
	c := cfg
	c.Image = im
	c.Boot = guest.BootRestore
	if err := bootAndProbe(ctx, c, *readyTimeout); err != nil {
		return fmt.Errorf("restore: %w", err)
	}
	log.Printf("=== selftest passed")
	return nil
}

// validateMatrix reports whether each disk attachment and console variant
// validates and supports save/restore, without booting. This is DESIGN.md
// section 9 question 1.
func validateMatrix(im *image.Image) error {
	dir, err := os.MkdirTemp("", "mantsana-validate")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	aux := filepath.Join(dir, image.AuxFile)
	if err := image.CloneFile(im.AuxPath(), aux); err != nil {
		return err
	}
	vmSide, link, err := guestnet.NewSocketPairLink()
	if err != nil {
		return err
	}
	defer vmSide.Close()
	defer link.Close()

	for _, noSerial := range []bool{false, true} {
		for _, disk := range []string{"nbd", "disk-image"} {
			spec := &guest.BootSpec{
				Name:        "validate",
				Manifest:    &im.Manifest,
				CPUs:        im.Manifest.CPUs,
				MemoryBytes: im.Manifest.MemoryBytes,
				AuxPath:     aux,
				Net:         vmSide,
				Logf:        log.Printf,
			}
			if disk == "nbd" {
				spec.NBDURL = "nbd+unix:///?socket=" + filepath.Join(dir, "nbd.sock")
			} else {
				spec.DiskPath = im.DiskPath()
			}
			b := &vzvm.Backend{NoSerial: noSerial}
			v, err := b.Validate(spec)
			if err != nil {
				log.Printf("validate disk=%s serial=%v: building config: %v", disk, !noSerial, err)
				continue
			}
			log.Printf("validate disk=%-10s serial=%-5v: valid=%v save/restore=%v", disk, !noSerial, okOrErr(v.Valid), okOrErr(v.SaveRestore))
		}
	}
	return nil
}

func okOrErr(err error) string {
	if err == nil {
		return "ok"
	}
	return err.Error()
}

// bootAndProbe starts a VM, waits for sshd, reads its banner, reports
// timings, and stops it.
func bootAndProbe(ctx context.Context, cfg guest.Config, readyTimeout time.Duration) error {
	t0 := time.Now()
	v, err := guest.Start(ctx, cfg)
	if err != nil {
		return err
	}
	tBoot := time.Since(t0)
	defer func() {
		sctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		t := time.Now()
		v.Stop(sctx, true)
		log.Printf("stopped in %v (err=%v)", time.Since(t).Round(time.Millisecond), v.Err())
	}()
	go logEvents(ctx, v, t0)

	rctx, cancel := context.WithTimeout(ctx, readyTimeout)
	defer cancel()
	if err := v.WaitReady(rctx, 22); err != nil {
		return fmt.Errorf("waiting for sshd: %w (console: %q)", err, v.Console())
	}
	tReady := time.Since(t0)
	c, err := v.Net().DialGuest(rctx, 22)
	if err != nil {
		return err
	}
	c.SetDeadline(time.Now().Add(10 * time.Second))
	banner, err := bufio.NewReader(c).ReadString('\n')
	c.Close()
	if err != nil {
		return fmt.Errorf("reading ssh banner: %w", err)
	}
	log.Printf("restored=%v boot=%v ssh-ready=%v banner=%q",
		v.Restored(), tBoot.Round(time.Millisecond), tReady.Round(time.Millisecond), banner)
	if err := checkGuestClock(rctx, v); err != nil {
		log.Printf("checking guest clock: %v", err)
	}
	for _, f := range v.Net().Flows.Recent() {
		if f.Closed() {
			log.Printf("  flow %s %s -> %s kind=%s in=%d out=%d %s", f.Proto, f.Src, f.Dst, f.Kind, f.BytesIn, f.BytesOut, f.Err)
		}
	}
	for _, d := range v.Net().DNS.Recent() {
		log.Printf("  dns %s %s -> %s (%v)", d.Name, d.Type, d.RCode, d.Duration)
	}
	return nil
}

func logEvents(ctx context.Context, v *guest.VM, t0 time.Time) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		<-v.Done()
		cancel()
	}()
	for _, ev := range v.Events().Recent() {
		log.Printf("  +%v event %s %s", ev.Time.Sub(t0).Round(time.Millisecond), ev.Kind, ev.Msg)
	}
	for ev := range v.Events().Subscribe(ctx) {
		log.Printf("  +%v event %s %s", ev.Time.Sub(t0).Round(time.Millisecond), ev.Kind, ev.Msg)
	}
}
