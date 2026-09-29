// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

// Package cimgrapi provides shared types for cimgr's HTTP API.
package cimgrapi

import (
	"github.com/tailscale/tb/ci/bootlog"
	"github.com/tailscale/tb/ci/ciid"
	"github.com/tailscale/tb/ci/guestlet"
)

// CreateVMResponse is the response body for POST /api/vms on cimgr.
// The request body for POST /api/vms is [guestlet.Opts].
//
// All fields are populated on a successful (201 Created) response.
type CreateVMResponse struct {
	// VM is the created VM, as reported by the owning cihostlet.
	VM *guestlet.Guestlet `json:"vm"`

	// Hostlet is the cihostlet on which the VM was scheduled.
	Hostlet ciid.HostletName `json:"hostlet"`

	// BootLogURL is a URL path on cimgr for streaming the VM's boot log.
	// Clients append the ?stream=true query parameter to receive
	// newline-delimited [bootlog.Event] JSON objects until either a
	// ready or error event terminates the stream.
	BootLogURL string `json:"boot_log_url"`

	// SSH describes how to reach the VM through the owning cihostlet's
	// SSH proxy: where to dial and how to authenticate, including the
	// per-VM bearer token that authorizes use of the proxy.
	SSH *bootlog.SSHInfo `json:"ssh"`
}
