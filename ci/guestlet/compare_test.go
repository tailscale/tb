// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package guestlet

import (
	"testing"

	"github.com/tailscale/tb/ci/ciid"
)

func TestCompareNames(t *testing.T) {
	tests := []struct {
		a, b ciid.GuestletName
		want int // -1, 0, or 1
	}{
		// Same hostlet and slot, differ by suffix (lexical on suffix).
		{"ci-linux-2-1-100", "ci-linux-2-1-200", -1},
		{"ci-linux-2-1-200", "ci-linux-2-1-100", 1},
		{"ci-linux-2-1-100", "ci-linux-2-1-100", 0},

		// Same hostlet, differ by slot number (numeric).
		{"ci-linux-2-1-100", "ci-linux-2-2-100", -1},
		{"ci-linux-2-9-100", "ci-linux-2-10-100", -1},
		{"ci-linux-2-10-100", "ci-linux-2-9-100", 1},

		// Different hostlet names.
		{"ci-linux-1-1-100", "ci-linux-2-1-100", -1},
		{"ci-linux-2-1-100", "ci-linux-1-1-100", 1},

		// Mac names with extra hyphens in hostlet name.
		{"ci-mac-ec2-m2-1-1-100", "ci-mac-ec2-m2-1-2-100", -1},
		{"ci-mac-ec2-m2-1-2-100", "ci-mac-ec2-m2-1-1-100", 1},
		{"ci-mac-ec2-m2-1-1-100", "ci-mac-ec2-m2-1-1-200", -1},

		// Lexical suffix comparison (not numeric).
		{"ci-linux-1-1-aaa", "ci-linux-1-1-bbb", -1},
		{"ci-linux-1-1-9", "ci-linux-1-1-10", 1}, // lexical: "9" > "10"

		// Either side fails to parse — fall back to raw lexical.
		{"alpha", "beta", -1},
		{"beta", "alpha", 1},
		{"same", "same", 0},
		{"ci-linux-2-9", "ci-linux-2-foo", -1}, // both fail to parse
		{"unparseable", "ci-linux-1-1-100", 1}, // lexical: "u" > "c"
	}
	for _, tt := range tests {
		got := CompareNames(tt.a, tt.b)
		if got != tt.want {
			t.Errorf("CompareNames(%q, %q) = %d, want %d", tt.a, tt.b, got, tt.want)
		}
	}
}
