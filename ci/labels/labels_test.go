// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package labels

import (
	jsonv1 "encoding/json"
	"slices"
	"strings"
	"testing"
)

func TestComparable(t *testing.T) {
	// Same elements in any order produce equal Labels.
	if Of("linux", "x") != Of("x", "linux") {
		t.Error("Of with reordered elements must be ==")
	}
	// Duplicate elements are deduplicated.
	if Of("a", "a", "b") != Of("a", "b") {
		t.Error("Of with duplicates must dedup")
	}
	// Different elements are not equal.
	if Of("a", "b") == Of("a", "c") {
		t.Error("different Labels must not be ==")
	}
}

func TestLenSliceContains(t *testing.T) {
	var zero Labels
	if zero.Len() != 0 {
		t.Errorf("zero.Len = %d, want 0", zero.Len())
	}
	if zero.Slice() != nil {
		t.Errorf("zero.Slice = %v, want nil", zero.Slice())
	}

	lb := Of("b", "a", "c")
	if lb.Len() != 3 {
		t.Errorf("Len = %d, want 3", lb.Len())
	}
	if got := lb.Slice(); !slices.Equal(got, []string{"a", "b", "c"}) {
		t.Errorf("Slice = %v, want [a b c]", got)
	}
	if !lb.Contains("a") {
		t.Error("Contains(a) = false, want true")
	}
	if lb.Contains("missing") {
		t.Error("Contains(missing) = true, want false")
	}
}

func TestParse(t *testing.T) {
	if Parse("") != (Labels{}) {
		t.Error(`Parse("") must return zero Labels`)
	}
	if Parse("b,a,a") != Of("a", "b") {
		t.Errorf(`Parse("b,a,a") = %v, want Of("a","b")`, Parse("b,a,a"))
	}
}

func TestJSONRoundTrip(t *testing.T) {
	lb := Of("linux", "x")
	b, err := jsonv1.Marshal(lb)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if string(b) != `["linux","x"]` {
		t.Errorf("Marshal = %s, want [\"linux\",\"x\"]", b)
	}
	var got Labels
	if err := jsonv1.Unmarshal(b, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got != lb {
		t.Errorf("round-trip = %v, want %v", got, lb)
	}
}

func TestAsMapKey(t *testing.T) {
	m := map[Labels]int{}
	m[Of("a", "b")] = 1
	m[Of("c")] = 2
	if got := m[Of("b", "a")]; got != 1 {
		t.Errorf("lookup with reordered Of = %d, want 1", got)
	}

	// JSON-encoding a map with Labels keys should produce comma-joined
	// keys via MarshalText.
	b, err := jsonv1.Marshal(m)
	if err != nil {
		t.Fatalf("Marshal map: %v", err)
	}
	// Map iteration order isn't stable, so just check both keys are
	// present in the expected form.
	s := string(b)
	if !strings.Contains(s, `"a,b":1`) || !strings.Contains(s, `"c":2`) {
		t.Errorf("Marshal map = %s, missing expected keys", s)
	}

	var got map[Labels]int
	if err := jsonv1.Unmarshal(b, &got); err != nil {
		t.Fatalf("Unmarshal map: %v", err)
	}
	if got[Of("a", "b")] != 1 || got[Of("c")] != 2 {
		t.Errorf("Unmarshal map = %v, missing expected entries", got)
	}
}

func TestSupersetOf(t *testing.T) {
	for _, tc := range []struct {
		a, b Labels
		want bool
	}{
		{Labels{}, Labels{}, true},
		{Labels{}, Of("a"), false},
		{Of("a", "b"), Labels{}, true},
		{Of("a", "b"), Of("a"), true},
		{Of("a", "b"), Of("c"), false},
		{Of("a", "b"), Of("a", "b"), true},
		{Of("a", "b"), Of("a", "b", "c"), false},
	} {
		if got := tc.a.SupersetOf(tc.b); got != tc.want {
			t.Errorf("%v.SupersetOf(%v) = %t, want %t",
				tc.a, tc.b, got, tc.want)
		}
	}
}
