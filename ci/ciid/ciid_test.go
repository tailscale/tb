// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package ciid

import "testing"

func TestGuestletNameParse(t *testing.T) {
	tests := []struct {
		name       GuestletName
		wantHost   HostletName
		wantSlot   int
		wantSuffix string
		wantOK     bool
	}{
		// Standard Linux names.
		{"ci-linux-1-8-1774827890", "ci-linux-1", 8, "1774827890", true},
		{"ci-linux-2-2-1769779953", "ci-linux-2", 2, "1769779953", true},
		{"ci-linux-10-1-99", "ci-linux-10", 1, "99", true},

		// Mac names with extra hyphens in hostlet name.
		{"ci-mac-ec2-m2-1-2-30092025", "ci-mac-ec2-m2-1", 2, "30092025", true},
		{"ci-mac-phys-m4-2-1-30092025", "ci-mac-phys-m4-2", 1, "30092025", true},

		// Non-numeric suffix is fine (it's opaque).
		{"host-1-abc", "host", 1, "abc", true},

		// Slot 0 is valid syntactically.
		{"h-0-suffix", "h", 0, "suffix", true},

		// Too few components.
		{"nohyphens", "", 0, "", false},
		{"one-field", "", 0, "", false},

		// Slot is not a number.
		{"a-notanum-suffix", "", 0, "", false},

		// Empty string.
		{"", "", 0, "", false},

		// Empty hostlet name (hyphen at start).
		{"-1-suffix", "", 0, "", false},
	}
	for _, tt := range tests {
		host, slot, suffix, ok := tt.name.Parse()
		if ok != tt.wantOK || host != tt.wantHost || slot != tt.wantSlot || suffix != tt.wantSuffix {
			t.Errorf("GuestletName(%q).Parse() = (%q, %d, %q, %v), want (%q, %d, %q, %v)",
				tt.name, host, slot, suffix, ok,
				tt.wantHost, tt.wantSlot, tt.wantSuffix, tt.wantOK)
		}
	}
}

func TestGuestletNameRunner(t *testing.T) {
	n := GuestletName("ci-linux-1-2-1234")
	r := n.Runner()
	if r != "ci-linux-1-2-1234" {
		t.Errorf("Runner() = %q, want %q", r, "ci-linux-1-2-1234")
	}
}

func TestHostletNameParse(t *testing.T) {
	tests := []struct {
		name       HostletName
		wantMeta   string
		wantNumber int
		wantOK     bool
	}{
		// Standard names.
		{"ci-linux-1", "linux", 1, true},
		{"ci-mac-ec2-m2-1", "mac-ec2-m2", 1, true},
		{"ci-linux-5", "linux", 5, true},

		// Too few components.
		{"", "", 0, false},
		{"ci", "", 0, false},
		{"ci-", "", 0, false},
		{"ci-linux", "", 0, false},
		{"ci-linux-", "", 0, false},
		{"-linux-", "", 0, false},
		{"-1", "", 0, false},

		// Number is not a number.
		{"ci-linux-notanum", "", 0, false},
	}
	for _, tt := range tests {
		meta, number, ok := tt.name.Parse()
		if ok != tt.wantOK || meta != tt.wantMeta || number != tt.wantNumber {
			t.Errorf("HostletName(%q).Parse() = (%q, %d, %v), want (%q, %d, %v)",
				tt.name, meta, number, ok,
				tt.wantMeta, tt.wantNumber, tt.wantOK)
		}
	}
}
