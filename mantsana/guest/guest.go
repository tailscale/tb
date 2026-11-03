// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

//go:build unix

// Package guest runs one guest VM and everything the host provides it: its
// private network (guestnet), metadata service (imds), and disk, served over
// NBD with writes discarded (guestbd). Booting the machine itself is
// delegated to a [Backend], so everything here is portable and tested with a
// fake; the real backend is package vzvm on macOS.
//
// A VM restores from its image's warm snapshot when one is usable, and falls
// back to a cold boot (marking the snapshot stale) if restoring fails.
package guest

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/tailscale/tb/guestbd"
	"github.com/tailscale/tb/guestnet"
	"github.com/tailscale/tb/imds"
	"github.com/tailscale/tb/mantsana/image"
	"tailscale.com/types/logger"
)

// BootMode selects how a VM starts.
type BootMode string

const (
	// BootAuto restores the warm snapshot if it is usable, else cold boots.
	BootAuto BootMode = ""
	// BootCold always cold boots.
	BootCold BootMode = "cold"
	// BootRestore requires a usable warm snapshot.
	BootRestore BootMode = "restore"
)

// Config configures a VM.
type Config struct {
	Name  string
	Slot  int
	Image *image.Image

	// CPUs and MemoryBytes default to the image's.
	CPUs        int
	MemoryBytes uint64

	Boot BootMode

	// Dir is a private directory for the VM's sockets and its copy of the
	// auxiliary storage. Keep it short: it holds a unix socket, and
	// sun_path is limited to 104 bytes on macOS. It is created if needed
	// and removed when the VM is done.
	Dir string

	Backend Backend

	// Services are extra guest-visible services, keyed by guest address.
	// The address must be the gateway, IMDS or DNS address. A service on
	// the IMDS address port 80 replaces the built-in metadata service.
	Services map[netip.AddrPort]guestnet.Service

	// Deny, Dialer and Resolver are passed to guestnet.
	Deny     []netip.Prefix
	Dialer   guestnet.Dialer
	Resolver guestnet.Resolver

	// UserData is served by the metadata service.
	UserData map[string]string
	// Hostname is the host's name, reported by the metadata service.
	Hostname string

	// StopTimeout bounds a graceful stop. Default 30s.
	StopTimeout time.Duration

	// Console, if non-nil, also receives the guest's serial console output
	// (which VM.Console keeps the tail of regardless).
	Console io.Writer

	Logf logger.Logf
	Now  func() time.Time
}

// Event is a notable thing that happened to a VM.
type Event struct {
	Time time.Time `json:"time"`
	Kind EventKind `json:"kind"`
	Msg  string    `json:"msg,omitempty"`
}

// EventKind is the type of an [Event].
type EventKind string

const (
	// EventBoot reports the boot mode actually used: "cold" or "restored".
	EventBoot EventKind = "boot"
	// EventRestoreFailed reports that restoring the warm snapshot failed
	// and the VM fell back to a cold boot. Msg is the error.
	EventRestoreFailed EventKind = "restore-failed"
	// EventNetworkUp reports that the guest has a DHCP lease (cold boot)
	// or its network resumed (restore).
	EventNetworkUp EventKind = "network-up"
	// EventGuestReady reports that the guest posted to IMDS /v0/ready.
	EventGuestReady EventKind = "guest-ready"
	// EventLog is a line the guest posted to IMDS /v0/log.
	EventLog EventKind = "log"
	// EventNet is a notable network event (DHCP, blocked flows).
	EventNet EventKind = "net"
	// EventStopping reports that a stop was requested.
	EventStopping EventKind = "stopping"
	// EventExited reports that the machine stopped. Msg is the error, if
	// any.
	EventExited EventKind = "exited"
)

// VM is a running guest.
type VM struct {
	cfg    Config
	events *guestnet.Log[Event]
	cons   *ringBuffer

	// Set during start, then read-only.
	net     *guestnet.Net
	imds    *imds.Handler
	imdsSrv *http.Server
	nbd     *guestbd.Server
	nbdLn   net.Listener
	machine Machine
	plan    bootPlan // the plan that booted

	mu         sync.Mutex
	restored   bool
	stopping   bool
	err        error
	persist    bool  // persist the disk at teardown (see bootPlan.persistPath)
	persistErr error // result of persisting

	done     chan struct{} // closed when torn down
	downOnce sync.Once
}

// Start creates and boots a VM. It returns once the machine is running.
func Start(ctx context.Context, cfg Config) (*VM, error) {
	cfg.withDefaults()
	plans, err := cfg.plans()
	if err != nil {
		return nil, err
	}
	return start(ctx, cfg, plans)
}

func (c *Config) withDefaults() {
	if c.CPUs == 0 {
		c.CPUs = c.Image.Manifest.CPUs
	}
	if c.MemoryBytes == 0 {
		c.MemoryBytes = c.Image.Manifest.MemoryBytes
	}
	if c.StopTimeout == 0 {
		c.StopTimeout = 30 * time.Second
	}
	if c.Logf == nil {
		c.Logf = logger.Discard
	}
	if c.Now == nil {
		c.Now = time.Now
	}
}

// bootPlan is one attempt at booting.
type bootPlan struct {
	restore   bool   // restore statePath instead of cold booting
	statePath string // saved state, if restore
	diskPath  string // raw disk, served over NBD with writes discarded
	auxSrc    string // aux file to clone to auxPath, or "" to use auxPath in place
	auxPath   string

	// persistPath, if set, is where the NBD disk's writes are persisted
	// (as a clone of diskPath plus the dirty pages) at teardown, if the VM
	// is armed to persist. Warm-up uses this.
	persistPath string
}

// plans returns the boot attempts for c, in order.
func (c *Config) plans() ([]bootPlan, error) {
	im := c.Image
	aux := filepath.Join(c.Dir, image.AuxFile)
	cold := bootPlan{diskPath: im.DiskPath(), auxSrc: im.AuxPath(), auxPath: aux}
	warm := bootPlan{
		restore:   true,
		statePath: im.WarmPath(image.StateFile),
		diskPath:  im.WarmPath(image.DiskFile),
		auxSrc:    im.WarmPath(image.AuxFile),
		auxPath:   aux,
	}
	ok, why := im.WarmUsable(c.Backend.HostOSBuild(), c.CPUs, c.MemoryBytes)
	switch c.Boot {
	case BootCold:
		return []bootPlan{cold}, nil
	case BootRestore:
		if !ok {
			return nil, fmt.Errorf("image %q has no usable warm snapshot: %s", im.Name, why)
		}
		return []bootPlan{warm}, nil
	case BootAuto:
		if ok {
			return []bootPlan{warm, cold}, nil
		}
		c.Logf("guest %s: cold boot: %s", c.Name, why)
		return []bootPlan{cold}, nil
	}
	return nil, fmt.Errorf("unknown boot mode %q", c.Boot)
}

func start(ctx context.Context, cfg Config, plans []bootPlan) (_ *VM, retErr error) {
	v := &VM{
		cfg:    cfg,
		events: guestnet.NewLog[Event](1024),
		cons:   &ringBuffer{max: 1 << 20},
		done:   make(chan struct{}),
	}
	defer func() {
		if retErr != nil {
			v.teardown(retErr)
		}
	}()
	if err := os.MkdirAll(cfg.Dir, 0o700); err != nil {
		return nil, err
	}
	vmSide, link, err := guestnet.NewSocketPairLink()
	if err != nil {
		return nil, err
	}
	defer vmSide.Close() // the framework dups it
	if err := v.startNet(link); err != nil {
		link.Close()
		return nil, err
	}

	for i, p := range plans {
		err = v.boot(ctx, p, vmSide)
		if err == nil {
			break
		}
		var re *RestoreError
		if !errors.As(err, &re) || i == len(plans)-1 {
			return nil, err
		}
		v.logf("%v; falling back to cold boot", err)
		v.event(EventRestoreFailed, err.Error())
		if re.Transient {
			continue // the snapshot is fine; don't mark it stale
		}
		if err := cfg.Image.MarkWarmStale(err.Error()); err != nil {
			v.logf("marking warm snapshot stale: %v", err)
		}
	}

	mode := "cold"
	if v.restored {
		mode = "restored"
	}
	v.event(EventBoot, mode)
	if v.restored {
		v.event(EventNetworkUp, "restored")
	}
	v.imds.SetMeta(func(m *imds.Meta) {
		m.BootMode = mode
		m.BootTime = cfg.Now()
	})
	go v.watch()
	return v, nil
}

func (v *VM) logf(format string, args ...any) {
	v.cfg.Logf("guest %s: "+format, append([]any{v.cfg.Name}, args...)...)
}

func (v *VM) event(kind EventKind, msg string) {
	v.events.Add(Event{Time: v.cfg.Now(), Kind: kind, Msg: msg})
}

// startNet starts the guest network and metadata service on link.
func (v *VM) startNet(link guestnet.FrameLink) error {
	cfg := v.cfg
	ncfg := guestnet.Config{
		Link:     link,
		GuestMAC: net.HardwareAddr(cfg.Image.Manifest.MAC),
		MTU:      cfg.Image.Manifest.MTU,
		Deny:     cfg.Deny,
		Dialer:   cfg.Dialer,
		Resolver: cfg.Resolver,
		Services: map[netip.AddrPort]guestnet.Service{},
		Now:      cfg.Now,
		Logf:     cfg.Logf,
	}
	for ap, svc := range cfg.Services {
		switch ap.Addr() {
		case guestnet.DefaultGatewayIP, guestnet.IMDSAddr, guestnet.DNSAddr:
		default:
			return fmt.Errorf("service address %v must be the gateway, IMDS or DNS address", ap)
		}
		ncfg.Services[ap] = svc
	}

	v.imds = imds.New(imds.Meta{
		Name:        cfg.Name,
		Slot:        cfg.Slot,
		Image:       cfg.Image.Name,
		CPUs:        cfg.CPUs,
		MemoryBytes: cfg.MemoryBytes,
		Host:        cfg.Hostname,
		GuestIP:     guestnet.DefaultGuestIP.String(),
		GatewayIP:   guestnet.DefaultGatewayIP.String(),
	}, cfg.UserData)
	v.imds.Now = cfg.Now
	v.imds.OnLog = func(line string) { v.event(EventLog, line) }
	imdsAP := netip.AddrPortFrom(guestnet.IMDSAddr, 80)
	if _, claimed := ncfg.Services[imdsAP]; !claimed {
		ln := guestnet.NewListenerService("imds")
		ncfg.Services[imdsAP] = ln
		v.imdsSrv = &http.Server{Handler: v.imds, ReadHeaderTimeout: 10 * time.Second}
		go v.imdsSrv.Serve(ln)
	}

	var err error
	if v.net, err = guestnet.New(ncfg); err != nil {
		return err
	}
	go v.relayNet()
	return nil
}

// relayNet turns network and metadata signals into events until the VM is
// torn down.
func (v *VM) relayNet() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		<-v.done
		cancel()
	}()
	netEvents := v.net.Events.Subscribe(ctx)
	leased, ready := v.net.Leased(), v.imds.Ready()
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-netEvents:
			if !ok {
				return
			}
			v.event(EventNet, ev.Kind+": "+ev.Msg)
		case <-leased:
			leased = nil
			if !v.Restored() {
				v.event(EventNetworkUp, "dhcp")
			}
		case <-ready:
			ready = nil
			v.event(EventGuestReady, "")
		}
	}
}

// boot makes one boot attempt. Resources from a failed attempt are
// released before it returns.
func (v *VM) boot(ctx context.Context, p bootPlan, netFile *os.File) error {
	cfg := v.cfg
	spec := &BootSpec{
		Name:        cfg.Name,
		Manifest:    &cfg.Image.Manifest,
		CPUs:        cfg.CPUs,
		MemoryBytes: cfg.MemoryBytes,
		AuxPath:     p.auxPath,
		Net:         netFile,
		Console:     v.consoleWriter(),
		Logf:        cfg.Logf,
	}
	if p.restore {
		spec.RestoreState = p.statePath
	}
	if p.auxSrc != "" {
		os.Remove(p.auxPath)
		if err := image.CloneFile(p.auxSrc, p.auxPath); err != nil {
			return err
		}
	}
	sock := filepath.Join(cfg.Dir, "nbd.sock")
	os.Remove(sock)
	ln, err := net.Listen("unix", sock)
	if err != nil {
		return err
	}
	srv := guestbd.NewServer(guestbd.FileSource(p.diskPath), guestbd.WithSharedSnapshot(), guestbd.WithMaxMem(0))
	go srv.Serve(ln)
	v.nbd, v.nbdLn = srv, ln
	spec.NBDURL = "nbd+unix:///?socket=" + sock
	t0 := time.Now()
	m, err := cfg.Backend.Boot(ctx, spec)
	if err != nil {
		v.closeNBD()
		return err
	}
	v.logf("booted (restore=%v) in %v", p.restore, time.Since(t0).Round(time.Millisecond))
	v.mu.Lock()
	v.machine = m
	v.plan = p
	v.restored = p.restore
	v.mu.Unlock()
	return nil
}

// persistDisk writes the NBD disk's current contents to v.plan.persistPath:
// a clone of the base disk plus the pages the guest wrote. The machine must
// be stopped.
func (v *VM) persistDisk() error {
	p := v.plan
	if v.nbd == nil || p.persistPath == "" {
		return errors.New("VM has no NBD disk to persist")
	}
	t0 := time.Now()
	os.Remove(p.persistPath)
	if err := image.CloneFile(p.diskPath, p.persistPath); err != nil {
		return err
	}
	snap := v.nbd.SharedSnapshot()
	if snap == nil {
		// The disk was never connected, so never written.
		return nil
	}
	f, err := os.OpenFile(p.persistPath, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	pages, err := snap.WriteDirtyTo(f)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("persisting disk: %w", err)
	}
	v.logf("persisted %d written pages to %s in %v", pages, p.persistPath, time.Since(t0).Round(time.Millisecond))
	return nil
}

func (v *VM) closeNBD() {
	if v.nbdLn != nil {
		v.nbdLn.Close()
		v.nbd.Close()
		v.nbd, v.nbdLn = nil, nil
	}
}

// watch waits for the machine to exit and tears down.
func (v *VM) watch() {
	<-v.machine.Done()
	err := v.machine.Err()
	msg := ""
	if err != nil {
		msg = err.Error()
	}
	v.event(EventExited, msg)
	v.teardown(err)
}

// teardown releases everything. It is idempotent.
func (v *VM) teardown(err error) {
	v.downOnce.Do(func() {
		// Log any step that is slow, since a stuck teardown holds up Stop.
		step := func(name string, f func()) {
			t0 := time.Now()
			slow := time.AfterFunc(5*time.Second, func() {
				v.logf("teardown: still %s after 5s; goroutines in %s", name, dumpGoroutines())
			})
			f()
			slow.Stop()
			if d := time.Since(t0); d > time.Second {
				v.logf("teardown: %s took %v", name, d.Round(time.Millisecond))
			}
		}
		if v.imdsSrv != nil {
			step("closing IMDS server", func() { v.imdsSrv.Close() })
		}
		if v.net != nil {
			step("closing network", func() { v.net.Close() })
		}
		v.mu.Lock()
		persist := v.persist
		v.mu.Unlock()
		if persist {
			perr := v.persistDisk()
			v.mu.Lock()
			v.persistErr = perr
			v.mu.Unlock()
		}
		step("closing NBD server", v.closeNBD)
		os.RemoveAll(v.cfg.Dir)
		v.mu.Lock()
		if !v.stopping {
			v.err = err
		}
		v.mu.Unlock()
		close(v.done)
	})
}

// Stop stops the VM, first asking the guest to shut down unless force,
// and waits until it is torn down or ctx is done.
func (v *VM) Stop(ctx context.Context, force bool) {
	v.mu.Lock()
	m := v.machine
	already := v.stopping
	v.stopping = true
	v.mu.Unlock()
	if !already {
		v.event(EventStopping, "")
	}
	if !force {
		if err := m.RequestStop(); err != nil {
			v.logf("RequestStop: %v", err)
		} else {
			t := time.NewTimer(v.cfg.StopTimeout)
			select {
			case <-m.Done():
			case <-t.C:
				v.logf("guest did not shut down within %v; stopping", v.cfg.StopTimeout)
			case <-ctx.Done():
			}
			t.Stop()
		}
	}
	select {
	case <-m.Done():
	default:
		t0 := time.Now()
		if err := m.Stop(); err != nil {
			v.logf("Stop: %v", err)
		}
		if d := time.Since(t0); d > time.Second {
			v.logf("stopping the machine took %v", d.Round(time.Millisecond))
		}
	}
	t0 := time.Now()
	select {
	case <-m.Done():
	case <-ctx.Done():
		v.logf("machine not stopped after %v: %v", time.Since(t0).Round(time.Millisecond), ctx.Err())
		return
	}
	if d := time.Since(t0); d > time.Second {
		v.logf("machine reported stopped %v after Stop", d.Round(time.Millisecond))
	}
	select {
	case <-v.done:
	case <-ctx.Done():
		v.logf("teardown not done: %v", ctx.Err())
	}
}

// Done returns a channel closed once the VM has stopped and its resources
// are released.
func (v *VM) Done() <-chan struct{} { return v.done }

// Err returns why the VM stopped: nil after a clean guest shutdown or a
// requested Stop. It is valid after Done is closed.
func (v *VM) Err() error {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.err
}

// Restored reports whether the VM was restored from a warm snapshot rather
// than cold booted.
func (v *VM) Restored() bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.restored
}

// Net returns the guest's network.
func (v *VM) Net() *guestnet.Net { return v.net }

// IMDS returns the guest's metadata service.
func (v *VM) IMDS() *imds.Handler { return v.imds }

// Events returns the VM's event log.
func (v *VM) Events() *guestnet.Log[Event] { return v.events }

// Console returns the most recent serial console output.
func (v *VM) Console() []byte { return v.cons.Bytes() }

// consoleWriter returns where the backend writes serial console output.
func (v *VM) consoleWriter() io.Writer {
	if v.cfg.Console == nil {
		return v.cons
	}
	return io.MultiWriter(v.cons, v.cfg.Console)
}

// WaitReady waits until the guest is usable: it has posted to IMDS
// /v0/ready, or a TCP connection to port on the guest succeeds. Port 0
// means only the IMDS signal counts.
func (v *VM) WaitReady(ctx context.Context, port uint16) error {
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		if port != 0 {
			dctx, cancel := context.WithTimeout(ctx, 2*time.Second)
			c, err := v.net.DialGuest(dctx, port)
			cancel()
			if err == nil {
				c.Close()
				return nil
			}
		}
		select {
		case <-v.imds.Ready():
			return nil
		case <-v.done:
			return fmt.Errorf("guest exited before it was ready: %v", v.Err())
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
		}
	}
}

// ringBuffer keeps the last max bytes written to it.
type ringBuffer struct {
	mu  sync.Mutex
	max int
	b   []byte
}

func (r *ringBuffer) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.b = append(r.b, p...)
	if over := len(r.b) - r.max; over > 0 {
		r.b = append(r.b[:0], r.b[over:]...)
	}
	return len(p), nil
}

func (r *ringBuffer) Bytes() []byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]byte(nil), r.b...)
}
