// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package cinet

import (
	"net/netip"
	"testing"
)

// The constants are formatted into iptables and pf rules, where a typo would
// only surface as a rule-load failure at VM start.
func TestTailnetNetsParse(t *testing.T) {
	for _, s := range []string{TailnetV4Net, TailnetV6Net} {
		p, err := netip.ParsePrefix(s)
		if err != nil {
			t.Errorf("ParsePrefix(%q): %v", s, err)
			continue
		}
		if p.Masked() != p {
			t.Errorf("%q has bits set below the prefix length; want %s", s, p.Masked())
		}
	}
}
