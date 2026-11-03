// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

//go:build darwin && arm64

package main

import (
	"context"
	"encoding/hex"
	"errors"
	"flag"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/tailscale/tb/guestbd"
	"github.com/tailscale/tb/guestnet"
	"github.com/tailscale/tb/mantsana/guest"
	"github.com/tailscale/tb/mantsana/image"
	"github.com/tailscale/tb/mantsana/vzvm"
)

// cmdNBDProbe boots an image's disk over NBD through a proxy that logs
// every byte exchanged, to debug the framework's NBD client.
func cmdNBDProbe(args []string) error {
	fs := flag.NewFlagSet("nbdprobe", flag.ExitOnError)
	export := fs.String("export", "", "NBD export name to put in the URL")
	dur := fs.Duration("duration", 20*time.Second, "how long to run the VM")
	maxDump := fs.Int("max-dump", 512, "bytes of each direction to hex dump per connection")
	fs.Parse(args)
	if fs.NArg() != 1 {
		return errors.New("usage: nbdprobe [flags] <image-name>")
	}
	im, err := (&image.Store{Root: *imagesDir}).Get(fs.Arg(0))
	if err != nil {
		return err
	}
	dir := "/tmp/mantsana-nbdprobe"
	os.RemoveAll(dir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	defer os.RemoveAll(dir)

	// guestbd on an inner socket; the framework connects to the proxy.
	innerSock := filepath.Join(dir, "inner.sock")
	inner, err := net.Listen("unix", innerSock)
	if err != nil {
		return err
	}
	srv := guestbd.NewServer(guestbd.FileSource(im.DiskPath()), guestbd.WithSharedSnapshot(), guestbd.WithMaxMem(0))
	go srv.Serve(inner)
	defer srv.Close()

	outerSock := filepath.Join(dir, "nbd.sock")
	outer, err := net.Listen("unix", outerSock)
	if err != nil {
		return err
	}
	defer outer.Close()
	go func() {
		for n := 1; ; n++ {
			c, err := outer.Accept()
			if err != nil {
				return
			}
			log.Printf("conn %d: accepted", n)
			go traceProxy(n, c, innerSock, *maxDump)
		}
	}()

	aux := filepath.Join(dir, image.AuxFile)
	if err := image.CloneFile(im.AuxPath(), aux); err != nil {
		return err
	}
	vmSide, link, err := guestnet.NewSocketPairLink()
	if err != nil {
		return err
	}
	defer link.Close()
	defer vmSide.Close()

	url := "nbd+unix:///" + *export + "?socket=" + outerSock
	log.Printf("booting with %s", url)
	b := &vzvm.Backend{}
	m, err := b.Boot(context.Background(), &guest.BootSpec{
		Name:        "nbdprobe",
		Manifest:    &im.Manifest,
		CPUs:        im.Manifest.CPUs,
		MemoryBytes: im.Manifest.MemoryBytes,
		NBDURL:      url,
		AuxPath:     aux,
		Net:         vmSide,
		Console:     os.Stderr,
		Logf:        log.Printf,
	})
	if err != nil {
		return err
	}
	log.Printf("VM started; running for %v", *dur)
	select {
	case <-m.Done():
		log.Printf("VM stopped: %v", m.Err())
	case <-time.After(*dur):
		log.Printf("stopping VM: %v", m.Stop())
	}
	return nil
}

// traceProxy relays c to the unix socket at target, hex dumping the first
// max bytes in each direction.
func traceProxy(n int, c net.Conn, target string, max int) {
	defer c.Close()
	t, err := net.Dial("unix", target)
	if err != nil {
		log.Printf("conn %d: dial guestbd: %v", n, err)
		return
	}
	defer t.Close()
	var wg sync.WaitGroup
	relay := func(dir string, dst, src net.Conn) {
		defer wg.Done()
		d := &dumper{n: n, dir: dir, left: max}
		nb, err := io.Copy(io.MultiWriter(dst, d), src)
		log.Printf("conn %d: %s done after %d bytes: %v", n, dir, nb, err)
		if uc, ok := dst.(*net.UnixConn); ok {
			uc.CloseWrite()
		}
	}
	wg.Add(2)
	go relay("client->server", t, c)
	go relay("server->client", c, t)
	wg.Wait()
}

type dumper struct {
	n    int
	dir  string
	left int
}

func (d *dumper) Write(p []byte) (int, error) {
	if d.left > 0 {
		q := p
		if len(q) > d.left {
			q = q[:d.left]
		}
		d.left -= len(q)
		log.Printf("conn %d: %s %d bytes:\n%s", d.n, d.dir, len(p), hex.Dump(q))
	}
	return len(p), nil
}
