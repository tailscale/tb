// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

// Package guestlettype contains types that describe ciguestlet components.
package guestlettype

import "github.com/tailscale/tb/ci/labels"

// LinuxVM is the type of a Linux VM, such as 'firecracker' or 'qemu'.
type LinuxVM string

const (
	Firecracker LinuxVM = "firecracker"
	QEMU        LinuxVM = "qemu"
)

// OS identifies the operating system of a guest VM. Values match the
// corresponding runtime.GOOS strings.
type OS string

const (
	OSLinux   OS = "linux"
	OSWindows OS = "windows"
	OSFreeBSD OS = "freebsd"
	OSDarwin  OS = "darwin"
	OSPlan9   OS = "plan9"
)

// OSFrom extracts the runner [guestlettype.OS] from a set of GitHub runner
// labels. Mostly they match up with the runtime.GOOS that [guestlettype.OS] uses,
// except for mac where GitHub uses "macOS" but runtime.GOOS is "darwin".
// See https://docs.github.com/en/actions/how-tos/manage-runners/self-hosted-runners/use-in-a-workflow#using-default-labels-to-route-jobs
func OSFrom(labels labels.Labels) OS {
	for label := range labels.All() {
		switch label {
		case "linux":
			return OSLinux
		case "windows":
			return OSWindows
		case "macOS":
			return OSDarwin
		case "freebsd":
			// freebsd is not a default GitHub label, but we can create custom labels.
			return OSFreeBSD
		}
	}

	// Default to Linux if no OS label specified.
	return OSLinux
}
