// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

// Package vmimage resolves the on-disk paths of a CI guest-VM image from a
// base directory laid out like the ci-guest-vm-images S3 bucket:
//
//	<base>/<os>/<vmm>/<timestamp>-<githash>/
//	    disk.qcow2             # all guests
//	    kernel                 # linux/firecracker only
//	    vm-state.bin[.zst]     # QEMU guests with a fast-restore snapshot (optional)
//	    vm-state-overlay.qcow2 # derived by ciguestlet from the snapshot path
//	    manifest.json           # all guests, required for QEMU guests
//
// The same layout is used in prod (base = the S3-Files NFS mount) and in dev
// (base = the local image build output dir), so cihostlet and cidevtool share
// one resolver that differs only by the base directory.
package vmimage

import (
	jsonv1 "encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/tailscale/tb/ci/guestlettype"
)

// Image holds the resolved on-host paths for a guest OS's VM image: the
// manifest turned into concrete machine-local state, by combining it with the
// file system mount point of the bucket it was read from.
type Image struct {
	// Disk is the absolute path to a rootfs/disk image. It is always required.
	// For Linux/Firecracker it is the qcow2 served to the guest via guestbd
	// over NBD; for QEMU guests it is the qcow2 used as the COW backing file.
	// For example:
	//   * /mnt/ci-vm-images/linux/firecracker/2026-06-03T041545Z-9c1b3571aa/disk.qcow2
	//   * /mnt/ci-vm-images/freebsd/qemu/2026-06-03T143127Z-f0fbfd6845/disk.qcow2
	//   * /mnt/ci-vm-images/windows/qemu/2026-06-03T143140Z-f0fbfd6845/disk.qcow2
	Disk string
	// Kernel is the absolute path to a Linux kernel, which is required for Linux
	// firecracker guests. It is empty for QEMU guests. For example:
	//   * /mnt/ci-vm-images/linux/firecracker/2026-06-03T041545Z-9c1b3571aa/kernel
	Kernel string
	// Snapshot is the absolute path to a QEMU migration state file, passed as
	// --vm-state-file for fast snapshot restore, or "" to cold-boot. It is only
	// set for QEMU guests. For example:
	//   * /mnt/ci-vm-images/freebsd/qemu/2026-06-03T143127Z-f0fbfd6845/vm-state.bin
	//   * /mnt/ci-vm-images/windows/qemu/2026-06-03T143140Z-f0fbfd6845/vm-state.bin
	Snapshot string
	// Manifest holds the parsed manifest.json from the same directory as the
	// image artifacts. It is never nil.
	Manifest *Manifest
}

// ErrImageNotFound reports that no valid image matches the requested version.
var ErrImageNotFound = errors.New("image not found")

// ResolveVersion returns the image selected by version.
func ResolveVersion(root string, gos guestlettype.OS, version Version) (*Image, error) {
	img, err := resolve(root, gos, version)
	if err != nil {
		return nil, fmt.Errorf("no image found for %s version %q: %w", gos, version, err)
	}
	return img, nil
}

// Version selects which image to resolve. The VersionStable and VersionUnstable
// sentinels take the newest image on their channel. A version directory name
// pins one image exactly, whether it is stable or unstable.
type Version string

const (
	// VersionStable selects the newest image on ChannelStable, which is what
	// production guests boot.
	VersionStable Version = "stable"
	// VersionUnstable selects the newest candidate image. A caller can
	// therefore ask for one without knowledge of its timestamp.
	VersionUnstable Version = "unstable"
)

// channel reports which channel v resolves from.
func (v Version) channel() Channel {
	if v == VersionUnstable {
		return ChannelUnstable
	}
	return channelOfName(string(v))
}

// dirName returns the one version directory that v pins, or "" if v takes the
// newest image on its channel instead.
func (v Version) dirName() string {
	switch v {
	case VersionStable, VersionUnstable:
		return ""
	}
	return string(v)
}

// versionRe matches a version directory name and captures its timestamp. The
// name is a UTC ISO 8601 basic-format instant, a 10-character git hash, and an
// optional unstable suffix, e.g. "2026-06-04T091200Z-a1b2c3d4e5-unstable".
// The match prevents path traversal when a caller requests an image by version
// name.
var versionRe = regexp.MustCompile(`^(\d{4}-\d{2}-\d{2}T\d{6})Z-[0-9a-f]{10}` +
	`(?:` + regexp.QuoteMeta(UnstableVersionSuffix) + `)?$`)

// versionTimeLayout parses the timestamp that versionRe captures, in the form
// that the image Makefiles produce with date -u +%Y-%m-%dT%H%M%SZ.
const versionTimeLayout = "2006-01-02T150405"

// ParseVersion parses s as a VersionStable or VersionUnstable sentinel, or a
// stable or unstable version directory name, and reports whether it is valid.
func ParseVersion(s string) (Version, bool) {
	switch v := Version(s); v {
	case VersionStable, VersionUnstable:
		return v, true
	}
	m := versionRe.FindStringSubmatch(s)
	if m == nil {
		return "", false
	}
	if _, err := time.Parse(versionTimeLayout, m[1]); err != nil {
		return "", false
	}
	return Version(s), true
}

// channelOfName reports the channel that a version dir name uses.
func channelOfName(name string) Channel {
	if strings.HasSuffix(name, UnstableVersionSuffix) {
		return ChannelUnstable
	}
	return ChannelStable
}

// channelOf reports the release channel of a version dir, which is recorded
// redundantly in both the dir name and the manifest.
//
// The redundancy is deliberate. If the two disagree the dir is treated as
// malformed and reported as an unknown channel, which no filter matches, so it
// is skipped rather than resolved. That means promoting an unstable image to
// production takes two independent mistakes instead of one.
//
// A manifest with no channel at all is read as malformed and will not be used.
// A valid image is expected to always contain the Version and Channel in its
// manifest.
//
// Note that the reading applies to the manifest only. An unstable *dir* whose
// manifest omits the channel still disagrees with its name, and is skipped.
func channelOf(name string, m *Manifest) Channel {
	byName := channelOfName(name)
	if byName != m.Channel {
		// Neither ChannelStable nor ChannelUnstable, so nothing matches it.
		return Channel("mismatch")
	}
	return byName
}

// resolve returns the newest valid version dir under <root>/<os>/<vmm>, on the
// channel that version selects. If version names one dir, resolve considers
// only that dir.
func resolve(root string, gos guestlettype.OS, version Version) (*Image, error) {
	if _, ok := ParseVersion(string(version)); !ok {
		return nil, fmt.Errorf("invalid image version %q", version)
	}
	wantChannel, wantDir := version.channel(), version.dirName()

	var osDir, vmm string
	switch gos {
	case guestlettype.OSLinux:
		osDir, vmm = "linux", "firecracker"
	case guestlettype.OSFreeBSD:
		osDir, vmm = "freebsd", "qemu"
	case guestlettype.OSWindows:
		osDir, vmm = "windows", "qemu"
	case guestlettype.OSPlan9:
		osDir, vmm = "plan9", "qemu"
	default:
		return nil, fmt.Errorf("no image layout for guest OS %q", gos)
	}

	parent := filepath.Join(root, osDir, vmm)
	ents, err := os.ReadDir(parent)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("%w: reading %s: %v", ErrImageNotFound, parent, err)
	}
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", parent, err)
	}
	// Sort in reverse lexicographic order to get the most recent timestamps first.
	slices.SortFunc(ents, func(a, b os.DirEntry) int {
		return strings.Compare(b.Name(), a.Name())
	})

	for _, ent := range ents {
		if !ent.IsDir() {
			continue
		}
		if wantDir != "" && ent.Name() != wantDir {
			continue
		}

		manifestPath := filepath.Join(parent, ent.Name(), "manifest.json")
		if !fileExists(manifestPath) {
			continue
		}
		manifestBytes, err := os.ReadFile(manifestPath)
		if err != nil {
			continue
		}
		var m Manifest
		if err := jsonv1.Unmarshal(manifestBytes, &m); err != nil {
			continue
		}
		if m.DiskFile == "" {
			continue
		}
		// A manifest naming a version other than the directory holding it means
		// the directory was renamed or copied, so skip it as malformed. Images
		// published before the field existed have no version to disagree with.
		if m.Version != "" && m.Version != ent.Name() {
			continue
		}
		m.Version = ent.Name()
		if channelOf(ent.Name(), &m) != wantChannel {
			continue
		}

		dir := filepath.Join(parent, ent.Name())
		img := &Image{
			Disk:     filepath.Join(dir, m.DiskFile),
			Manifest: &m,
		}
		if !fileExists(img.Disk) {
			continue
		}

		if gos == guestlettype.OSLinux {
			img.Kernel = filepath.Join(dir, m.KernelFile)
			if m.KernelFile == "" || !fileExists(img.Kernel) {
				continue
			}
		}
		if m.SnapshotFile != "" {
			img.Snapshot = filepath.Join(dir, m.SnapshotFile)
		}

		return img, nil
	}

	return nil, fmt.Errorf("%w: no valid image for %s in %s", ErrImageNotFound, gos, parent)
}

// DevRoot returns the base directory for VM images in a dev environment. The
// directory is populated by cmd/ciguestlet/build Makefiles, and should mirror
// the layout of the S3 ci-guest-vm-images bucket used in prod.
func DevRoot() string {
	var base string
	if out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output(); err == nil {
		base = strings.TrimSpace(string(out))
	} else {
		wd, _ := os.Getwd()
		base = wd
	}
	return filepath.Join(base, "cmd", "ciguestlet", "build", "output")
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
