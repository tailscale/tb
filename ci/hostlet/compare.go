// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package hostlet

import (
	"cmp"

	"github.com/tailscale/tb/ci/ciid"
)

// CompareNames compares two guestlet names. If both parse successfully, they
// are ordered by hostlet name, then slot number, then lexically by suffix.
// If either side fails to parse, the raw strings are compared lexically.
func CompareNames(a, b ciid.HostletName) int {
	am, an, aok := a.Parse()
	bm, bn, bok := b.Parse()
	if !aok || !bok {
		return cmp.Compare(a, b)
	}

	// Sort by metadata first, then by number.
	if c := cmp.Compare(am, bm); c != 0 {
		return c
	}
	return cmp.Compare(an, bn)
}
