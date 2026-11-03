// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

//go:build unix

// Package guesttest provides a fake guest.Backend for testing code built on
// package guest. Its machines are guestnettest guests attached to the VM end
// of the network socketpair, with the fixed guest address (as if holding a
// lease) and a fake sshd on port 22.
package guesttest

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"sync"
	"syscall"
	"testing"

	"github.com/tailscale/tb/guestnet"
	"github.com/tailscale/tb/guestnet/guestnettest"
	"github.com/tailscale/tb/mantsana/guest"
)

// SSHBanner is what the fake sshd sends on each connection.
const SSHBanner = "SSH-2.0-fake\r\n"

// Backend is a fake guest.Backend.
type Backend struct {
	T testing.TB

	mu                     sync.Mutex
	failRestore, transient bool // see SetFailRestore
	specs                  []guest.BootSpec
	machines               []*Machine
}

var _ guest.Backend = (*Backend)(nil)

// SetFailRestore makes subsequent restores fail. A transient failure is
// like one due to the Mac's session being locked: the snapshot is fine.
func (b *Backend) SetFailRestore(fail, transient bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.failRestore, b.transient = fail, transient
}

// Specs returns the specs of every Boot call so far.
func (b *Backend) Specs() []guest.BootSpec {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]guest.BootSpec(nil), b.specs...)
}

// Machines returns every machine booted so far.
func (b *Backend) Machines() []*Machine {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]*Machine(nil), b.machines...)
}

// HostOSBuild implements guest.Backend.
func (b *Backend) HostOSBuild() string { return "25F71" }

// VMLimit implements guest.Backend.
func (b *Backend) VMLimit() int { return 2 }

// Boot implements guest.Backend.
func (b *Backend) Boot(ctx context.Context, spec *guest.BootSpec) (guest.Machine, error) {
	b.mu.Lock()
	b.specs = append(b.specs, *spec)
	failRestore, transient := b.failRestore, b.transient
	b.mu.Unlock()
	if (spec.NBDURL == "") == (spec.DiskPath == "") {
		return nil, fmt.Errorf("want exactly one of NBDURL and DiskPath; got %q, %q", spec.NBDURL, spec.DiskPath)
	}
	if _, err := os.Stat(spec.AuxPath); err != nil {
		return nil, err
	}
	if spec.RestoreState != "" {
		if failRestore {
			return nil, &guest.RestoreError{Err: errors.New("fake restore failure"), Transient: transient}
		}
		if _, err := os.Stat(spec.RestoreState); err != nil {
			return nil, &guest.RestoreError{Err: err}
		}
	}
	// The caller closes spec.Net after Boot, as the framework dups it.
	fd, err := dupFile(spec.Net)
	if err != nil {
		return nil, err
	}
	link := guestnet.NewFileLink(fd)
	g := guestnettest.New(b.T, link, guestnettest.Config{
		IP:      guestnet.DefaultGuestIP,
		Subnet:  guestnet.DefaultSubnet,
		Gateway: guestnet.DefaultGatewayIP,
		MAC:     net.HardwareAddr(spec.Manifest.MAC),
	})
	ln, err := g.ListenTCP(22)
	if err != nil {
		link.Close()
		return nil, err
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			fmt.Fprint(c, SSHBanner)
			c.Close()
		}
	}()
	if spec.Console != nil {
		fmt.Fprintf(spec.Console, "fake console for %s\n", spec.Name)
	}
	m := &Machine{Guest: g, link: link, ln: ln, done: make(chan struct{})}
	b.mu.Lock()
	b.machines = append(b.machines, m)
	b.mu.Unlock()
	return m, nil
}

// Machine is a fake running VM.
type Machine struct {
	// Guest is the machine's network stack, for dialing out of the guest.
	Guest *guestnettest.Guest

	link *guestnet.FileLink
	ln   net.Listener
	once sync.Once
	done chan struct{}
}

var _ guest.Machine = (*Machine)(nil)

// Done implements guest.Machine.
func (m *Machine) Done() <-chan struct{} { return m.done }

// Err implements guest.Machine.
func (m *Machine) Err() error { return nil }

// RequestStop implements guest.Machine. The fake guest shuts down at once.
func (m *Machine) RequestStop() error { return m.Stop() }

// Stop implements guest.Machine.
func (m *Machine) Stop() error {
	m.once.Do(func() {
		m.ln.Close()
		m.link.Close()
		close(m.done)
	})
	return nil
}

// SaveState implements guest.Machine.
func (m *Machine) SaveState(path string) error {
	return os.WriteFile(path, []byte("fake state"), 0o644)
}

// dupFile dups f as a non-blocking file, as the framework dups the network
// socket it is given. Non-blocking is shared with f, which is fine since
// the caller closes f after Boot.
func dupFile(f *os.File) (*os.File, error) {
	sc, err := f.SyscallConn()
	if err != nil {
		return nil, err
	}
	var nf *os.File
	var derr error
	sc.Control(func(fd uintptr) {
		nfd, err := syscall.Dup(int(fd))
		if err != nil {
			derr = err
			return
		}
		if err := syscall.SetNonblock(nfd, true); err != nil {
			syscall.Close(nfd)
			derr = err
			return
		}
		nf = os.NewFile(uintptr(nfd), "dup")
	})
	return nf, derr
}
