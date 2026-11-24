// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

//go:build unix

package main

import (
	"net/netip"
	"testing"
)

func TestPolicy(t *testing.T) {
	// The default is the host's private interface prefixes; these are a
	// colo Mac's LAN and Tart VM bridge.
	local := func() ([]netip.Prefix, error) {
		return []netip.Prefix{netip.MustParsePrefix("10.206.0.0/16"), netip.MustParsePrefix("192.168.64.0/24")}, nil
	}
	byDefault, err := parsePolicy("31369", "")
	if err != nil {
		t.Fatal(err)
	}
	byDefault.interfacePrefixes = local
	explicit, err := parsePolicy("31369", "172.16.0.0/12")
	if err != nil {
		t.Fatal(err)
	}
	explicit.interfacePrefixes = local
	for _, tt := range []struct {
		addr                      string
		wantDefault, wantExplicit bool
	}{
		{"10.206.101.224:31369", true, false},
		{"192.168.64.2:31369", true, false},
		{"172.16.0.1:31369", false, true},
		{"10.0.0.1:31369", false, false},    // RFC 1918, but not a local LAN
		{"192.168.1.2:31369", false, false}, // likewise
		{"10.206.101.224:22", false, false}, // port not allowed
		{"100.80.116.78:31369", false, false},
		{"1.1.1.1:31369", false, false},
		{"127.0.0.1:31369", false, false},
		{"[fd00::1]:31369", false, false},
	} {
		ap := netip.MustParseAddrPort(tt.addr)
		if got := byDefault.allow(ap); got != tt.wantDefault {
			t.Errorf("default: allow(%s) = %v, want %v", tt.addr, got, tt.wantDefault)
		}
		if got := explicit.allow(ap); got != tt.wantExplicit {
			t.Errorf("explicit: allow(%s) = %v, want %v", tt.addr, got, tt.wantExplicit)
		}
	}
}

func TestParsePolicyErrors(t *testing.T) {
	for _, tt := range []struct{ ports, prefixes string }{
		{"", ""},
		{"x", ""},
		{"70000", ""},
		{"31369", "10.0.0.0"},
		{"31369", "fd00::/8"},
	} {
		if _, err := parsePolicy(tt.ports, tt.prefixes); err == nil {
			t.Errorf("parsePolicy(%q, %q) succeeded", tt.ports, tt.prefixes)
		}
	}
}

func TestLocalPrivatePrefixes(t *testing.T) {
	prefixes, err := localPrivatePrefixes()
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("local private prefixes: %v", prefixes)
	for _, p := range prefixes {
		if !p.Addr().Is4() || !p.Addr().IsPrivate() || p != p.Masked() {
			t.Errorf("unexpected prefix %v", p)
		}
	}
}
