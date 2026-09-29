// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package hostlet

import (
	"testing"

	"github.com/tailscale/tb/ci/ciid"
)

func TestCompareNames(t *testing.T) {
	tests := []struct {
		a, b ciid.HostletName
		want int // -1, 0, or 1
	}{
		// Differ by number.
		{"ci-linux-100", "ci-linux-200", -1},
		{"ci-linux-200", "ci-linux-100", 1},
		{"ci-linux-100", "ci-linux-100", 0},
		{"ci-linux-1", "ci-linux-10", -1},
		{"ci-linux-10", "ci-linux-1", 1},

		// Differ by metadata.
		{"ci-linux-1", "ci-mac-ec2-m2-1", -1},
		{"ci-mac-ec2-m2-1", "ci-linux-1", 1},

		// Mac names with extra hyphens in hostlet name.
		{"ci-mac-ec2-m2-1", "ci-mac-ec2-m2-2", -1},
		{"ci-mac-ec2-m2-2", "ci-mac-ec2-m2-1", 1},
		{"ci-mac-ec2-m2-1", "ci-mac-ec2-m2-1", 0},

		// Either side fails to parse — fall back to raw lexical.
		{"alpha", "beta", -1},
		{"beta", "alpha", 1},
		{"same", "same", 0},
		{"ci-linux-foo", "ci-linux-bar", 1}, // both fail to parse
		{"unparseable", "ci-linux-1", 1},    // lexical: "u" > "c"
	}
	for _, tt := range tests {
		got := CompareNames(tt.a, tt.b)
		if got != tt.want {
			t.Errorf("CompareNames(%q, %q) = %d, want %d", tt.a, tt.b, got, tt.want)
		}
	}
}
