// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package image

import (
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
)

func TestStoreRoundTrip(t *testing.T) {
	s := &Store{Root: t.TempDir()}
	mac, _ := net.ParseMAC("02:76:7a:00:00:02")
	im, err := s.Create("macos-test", Manifest{
		OS:            "macos",
		Arch:          "arm64",
		HardwareModel: []byte("hw"),
		MachineID:     []byte("id"),
		MAC:           MACAddr(mac),
		CPUs:          4,
		MemoryBytes:   8 << 30,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Create("macos-test", Manifest{}); err == nil {
		t.Fatal("duplicate Create succeeded")
	}

	got, err := s.Get("macos-test")
	if err != nil {
		t.Fatal(err)
	}
	if got.Manifest.MAC.String() != "02:76:7a:00:00:02" {
		t.Errorf("MAC = %v", got.Manifest.MAC)
	}
	if got.Manifest.Display != DefaultDisplay {
		t.Errorf("Display = %+v, want default", got.Manifest.Display)
	}
	if got.Manifest.MTU != 1500 {
		t.Errorf("MTU = %d, want 1500", got.Manifest.MTU)
	}
	if got.Warm != nil {
		t.Errorf("Warm = %+v, want nil", got.Warm)
	}

	// The MAC is encoded as text.
	b, _ := os.ReadFile(filepath.Join(im.Dir, ManifestFile))
	var raw map[string]any
	json.Unmarshal(b, &raw)
	if raw["mac"] != "02:76:7a:00:00:02" {
		t.Errorf("mac in JSON = %v", raw["mac"])
	}

	list, err := s.List()
	if err != nil || len(list) != 1 || list[0].Name != "macos-test" {
		t.Fatalf("List = %v, %v", list, err)
	}

	if _, err := s.Get("nope"); err == nil {
		t.Fatal("Get missing succeeded")
	} else {
		var nf *NotFoundError
		if !errors.As(err, &nf) {
			t.Errorf("err = %v, want NotFoundError", err)
		}
	}
	if _, err := s.Get("../etc"); err == nil {
		t.Fatal("Get with path traversal succeeded")
	}
}

func TestWarm(t *testing.T) {
	s := &Store{Root: t.TempDir()}
	im, err := s.Create("img", Manifest{OS: "macos", CPUs: 4, MemoryBytes: 1 << 30})
	if err != nil {
		t.Fatal(err)
	}
	if ok, why := im.WarmUsable("25A354", 4, 1<<30); ok || why != "no warm snapshot" {
		t.Fatalf("WarmUsable = %v, %q", ok, why)
	}

	// Warm info without the files is not usable and is not loaded.
	hash, _ := im.ManifestHash()
	wi := &WarmInfo{HostOSBuild: "25A354", ManifestHash: hash, CPUs: 4, MemoryBytes: 1 << 30}
	if err := im.SaveWarmInfo(wi); err != nil {
		t.Fatal(err)
	}
	if got, err := s.Get("img"); err == nil || got != nil {
		t.Fatalf("Get with incomplete warm dir = %v, %v; want error", got, err)
	}
	for _, f := range []string{DiskFile, AuxFile, StateFile} {
		os.WriteFile(im.WarmPath(f), []byte("x"), 0o644)
	}
	got, err := s.Get("img")
	if err != nil {
		t.Fatal(err)
	}
	if ok, why := got.WarmUsable("25A354", 4, 1<<30); !ok {
		t.Errorf("WarmUsable = false: %s", why)
	}
	if ok, _ := got.WarmUsable("26A100", 4, 1<<30); ok {
		t.Error("usable after host OS change")
	}
	if ok, _ := got.WarmUsable("25A354", 8, 1<<30); ok {
		t.Error("usable with different CPU count")
	}
	if err := got.MarkWarmStale("restore failed"); err != nil {
		t.Fatal(err)
	}
	got, _ = s.Get("img")
	if ok, why := got.WarmUsable("25A354", 4, 1<<30); ok || why != "stale: restore failed" {
		t.Errorf("after stale: %v %q", ok, why)
	}

	// Changing the manifest invalidates the snapshot.
	got.Warm.Stale = false
	got.SaveWarmInfo(got.Warm)
	m := got.Manifest
	m.Description = "changed"
	SaveManifest(filepath.Join(got.Dir, ManifestFile), &m)
	got, _ = s.Get("img")
	if ok, why := got.WarmUsable("25A354", 4, 1<<30); ok || why != "image manifest changed since snapshot" {
		t.Errorf("after manifest change: %v %q", ok, why)
	}

	if err := got.DeleteWarm(); err != nil {
		t.Fatal(err)
	}
	got, _ = s.Get("img")
	if got.Warm != nil {
		t.Error("warm survived DeleteWarm")
	}
}

func TestCloneFile(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	os.WriteFile(src, []byte("hello"), 0o644)
	dst := filepath.Join(dir, "dst")
	if err := CloneFile(src, dst); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(dst)
	if string(b) != "hello" {
		t.Fatalf("dst = %q", b)
	}
	if err := CloneFile(src, dst); err == nil {
		t.Fatal("clone over existing file succeeded")
	}
}
