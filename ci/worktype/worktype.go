// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

// Package worktype contains types used for work requests ci-mgr -> hostlet and
// hostlet -> guestlet as well as for the hostlet announcing status to ci-mgr.
package worktype

import (
	"github.com/tailscale/tb/ci/ciid"
	"github.com/tailscale/tb/ci/guestlet"
	"github.com/tailscale/tb/ci/hostlet"
	"github.com/tailscale/tb/ci/labels"
)

// WorkRequest is the request from cimgr to a host to do some work.
type WorkRequest struct {
	// ID is an identifier of the request, used for logging.
	ID string `json:"id"`

	// TODO(irbekrm): add other types of work. Today (2025-09-17) the only known
	// work type is GitHub Actions runner (when a request has non-nil
	// GitHubActionsRunner field).

	// GitHubActionsRunner is set if the request is for running a GitHub Actions
	// runner.
	GithubActionsRunner *GithubActionsRunner `json:"githubActionsRunner,omitzero"`
}

// HostInfo is the information about host state that cihostlet shares with cimgr.
type HostInfo struct {
	// Name is a unique identifier of this host.
	Name ciid.HostletName `json:"name"`
	// OS is the operating system of this host.
	OS hostlet.OS `json:"os"`
	// RequestEndpoint is the URL this host's API is hosted at,
	// e.g. 'http://ci-linux-1.corp.ts.net:8692'.
	RequestEndpoint string `json:"requestEndpoint"`
	// DebugURL is the URL on which the host serves some debug info.
	DebugURL string `json:"debugURL"`
	// RunnerLabels are the runner labels with which this host's guests can run GitHub
	// Actions runners.
	RunnerLabels labels.Labels `json:"runnerLabels"`
	// MaxGuestlets is the maximum number of guestlets (baseline + on-demand) that
	// this host can run concurrently. It mirrors cihostlet's --max-guestlets flag.
	MaxGuestlets int `json:"maxGuestlets,omitzero"`
	// BaselineGuestlets is the number of pre-configured baseline guestlet slots on
	// this host. It mirrors cihostlet's --guestlets flag. The remaining
	// MaxGuestlets-BaselineGuestlets slots are available for on-demand VMs.
	BaselineGuestlets int `json:"baselineGuestlets,omitzero"`
	// Guestlets contains the statuses of the guestlets on this host mapped by
	// guestlet name, e.g {"ci-mac-ec2-m2-1-2":{},"ci-linux-1-3":{}}
	Guestlets map[ciid.GuestletName]*guestlet.Guestlet `json:"guestlets"`
	// Draining reports whether this host is intentionally not accepting new work.
	// The host may be shutting down, it may be waiting to restart so it picks up
	// a newly-deployed binary, or it may have been marked draining via the API.
	Draining bool `json:"draining"`
	// ShuttingDown reports whether this host is in the process of shutting down.
	// This is an irreversible state that is triggered by a SIGTERM or a deploy.
	// When ShuttingDown is true, Draining will always be true.
	ShuttingDown bool `json:"shuttingDown"`
}

// GitHubActionsRunner describes GitHub Actions runner on a guest VM.
type GithubActionsRunner struct {
	RunnerLabels labels.Labels `json:"runnerLabels"`
	// JobID is the value of github.WorkflowJob.ID. Currently used for logging only.
	JobID int64 `json:"jobID"`
}

// HostWSMessage is a message sent from cihostlet to cimgr over a websocket
// connection.
type HostWSMessage struct {
	// Type is the kind of message being sent.
	Type HostWSMessageType `json:"type"`
	// HostInfo contains the current state of the host.
	// It is non-nil for HostWSHello and HostWSGuestUpdate messages
	// and nil for HostWSPing messages.
	HostInfo *HostInfo `json:"hostInfo,omitzero"`
}

// HostWSMessageType is the type of a websocket message from cihostlet to cimgr.
type HostWSMessageType string

const (
	// HostWSHello is sent when a hostlet first connects to cimgr.
	HostWSHello HostWSMessageType = "hello"

	// HostWSPing is sent periodically (every 5s) as a keepalive.
	//
	// We send an explicit application-level ping every 5 seconds rather
	// than trusting the TCP layer keep-alives, which may not even be
	// enabled, and generally react slowly. We could use a
	// WebSocket-level ping but it's all basically the same bytes on the
	// wire and this way we can also include host info if we want to in
	// the future.
	HostWSPing HostWSMessageType = "ping"

	// HostWSGuestUpdate is sent when the state of a guest VM changes.
	HostWSGuestUpdate HostWSMessageType = "guest-update"
)
