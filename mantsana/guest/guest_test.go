// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

//go:build unix

package guest_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tailscale/tb/guestnet"
	"github.com/tailscale/tb/imds"
	"github.com/tailscale/tb/mantsana/guest"
	"github.com/tailscale/tb/mantsana/guest/guesttest"
	"github.com/tailscale/tb/mantsana/image"
)

func newTestImage(t *testing.T) *image.Image {
	t.Helper()
	s := &image.Store{Root: t.TempDir()}
	im, err := s.Create("img", image.Manifest{
		OS:          "macos",
		CPUs:        4,
		MemoryBytes: 8 << 30,
		MAC:         image.MACAddr(guestnet.DefaultGuestMAC),
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{im.DiskPath(), im.AuxPath()} {
		if err := os.WriteFile(f, make([]byte, 1<<20), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return im
}

func testConfig(t *testing.T, b guest.Backend, im *image.Image) guest.Config {
	// A short directory, since it holds a unix socket.
	dir, err := os.MkdirTemp("", "mg")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return guest.Config{
		Name:     "test-1",
		Slot:     1,
		Image:    im,
		Dir:      filepath.Join(dir, "s1"),
		Backend:  b,
		Resolver: guestnet.StaticResolver{Addr: netip.MustParseAddr("192.0.2.1")},
		UserData: map[string]string{"k": "v"},
		Logf:     t.Logf,
	}
}

func testContext(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// waitEvent waits for an event of kind and returns it.
func waitEvent(t *testing.T, v *guest.VM, kind guest.EventKind) guest.Event {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		for _, ev := range v.Events().Recent() {
			if ev.Kind == kind {
				return ev
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("no %q event; have %+v", kind, v.Events().Recent())
	return guest.Event{}
}

// lastMachine returns the most recently booted fake machine.
func lastMachine(t *testing.T, b *guesttest.Backend) *guesttest.Machine {
	t.Helper()
	ms := b.Machines()
	if len(ms) == 0 {
		t.Fatal("no machines booted")
	}
	return ms[len(ms)-1]
}

// lastSpec returns the most recent Boot call's spec.
func lastSpec(t *testing.T, b *guesttest.Backend) guest.BootSpec {
	t.Helper()
	ss := b.Specs()
	if len(ss) == 0 {
		t.Fatal("no Boot calls")
	}
	return ss[len(ss)-1]
}

// guestMeta fetches IMDS /v0/meta from inside the most recent guest.
func guestMeta(t *testing.T, ctx context.Context, b *guesttest.Backend) imds.Meta {
	t.Helper()
	m := lastMachine(t, b)
	hc := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return m.Guest.DialTCP(ctx, netip.AddrPortFrom(guestnet.IMDSAddr, 80))
		},
	}}
	req, _ := http.NewRequestWithContext(ctx, "GET", "http://169.254.169.254/v0/meta", nil)
	res, err := hc.Do(req)
	if err != nil {
		t.Fatalf("IMDS: %v", err)
	}
	defer res.Body.Close()
	var meta imds.Meta
	if err := json.NewDecoder(res.Body).Decode(&meta); err != nil {
		t.Fatal(err)
	}
	return meta
}

func TestColdBoot(t *testing.T) {
	b := &guesttest.Backend{T: t}
	im := newTestImage(t)
	cfg := testConfig(t, b, im)
	ctx := testContext(t)
	v, err := guest.Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if v.Restored() {
		t.Error("Restored = true")
	}
	if ev := waitEvent(t, v, guest.EventBoot); ev.Msg != "cold" {
		t.Errorf("boot event = %q, want cold", ev.Msg)
	}
	if err := v.WaitReady(ctx, 22); err != nil {
		t.Fatal(err)
	}
	c, err := v.Net().DialGuest(ctx, 22)
	if err != nil {
		t.Fatal(err)
	}
	line, _ := bufio.NewReader(c).ReadString('\n')
	c.Close()
	if line != guesttest.SSHBanner {
		t.Errorf("banner = %q", line)
	}

	meta := guestMeta(t, ctx, b)
	if meta.Name != "test-1" || meta.BootMode != "cold" || meta.CPUs != 4 {
		t.Errorf("meta = %+v", meta)
	}

	// The disk is served over NBD on a unix socket in the VM dir, and the
	// aux file is a private copy there.
	spec := lastSpec(t, b)
	sock, ok := strings.CutPrefix(spec.NBDURL, "nbd+unix:///?socket=")
	if !ok || filepath.Dir(sock) != cfg.Dir {
		t.Fatalf("NBDURL = %q", spec.NBDURL)
	}
	nc, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	magic := make([]byte, 8)
	nc.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.ReadFull(nc, magic); err != nil || string(magic) != "NBDMAGIC" {
		t.Errorf("NBD handshake = %q, %v", magic, err)
	}
	nc.Close()
	if spec.AuxPath != filepath.Join(cfg.Dir, image.AuxFile) {
		t.Errorf("AuxPath = %q", spec.AuxPath)
	}
	if !strings.Contains(string(v.Console()), "fake console") {
		t.Errorf("console = %q", v.Console())
	}

	v.Stop(ctx, false)
	select {
	case <-v.Done():
	default:
		t.Fatal("not done after Stop")
	}
	if err := v.Err(); err != nil {
		t.Errorf("Err = %v", err)
	}
	if _, err := os.Stat(cfg.Dir); !os.IsNotExist(err) {
		t.Errorf("VM dir survived: %v", err)
	}
}

func TestWarmAndRestore(t *testing.T) {
	b := &guesttest.Backend{T: t}
	im := newTestImage(t)
	ctx := testContext(t)

	cfg := testConfig(t, b, im)
	if _, err := guest.Start(ctx, guest.Config{Image: im, Backend: b, Boot: guest.BootRestore, Dir: cfg.Dir}); err == nil {
		t.Fatal("BootRestore without a warm snapshot succeeded")
	}

	wi, err := guest.Warm(ctx, guest.WarmConfig{Config: cfg, Settle: time.Millisecond})
	if err != nil {
		t.Fatalf("Warm: %v", err)
	}
	if wi.HostOSBuild != "25F71" || wi.CPUs != 4 {
		t.Errorf("WarmInfo = %+v", wi)
	}
	if spec := lastSpec(t, b); spec.NBDURL == "" || spec.DiskPath != "" || spec.RestoreState != "" {
		t.Errorf("warm-up spec = %+v; want NBD disk (as for restores), cold boot", spec)
	}
	for _, f := range []string{image.DiskFile, image.AuxFile, image.StateFile, image.WarmFile} {
		if _, err := os.Stat(im.WarmPath(f)); err != nil {
			t.Errorf("warm file: %v", err)
		}
	}

	v, err := guest.Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !v.Restored() {
		t.Fatal("auto boot with a warm snapshot did not restore")
	}
	waitEvent(t, v, guest.EventNetworkUp)
	if spec := lastSpec(t, b); spec.RestoreState != im.WarmPath(image.StateFile) || spec.NBDURL == "" {
		t.Errorf("restore spec = %+v", spec)
	}
	if meta := guestMeta(t, ctx, b); meta.BootMode != "restored" {
		t.Errorf("boot mode = %q", meta.BootMode)
	}
	v.Stop(ctx, true)

	// A transient restore failure (a locked session) falls back to cold
	// boot and leaves the snapshot usable.
	b.SetFailRestore(true, true)
	v, err = guest.Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if v.Restored() {
		t.Error("restored despite failure")
	}
	waitEvent(t, v, guest.EventRestoreFailed)
	v.Stop(ctx, true)
	if ok, why := im.WarmUsable("25F71", 4, 8<<30); !ok {
		t.Errorf("after transient restore failure: not usable: %s", why)
	}

	// Any other failed restore falls back to cold boot and marks the
	// snapshot stale.
	b.SetFailRestore(true, false)
	v, err = guest.Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if v.Restored() {
		t.Error("restored despite failure")
	}
	waitEvent(t, v, guest.EventRestoreFailed)
	v.Stop(ctx, true)
	if ok, why := im.WarmUsable("25F71", 4, 8<<30); ok || !strings.HasPrefix(why, "stale: ") {
		t.Errorf("after failed restore: WarmUsable = %v, %q", ok, why)
	}
}

// TestDefaultMAC checks that images' default MAC is the guest MAC guestnet
// expects by default, which mantsana/image can't import guestnet to share.
func TestDefaultMAC(t *testing.T) {
	if !bytes.Equal(image.DefaultMAC, guestnet.DefaultGuestMAC) {
		t.Errorf("image.DefaultMAC %v != guestnet.DefaultGuestMAC %v", image.DefaultMAC, guestnet.DefaultGuestMAC)
	}
}

func TestBadService(t *testing.T) {
	b := &guesttest.Backend{T: t}
	cfg := testConfig(t, b, newTestImage(t))
	cfg.Services = map[netip.AddrPort]guestnet.Service{
		netip.MustParseAddrPort("8.8.8.8:53"): &guestnet.ForwardService{},
	}
	if _, err := guest.Start(testContext(t), cfg); err == nil {
		t.Fatal("service on a public address accepted")
	}
}
