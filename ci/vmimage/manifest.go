// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package vmimage

import "github.com/tailscale/tb/ci/guestlettype"

// Channel is a release channel for a guest VM image.
type Channel string

const (
	// ChannelStable is the channel serving production CI.
	ChannelStable Channel = "stable"
	// ChannelUnstable marks a candidate image published for pre-merge
	// validation. Version dirs holding one are suffixed
	// UnstableVersionSuffix, and are not served to production guests.
	ChannelUnstable Channel = "unstable"
)

// UnstableVersionSuffix is appended to the <timestamp>-<githash> version
// directory name of an unstable image, e.g.
// "2026-06-04T091200Z-a1b2c3d4e5-unstable".
//
// The marker is a suffix rather than a prefix so that all builds of a given
// day still sort together. Unstable images live under the same <os>/<vmm>/
// prefix as stable images and benefit from the same low-latency S3 Files tier
// (see deploy/terraform/ci-vm-images/s3files.tf).
const UnstableVersionSuffix = "-unstable"

// Manifest holds metadata about a VM image build. It is generally produced by
// buildkite and written to S3 in the same directory as the image artifacts.
// cihostlet reads the manifest to determine which QEMU version and CPU flags to
// use when booting the image.
type Manifest struct {
	Schema       int               `json:"schema"`
	OS           guestlettype.OS   `json:"os"`
	DiskFile     string            `json:"disk_file"`              // the disk image filename within the same directory as the manifest, e.g. "disk.qcow2"
	KernelFile   string            `json:"kernel_file,omitzero"`   // the kernel image filename within the same directory as the manifest, e.g. "kernel"; empty for non-Linux guests
	SnapshotFile string            `json:"snapshot_file,omitzero"` // the snapshot filename within the same directory as the manifest, e.g. "vm-state.bin"; empty for cold boot or Firecracker guests
	Hypervisor   string            `json:"hypervisor"`             // one of "qemu" or "firecracker"
	Channel      Channel           `json:"channel"`                // release channel; required, and one of ChannelStable or ChannelUnstable
	BuildTime    string            `json:"build_time"`
	GitHash      string            `json:"git_hash"`
	Version      string            `json:"version,omitzero"` // the version directory holding the image, e.g. "2026-06-03T041545Z-9c1b3571aa"; the directory name is authoritative, and Resolve fills this in from it
	BuildHost    BuildHost         `json:"build_host"`
	QEMU         *QEMUInfo         `json:"qemu,omitzero"`        // non-nil iff Hypervisor is "qemu"
	Firecracker  *FirecrackerInfo  `json:"firecracker,omitzero"` // non-nil iff Hypervisor is "firecracker"
	Buildkite    map[string]string `json:"buildkite,omitzero"`
}

// BuildHost holds the build host's metadata at image build time.
type BuildHost struct {
	Hostname string `json:"hostname"` // the build host's hostname
	GOOS     string `json:"goos"`     // the build host's GOOS, e.g. "linux"
	GOARCH   string `json:"goarch"`   // the build host's GOARCH, e.g. "amd64"
}

// QEMUInfo holds image metadata for QEMU guest images.
type QEMUInfo struct {
	Binary  string `json:"binary"`        // the QEMU binary used, e.g. "qemu-system-x86_64"
	Version string `json:"version"`       // the QEMU version used, e.g. "10.2.2"
	CPU     string `json:"cpu,omitempty"` // the QEMU -cpu flag used, e.g. "Skylake-Server-v3"
	Machine string `json:"machine"`       // the QEMU -machine flag used, e.g. "type=pc-i440fx-10.2,accel=kvm"
}

// FirecrackerInfo holds image metadata for Firecracker guest images.
type FirecrackerInfo struct {
	KernelVersion string `json:"kernel_version"` // the Linux kernel version used, e.g. "6.5.0"
}
