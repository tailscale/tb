// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

// Package ciid contains identifier types used in the CI system.
package ciid

import (
	"strconv"
	"strings"
)

// GuestletName is a globally unique guestlet name of the form "HOSTNAME-N-UNIXTIME",
// e.g. "ci-linux-1-4-1774827890" for ci-linux-1 host, slot 4, unix time 1774827890.
type GuestletName string

// Parse decomposes a GuestletName into its three components: the hostlet name
// (e.g. "ci-linux-1"), the 1-indexed slot number (e.g. 8), and the opaque
// suffix (e.g. "1774827890", typically a unix timestamp). It returns ok=false
// if the name doesn't match the expected format.
//
// It works backwards from the end using LastIndexByte so that hostlet names
// containing extra hyphens are handled correctly and no allocations are needed.
func (n GuestletName) Parse() (hostletName HostletName, slot int, suffix string, ok bool) {
	s := string(n)

	// Cut off the suffix (after the last hyphen).
	i := strings.LastIndexByte(s, '-')
	if i <= 0 {
		return
	}
	suffix = s[i+1:]
	s = s[:i]

	// Cut off the slot number (after the new last hyphen).
	i = strings.LastIndexByte(s, '-')
	if i <= 0 {
		return "", 0, "", false
	}
	slot, err := strconv.Atoi(s[i+1:])
	if err != nil {
		return "", 0, "", false
	}
	hostletName = HostletName(s[:i])
	ok = true
	return
}

// Runner returns the guestlet name as a GitHubRunnerName, since guestlets
// register as GitHub Actions runners using their guestlet name.
func (n GuestletName) Runner() GitHubRunnerName { return GitHubRunnerName(n) }

// GitHubRunnerName is the name of a GitHub Actions runner. For runners managed
// by cihostlet/ciguestlet, these are GuestletName values. But the GitHub org may
// also contain legacy or third-party runners with other naming conventions.
type GitHubRunnerName string

// Matches reports whether this GitHub runner name corresponds to the given hostlet.
func (n GitHubRunnerName) Matches(hostlet HostletName) bool {
	gh := string(n)
	h := string(hostlet)
	return strings.HasPrefix(gh, h+"-")
}

// HostletName is the globally unique name for a cihostlet of the form "ci-METADATA-N",
// Example hosts:
// * "ci-mac-ec2-m2-1" for the first M2 mac EC2 instance.
// * "ci-linux-5" for the fifth Linux hostlet running on an EC2 instance with nested virtualization enabled.
type HostletName string

// Parse decomposes a HostletName into its metadata (e.g. "linux", or
// "mac-ec2-m2") and number. For example, "ci-mac-ec2-m2-1" parses into
// metadata="mac-ec2-m2" and number=1.
//
// It works backwards from the end using LastIndexByte so that hostlet names
// containing extra hyphens are handled correctly and no allocations are needed.
func (n HostletName) Parse() (metadata string, number int, ok bool) {
	s := string(n)
	if !strings.HasPrefix(s, "ci-") {
		return "", 0, false
	}
	s = s[3:]

	// Cut off the number (after the last hyphen).
	i := strings.LastIndexByte(s, '-')
	if i <= 0 {
		return "", 0, false
	}
	number, err := strconv.Atoi(s[i+1:])
	if err != nil {
		return "", 0, false
	}
	return s[:i], number, true
}

// IsDynamic reports whether the given hostlet name should be considered a
// dynamic hostlet that can be scaled up and down by cimgr. At the time of
// writing (2026-05-21), only ci-linux-N hostlets are dynamic.
func (n HostletName) IsDynamic() bool {
	metadata, _, ok := n.Parse()
	return ok && metadata == "linux"
}
