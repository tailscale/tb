// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

// Package hostlet provides shared types for cihostlet and its clients.
package hostlet

import (
	"time"

	"github.com/tailscale/tb/ci/guestlettype"
)

// OS is the operating system cihostlet is running on. It uses the same string
// values as runtime.GOOS. [OSDarwin] hosts only support running
// guestlettype.OSDarwin guests. [OSLinux] hostlets support running
// guestlettype.OSLinux, guestlettype.OSWindows, and guestlettype.OSFreeBSD guests.
type OS string

const (
	OSLinux  OS = "linux"
	OSDarwin OS = "darwin"
)

// OSFor returns the [OS] that a hostlet should run on for scheduling the given
// guestlet OS.
func OSFor(os guestlettype.OS) OS {
	switch os {
	case guestlettype.OSDarwin:
		return OSDarwin
	default:
		// Everything other than mac runs on our linux cihostlets.
		return OSLinux
	}
}

// DrainRequest is the body of POST /api/drain. When Draining is true, the
// hostlet will reject any requests for new VMs. Unlike a SIGTERM-driven
// shutdown, it is reversible.
type DrainRequest struct {
	Draining bool `json:"draining"`
}

// DrainResponse is the body of GET /api/drain, which reports whether the
// hostlet is currently draining and/or shutting down. If ShuttingDown is true,
// Draining will always be true. ShuttingDown is an irreversible state triggered
// by a SIGTERM or deploy, while Draining can be toggled via the API.
type DrainResponse struct {
	Draining     bool `json:"draining"`
	ShuttingDown bool `json:"shuttingDown"`
}

// VMTTLRequest is the body of POST /api/vms/{name}/ttl, which makes sure
// the VM's TTL deadline is at least TTLSeconds from now. A deadline that is
// already later is left alone; the deadline never moves earlier.
type VMTTLRequest struct {
	TTLSeconds int `json:"ttl_seconds"`
}

// VMTTLResponse is the body of a successful POST /api/vms/{name}/ttl.
type VMTTLResponse struct {
	TTLDeadline time.Time `json:"ttl_deadline"` // when the hostlet will stop the VM if it is still idle
}
