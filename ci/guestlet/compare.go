// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

// Package guestlet provides shared utilities for CI guestlet tooling.
package guestlet

import (
	"cmp"

	"github.com/tailscale/tb/ci/ciid"
)

// CompareNames compares two guestlet names. If both parse successfully, they
// are ordered by hostlet name, then slot number, then lexically by suffix.
// If either side fails to parse, the raw strings are compared lexically.
func CompareNames(a, b ciid.GuestletName) int {
	ah, aslot, asuf, aok := a.Parse()
	bh, bslot, bsuf, bok := b.Parse()
	if !aok || !bok {
		return cmp.Compare(a, b)
	}
	if c := cmp.Compare(ah, bh); c != 0 {
		return c
	}
	if c := cmp.Compare(aslot, bslot); c != 0 {
		return c
	}
	return cmp.Compare(asuf, bsuf)
}
