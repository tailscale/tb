// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package vmimage

import (
	"bytes"
	jsonv1 "encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tailscale/tb/ci/guestlettype"
)

func writeImageDir(t *testing.T, root, osDir, vmm, ver string, m Manifest, files ...string) string {
	t.Helper()
	dir := filepath.Join(root, osDir, vmm, ver)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if err := os.WriteFile(filepath.Join(dir, f), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	b, err := jsonv1.MarshalIndent(m, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestResolve(t *testing.T) {
	root := t.TempDir()
	linuxDir := writeImageDir(t, root, "linux", "firecracker", "2026-05-29T041300Z-aaaa",
		Manifest{
			OS:         guestlettype.OSLinux,
			Hypervisor: "firecracker",
			Channel:    ChannelStable,
			DiskFile:   "disk.qcow2", KernelFile: "kernel",
			Firecracker: &FirecrackerInfo{KernelVersion: "6.8.0"},
		}, "disk.qcow2", "kernel")
	// Windows with a snapshot; FreeBSD cold-boot (no snapshot).
	winDir := writeImageDir(t, root, "windows", "qemu", "2026-05-20T000000Z-bbbb",
		Manifest{
			OS:         guestlettype.OSWindows,
			Hypervisor: "qemu",
			Channel:    ChannelStable,
			DiskFile:   "disk.qcow2", SnapshotFile: "vm-state.bin",
		}, "disk.qcow2", "vm-state.bin")
	bsdDir := writeImageDir(t, root, "freebsd", "qemu", "2026-04-21T000000Z-cccc",
		Manifest{
			OS:         guestlettype.OSFreeBSD,
			Hypervisor: "qemu",
			Channel:    ChannelStable,
			DiskFile:   "disk.qcow2",
		}, "disk.qcow2")

	t.Run("linux", func(t *testing.T) {
		img, err := ResolveVersion(root, guestlettype.OSLinux, VersionStable)
		if err != nil {
			t.Fatal(err)
		}
		if img.Disk != filepath.Join(linuxDir, "disk.qcow2") || img.Kernel != filepath.Join(linuxDir, "kernel") {
			t.Errorf("linux image = %+v", img)
		}
		if img.Snapshot != "" {
			t.Errorf("linux snapshot = %q, want empty", img.Snapshot)
		}
		if img.Manifest.Firecracker == nil || img.Manifest.Firecracker.KernelVersion != "6.8.0" {
			t.Errorf("linux manifest firecracker = %+v, want kernel_version 6.8.0", img.Manifest.Firecracker)
		}
	})

	t.Run("windows-with-snapshot", func(t *testing.T) {
		img, err := ResolveVersion(root, guestlettype.OSWindows, VersionStable)
		if err != nil {
			t.Fatal(err)
		}
		if img.Disk != filepath.Join(winDir, "disk.qcow2") || img.Snapshot != filepath.Join(winDir, "vm-state.bin") {
			t.Errorf("windows image = %+v", img)
		}
		if img.Kernel != "" {
			t.Errorf("windows kernel = %q, want empty", img.Kernel)
		}
	})

	t.Run("freebsd-cold-boot", func(t *testing.T) {
		// QEMU guests only optionally ship a snapshot; a manifest without one
		// resolves with Snapshot empty to cold-boot.
		img, err := ResolveVersion(root, guestlettype.OSFreeBSD, VersionStable)
		if err != nil {
			t.Fatal(err)
		}
		if img.Disk != filepath.Join(bsdDir, "disk.qcow2") {
			t.Errorf("freebsd disk = %q", img.Disk)
		}
		if img.Snapshot != "" {
			t.Errorf("freebsd snapshot = %q, want empty (cold boot)", img.Snapshot)
		}
	})
}

func TestResolveIgnoresIncompleteDirs(t *testing.T) {
	root := t.TempDir()
	linuxManifest := Manifest{
		OS:          guestlettype.OSLinux,
		Hypervisor:  "firecracker",
		Channel:     ChannelStable,
		DiskFile:    "disk.qcow2",
		KernelFile:  "kernel",
		Firecracker: &FirecrackerInfo{KernelVersion: "6.8.0"},
	}
	// Newest dir has no kernel file; next has no disk file; both are incomplete
	// and skipped in favour of the older complete pair.
	writeImageDir(t, root, "linux", "firecracker", "2026-05-29T041300Z-nokernel", linuxManifest, "disk.qcow2")
	writeImageDir(t, root, "linux", "firecracker", "2026-05-28T000000Z-nodisk", linuxManifest, "kernel")
	want := writeImageDir(t, root, "linux", "firecracker", "2026-05-20T000000Z-good", linuxManifest, "disk.qcow2", "kernel")
	writeImageDir(t, root, "linux", "firecracker", "2026-05-12T041351Z-older", linuxManifest, "disk.qcow2", "kernel")
	// A stray file whose name sorts after every dir is ignored.
	if err := os.WriteFile(filepath.Join(root, "linux", "firecracker", "2026-06-01T000000Z-file"), nil, 0o644); err != nil {
		t.Fatal(err)
	}

	img, err := ResolveVersion(root, guestlettype.OSLinux, VersionStable)
	if err != nil {
		t.Fatal(err)
	}
	if img.Disk != filepath.Join(want, "disk.qcow2") || img.Kernel != filepath.Join(want, "kernel") {
		t.Errorf("Resolve = %+v, want disk/kernel under %q", img, want)
	}
}

func TestResolveManifestVersion(t *testing.T) {
	const ver = "2026-05-20T000000Z-bbbbbbbbbb"
	linuxManifest := func(version string) Manifest {
		return Manifest{
			OS:          guestlettype.OSLinux,
			Hypervisor:  "firecracker",
			Channel:     ChannelStable,
			DiskFile:    "disk.qcow2",
			KernelFile:  "kernel",
			Version:     version,
			Firecracker: &FirecrackerInfo{KernelVersion: "6.8.0"},
		}
	}

	t.Run("recorded", func(t *testing.T) {
		root := t.TempDir()
		writeImageDir(t, root, "linux", "firecracker", ver, linuxManifest(ver), "disk.qcow2", "kernel")

		img, err := ResolveVersion(root, guestlettype.OSLinux, VersionStable)
		if err != nil {
			t.Fatal(err)
		}
		if img.Manifest.Version != ver {
			t.Errorf("Version = %q, want %q", img.Manifest.Version, ver)
		}
	})

	t.Run("absent", func(t *testing.T) {
		// Every image already in the bucket predates the field.
		root := t.TempDir()
		writeImageDir(t, root, "linux", "firecracker", ver, linuxManifest(""), "disk.qcow2", "kernel")

		img, err := ResolveVersion(root, guestlettype.OSLinux, VersionStable)
		if err != nil {
			t.Fatal(err)
		}
		if img.Manifest.Version != ver {
			t.Errorf("Version = %q, want it filled in from the directory as %q", img.Manifest.Version, ver)
		}
	})

	t.Run("disagrees-with-directory", func(t *testing.T) {
		// Serving this would misreport which build the guest booted.
		root := t.TempDir()
		writeImageDir(t, root, "linux", "firecracker", ver, linuxManifest("2026-01-01T000000Z-dddddddddd"), "disk.qcow2", "kernel")

		if _, err := ResolveVersion(root, guestlettype.OSLinux, VersionStable); err == nil {
			t.Error("Resolve accepted a dir whose manifest names another version, want error")
		}
	})
}

// TestResolveSkipsUnstable is the load-bearing test for publishing unstable
// images: they share the stable prefix, and an unstable dir sorts *after* the
// same dir without the suffix, so nothing about the sort order keeps one out.
// If this fails, an unstable push serves the entire CI fleet.
func TestResolveSkipsUnstable(t *testing.T) {
	linuxManifest := func(ch Channel) Manifest {
		return Manifest{
			OS:          guestlettype.OSLinux,
			Hypervisor:  "firecracker",
			DiskFile:    "disk.qcow2",
			KernelFile:  "kernel",
			Channel:     ch,
			Firecracker: &FirecrackerInfo{KernelVersion: "6.8.0"},
		}
	}

	t.Run("newest-is-unstable", func(t *testing.T) {
		root := t.TempDir()
		writeImageDir(t, root, "linux", "firecracker", "2026-05-29T041300Z-aaaaaaaaaa-unstable", linuxManifest(ChannelUnstable), "disk.qcow2", "kernel")
		want := writeImageDir(t, root, "linux", "firecracker", "2026-05-20T000000Z-bbbbbbbbbb", linuxManifest(ChannelStable), "disk.qcow2", "kernel")

		img, err := ResolveVersion(root, guestlettype.OSLinux, VersionStable)
		if err != nil {
			t.Fatal(err)
		}
		if img.Disk != filepath.Join(want, "disk.qcow2") {
			t.Errorf("Resolve picked %q, want the older stable image under %q", img.Disk, want)
		}
	})

	t.Run("same-timestamp", func(t *testing.T) {
		// The suffixed name is a strict superstring, so it sorts first under the
		// descending sort even at an identical timestamp.
		root := t.TempDir()
		writeImageDir(t, root, "linux", "firecracker", "2026-05-29T041300Z-aaaaaaaaaa-unstable", linuxManifest(ChannelUnstable), "disk.qcow2", "kernel")
		want := writeImageDir(t, root, "linux", "firecracker", "2026-05-29T041300Z-aaaaaaaaaa", linuxManifest(ChannelStable), "disk.qcow2", "kernel")

		img, err := ResolveVersion(root, guestlettype.OSLinux, VersionStable)
		if err != nil {
			t.Fatal(err)
		}
		if img.Disk != filepath.Join(want, "disk.qcow2") {
			t.Errorf("Resolve picked %q, want the stable sibling under %q", img.Disk, want)
		}
	})

	t.Run("only-unstable", func(t *testing.T) {
		// Better to fail to boot than to boot an unstable image.
		root := t.TempDir()
		writeImageDir(t, root, "linux", "firecracker", "2026-05-29T041300Z-aaaaaaaaaa-unstable", linuxManifest(ChannelUnstable), "disk.qcow2", "kernel")
		if _, err := ResolveVersion(root, guestlettype.OSLinux, VersionStable); err == nil {
			t.Error("Resolve succeeded with only an unstable image present, want error")
		}
	})

	t.Run("mismatched-name-and-manifest", func(t *testing.T) {
		// Either half of the marker being wrong makes the dir malformed, and a
		// malformed dir is skipped rather than trusted. Promoting an unstable
		// image therefore takes two independent mistakes.
		for _, tt := range []struct {
			name    string
			ver     string
			channel Channel
		}{
			{"suffix-without-manifest-channel", "2026-05-29T041300Z-aaaaaaaaaa-unstable", ChannelStable},
			{"manifest-channel-without-suffix", "2026-05-29T041300Z-aaaaaaaaaa", ChannelUnstable},
			{"suffix-with-empty-manifest-channel", "2026-05-29T041300Z-aaaaaaaaaa-unstable", Channel("")},
			{"empty-manifest-channel", "2026-05-29T041300Z-aaaaaaaaaa", Channel("")},
		} {
			t.Run(tt.name, func(t *testing.T) {
				root := t.TempDir()
				writeImageDir(t, root, "linux", "firecracker", tt.ver, linuxManifest(tt.channel), "disk.qcow2", "kernel")
				if _, err := ResolveVersion(root, guestlettype.OSLinux, VersionStable); err == nil {
					t.Error("Resolve accepted a dir whose name and manifest disagree, want error")
				}
			})
		}
	})

	t.Run("stable-reports-its-version", func(t *testing.T) {
		root := t.TempDir()
		const ver = "2026-05-20T000000Z-bbbbbbbbbb"
		writeImageDir(t, root, "linux", "firecracker", ver, linuxManifest(ChannelStable), "disk.qcow2", "kernel")

		img, err := ResolveVersion(root, guestlettype.OSLinux, VersionStable)
		if err != nil {
			t.Fatal(err)
		}
		if img.Manifest.Version != ver {
			t.Errorf("Version = %q, want %q", img.Manifest.Version, ver)
		}
	})
}

// TestResolveSkipsManifestWithoutChannel covers a manifest written before the
// channel field existed. The field is required as of 2026-08-18, so manifests
// without an explicit channel are ignored.
func TestResolveSkipsManifestWithoutChannel(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "linux", "firecracker", "2026-05-20T000000Z-bbbbbbbbbb")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"disk.qcow2", "kernel"} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	manifest := `{"schema":1,"os":"linux","disk_file":"disk.qcow2","kernel_file":"kernel","hypervisor":"firecracker"}`
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := ResolveVersion(root, guestlettype.OSLinux, VersionStable); err == nil {
		t.Error("Resolve served a pre-channel manifest, want it skipped as malformed")
	}
}

func TestResolveErrors(t *testing.T) {
	t.Run("missing-tree", func(t *testing.T) {
		if _, err := ResolveVersion(t.TempDir(), guestlettype.OSLinux, VersionStable); !errors.Is(err, ErrImageNotFound) {
			t.Errorf("want ErrImageNotFound when no image tree is present, got %v", err)
		}
	})

	t.Run("no-manifest", func(t *testing.T) {
		// A dir with the image files but no manifest.json is not resolvable.
		root := t.TempDir()
		dir := filepath.Join(root, "linux", "firecracker", "2026-05-29T041300Z-nomanifest")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		for _, f := range []string{"disk.qcow2", "kernel"} {
			if err := os.WriteFile(filepath.Join(dir, f), []byte("x"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := ResolveVersion(root, guestlettype.OSLinux, VersionStable); !errors.Is(err, ErrImageNotFound) {
			t.Errorf("want ErrImageNotFound when no manifest.json is present, got %v", err)
		}
	})

	t.Run("only-incomplete", func(t *testing.T) {
		root := t.TempDir()
		writeImageDir(t, root, "linux", "firecracker", "2026-05-29T041300Z-nokernel",
			Manifest{
				OS:          guestlettype.OSLinux,
				Hypervisor:  "firecracker",
				Channel:     ChannelStable,
				DiskFile:    "disk.qcow2",
				KernelFile:  "kernel",
				Firecracker: &FirecrackerInfo{KernelVersion: "6.8.0"},
			}, "disk.qcow2") // no kernel file
		writeImageDir(t, root, "windows", "qemu", "2026-05-20T000000Z-nodisk",
			Manifest{
				OS:           guestlettype.OSWindows,
				Hypervisor:   "qemu",
				Channel:      ChannelStable,
				DiskFile:     "disk.qcow2",
				SnapshotFile: "vm-state.bin",
			}, "vm-state.bin") // no disk file
		if _, err := ResolveVersion(root, guestlettype.OSLinux, VersionStable); err == nil {
			t.Error("want error when only incomplete Linux dirs present, got nil")
		}
		if _, err := ResolveVersion(root, guestlettype.OSWindows, VersionStable); err == nil {
			t.Error("want error when only incomplete Windows dirs present, got nil")
		}
	})

	t.Run("unknown-os", func(t *testing.T) {
		// darwin guests come from a tart pull rather than this bucket, so they
		// will never have an image layout created here and should always fail.
		_, err := ResolveVersion(t.TempDir(), guestlettype.OSDarwin, VersionStable)
		if err == nil {
			t.Fatal("want error for a guest OS with no image layout, got nil")
		}
		if errors.Is(err, ErrImageNotFound) {
			t.Errorf("invalid guest OS must not be reported as a missing image: %v", err)
		}
		if want := "no image layout"; !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to mention %q", err, want)
		}
	})
}

func TestResolveSnapshotCompressed(t *testing.T) {
	root := t.TempDir()
	dir := writeImageDir(t, root, "windows", "qemu", "2026-05-20T000000Z-bbbb",
		Manifest{
			OS:           guestlettype.OSWindows,
			Hypervisor:   "qemu",
			Channel:      ChannelStable,
			DiskFile:     "disk.qcow2",
			SnapshotFile: "vm-state.bin.zst",
		}, "disk.qcow2", "vm-state.bin.zst")
	img, err := ResolveVersion(root, guestlettype.OSWindows, VersionStable)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(dir, "vm-state.bin.zst"); img.Snapshot != want {
		t.Errorf("snapshot = %q, want %q (from manifest)", img.Snapshot, want)
	}
}

// TestManifestChannelRoundTrip checks that both channels use explicit JSON
// fields. A manifest without a channel must not resolve as stable.
func TestManifestChannelRoundTrip(t *testing.T) {
	stable, err := jsonv1.Marshal(Manifest{OS: guestlettype.OSLinux, Channel: ChannelStable})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(stable, []byte(`"channel":"stable"`)) {
		t.Errorf("stable manifest = %s, want an explicit stable channel field", stable)
	}

	unstable, err := jsonv1.Marshal(Manifest{OS: guestlettype.OSLinux, Channel: ChannelUnstable})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(unstable, []byte(`"channel":"unstable"`)) {
		t.Errorf("unstable manifest = %s, want a channel field", unstable)
	}

	// A manifest predating channels (added 2026-08-18) must not read as stable
	var m Manifest
	if err := jsonv1.Unmarshal([]byte(`{"os":"linux","disk_file":"disk.qcow2"}`), &m); err != nil {
		t.Fatal(err)
	}
	if m.Channel == ChannelStable || m.Channel == ChannelUnstable {
		t.Errorf("channel = %q for a manifest with no channel field, want neither known channel", m.Channel)
	}
}

func TestParseVersion(t *testing.T) {
	for _, tt := range []struct {
		in   string
		want bool
	}{
		{"stable", true},
		{"unstable", true},
		{"2026-06-04T091200Z-a1b2c3d4e5-unstable", true},
		{"2026-06-04T091200Z-a1b2c3d4e5", true},
		{"", false},
		{"latest", false},
		// Extra path info must not parse, or a caller escapes the image root.
		{"../../../etc/passwd", false},
		{"2026-06-04T091200Z-a1b2c3d4e5-unstable/../..", false},
		{"2026-06-04T091200Z-a1b2c3d4e5-unstable/kernel", false},
		{"/etc/passwd", false},
		{"2026-06-04T091200Z-a1b2c3d4e5-unstable\n", false},
		{".", false},
		{"..", false},
		// These names are malformed, but they carry no traversal.
		{"2026-06-04T091200Z-a1b2c3d4e-unstable", false},   // invalid 9-char hash
		{"2026-06-04T091200Z-a1b2c3d4e55-unstable", false}, // invalid 11-char hash
		{"2026-06-04T091200Z-A1B2C3D4E5-unstable", false},  // uppercase hash
		{"2026-06-04T091200Z-a1b2c3d4eg-unstable", false},  // non-hex hash
		{"2026-06-04T091200-a1b2c3d4e5-unstable", false},   // no Z
		{"2026-06-04X091200Z-a1b2c3d4e5-unstable", false},  // wrong T separator
		{"2026-06-04T09120Z-a1b2c3d4e5-unstable", false},   // short timestamp
		{"2026-06-04T091200Z-a1b2c3d4e5-dev", false},       // undefined suffix
		{"unstable-2026-06-04T091200Z-a1b2c3d4e5", false},
		// These names have the right shape, but no build produces them. date -u
		// never emits a 13th month, a 31st of June, or a 25th hour.
		{"2026-13-04T091200Z-a1b2c3d4e5-unstable", false},
		{"2026-06-31T091200Z-a1b2c3d4e5-unstable", false},
		{"2026-06-04T251200Z-a1b2c3d4e5-unstable", false},
		{"2026-06-04T096100Z-a1b2c3d4e5-unstable", false},
	} {
		got, ok := ParseVersion(tt.in)
		if ok != tt.want {
			t.Errorf("ParseVersion(%q) ok = %v, want %v", tt.in, ok, tt.want)
			continue
		}
		want := Version(tt.in)
		if !ok {
			want = "" // a rejected version must not reach a caller that ignores ok
		}
		if got != want {
			t.Errorf("ParseVersion(%q) = %q, want %q", tt.in, got, want)
		}
	}
}

func TestResolveVersion(t *testing.T) {
	linuxManifest := func(ch Channel) Manifest {
		return Manifest{
			OS: guestlettype.OSLinux, Hypervisor: "firecracker",
			DiskFile: "disk.qcow2", KernelFile: "kernel", Channel: ch,
			Firecracker: &FirecrackerInfo{KernelVersion: "6.8.0"},
		}
	}
	const (
		stableVer     = "2026-05-20T000000Z-bbbbbbbbbb"
		olderStable   = "2026-05-10T000000Z-dddddddddd"
		unstableVer   = "2026-05-29T041300Z-aaaaaaaaaa-unstable"
		olderUnstable = "2026-05-10T000000Z-cccccccccc-unstable"
	)
	newRoot := func(t *testing.T) string {
		root := t.TempDir()
		writeImageDir(t, root, "linux", "firecracker", stableVer, linuxManifest(ChannelStable), "disk.qcow2", "kernel")
		writeImageDir(t, root, "linux", "firecracker", olderStable, linuxManifest(ChannelStable), "disk.qcow2", "kernel")
		writeImageDir(t, root, "linux", "firecracker", unstableVer, linuxManifest(ChannelUnstable), "disk.qcow2", "kernel")
		writeImageDir(t, root, "linux", "firecracker", olderUnstable, linuxManifest(ChannelUnstable), "disk.qcow2", "kernel")
		return root
	}

	t.Run("unstable-sentinel-picks-newest-unstable", func(t *testing.T) {
		img, err := ResolveVersion(newRoot(t), guestlettype.OSLinux, VersionUnstable)
		if err != nil {
			t.Fatal(err)
		}
		if img.Manifest.Version != unstableVer {
			t.Errorf("Version = %q, want the newest unstable %q", img.Manifest.Version, unstableVer)
		}
	})

	t.Run("stable-sentinel-picks-newest-stable", func(t *testing.T) {
		img, err := ResolveVersion(newRoot(t), guestlettype.OSLinux, VersionStable)
		if err != nil {
			t.Fatal(err)
		}
		if img.Manifest.Version != stableVer {
			t.Errorf("Version = %q, want the stable image %q", img.Manifest.Version, stableVer)
		}
	})

	t.Run("exact-version", func(t *testing.T) {
		for _, want := range []Version{stableVer, olderStable, unstableVer, olderUnstable} {
			img, err := ResolveVersion(newRoot(t), guestlettype.OSLinux, want)
			if err != nil {
				t.Fatalf("ResolveVersion(%q): %v", want, err)
			}
			if Version(img.Manifest.Version) != want {
				t.Errorf("Version = %q, want %q", img.Manifest.Version, want)
			}
		}
	})

	t.Run("absent-version", func(t *testing.T) {
		_, err := ResolveVersion(newRoot(t), guestlettype.OSLinux, "2026-01-01T000000Z-dddddddddd-unstable")
		if err == nil {
			t.Error("want error for a version that is not present, got nil")
		}
	})

	t.Run("rejects-bad-version-before-touching-disk", func(t *testing.T) {
		// A nonexistent root proves no filesystem access happened. A valid
		// version would instead fail with a read error that names the path.
		_, err := ResolveVersion("/nonexistent-root", guestlettype.OSLinux, "../../etc/passwd")
		if err == nil {
			t.Fatal("want error for an invalid version, got nil")
		}
		if want := "invalid image version"; !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to mention %q", err, want)
		}
		if errors.Is(err, ErrImageNotFound) {
			t.Errorf("invalid version must not be reported as a missing image: %v", err)
		}
	})

	t.Run("no-unstable-present", func(t *testing.T) {
		root := t.TempDir()
		writeImageDir(t, root, "linux", "firecracker", stableVer, linuxManifest(ChannelStable), "disk.qcow2", "kernel")
		if _, err := ResolveVersion(root, guestlettype.OSLinux, VersionUnstable); err == nil {
			t.Error("want error when no unstable image exists, got nil")
		}
	})

	t.Run("mismatched-dir-not-bootable-by-name", func(t *testing.T) {
		// An explicit request for a malformed dir must not bypass the
		// cross-check that keeps Resolve from serving it.
		for _, tt := range []struct {
			version Version
			channel Channel
		}{
			{unstableVer, ChannelStable},
			{stableVer, ChannelUnstable},
		} {
			root := t.TempDir()
			writeImageDir(t, root, "linux", "firecracker", string(tt.version), linuxManifest(tt.channel), "disk.qcow2", "kernel")
			if _, err := ResolveVersion(root, guestlettype.OSLinux, tt.version); err == nil {
				t.Errorf("ResolveVersion(%q) accepted a dir whose name and manifest disagree", tt.version)
			}
		}
	})
}
