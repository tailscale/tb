// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

// Package bootlog defines structured boot event types shared between
// cihostlet (collector) and API consumers.
package bootlog

import (
	"github.com/tailscale/tb/ci/ciid"
	"github.com/tailscale/tb/ci/statustype"
)

// Event is a single boot log entry for a VM. Each event has a relative
// timestamp and exactly one of the optional fields set.
type Event struct {
	T     float64               `json:"t"`              // seconds since VM creation
	State statustype.GuestState `json:"state,omitzero"` // state transition (e.g. "starting-vm")
	Log   string                `json:"log,omitzero"`   // raw log line from ciguestlet
	Ready bool                  `json:"ready,omitzero"` // terminal: VM is SSH-ready
	Error string                `json:"error,omitzero"` // terminal: boot failed
	SSH   *SSHInfo              `json:"ssh,omitzero"`   // direct cihostlet SSH proxy details, set by cimgr on ready
}

// SSHInfo tells an API caller how to reconnect directly to cihostlet for SSH
// once a VM is ready. The bearer token itself is returned by VM creation and
// should be presented in Authorization: Bearer <token> when connecting to URL.
type SSHInfo struct {
	Hostlet     ciid.HostletName `json:"hostlet"`
	URL         string           `json:"url"`
	BearerToken string           `json:"bearer_token,omitzero"` // authorization secret for cihostlet's SSH proxy
}
