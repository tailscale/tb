// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

// Package cinet holds network addressing shared between cihostlet and
// ciguestlet.
package cinet

// TailnetV4Net and TailnetV6Net are the ranges Tailscale allocates node
// addresses from. Guest VMs are never given an address in either, and never
// have a legitimate reason to address one: everything the host offers a guest
// is served on the guest's bridge gateway IP.
const (
	TailnetV4Net = "100.64.0.0/10"
	TailnetV6Net = "fd7a:115c:a1e0::/48"
)
