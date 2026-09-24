// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package rendezvous

import (
	"fmt"
	"testing"
)

func keys(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("key-%d", i)
	}
	return out
}

func TestTable(t *testing.T) {
	var tb Table
	if _, ok := tb.Pick("x"); ok {
		t.Fatal("empty table picked something")
	}

	tb.Set(map[string]float64{"a": 1, "b": 1, "c": 1})
	ks := keys(30000)
	counts := map[string]int{}
	owner := map[string]string{}
	for _, k := range ks {
		m, ok := tb.Pick(k)
		if !ok {
			t.Fatal("no pick")
		}
		if again, _ := tb.Pick(k); again != m {
			t.Fatalf("pick of %s not deterministic", k)
		}
		counts[m]++
		owner[k] = m
	}
	for m, n := range counts {
		// About a third each; wide margins so this never flakes while
		// still catching a broken hash.
		if n < 8500 || n > 11500 {
			t.Errorf("%s owns %d of %d, want about a third", m, n, len(ks))
		}
	}

	// Removing c moves only c's keys.
	tb.Set(map[string]float64{"a": 1, "b": 1})
	moved := 0
	for _, k := range ks {
		m, _ := tb.Pick(k)
		switch {
		case owner[k] == "c":
			if m == "c" {
				t.Fatal("removed member still picked")
			}
			moved++
		case m != owner[k]:
			t.Fatalf("key %s moved from surviving member %s to %s", k, owner[k], m)
		}
	}
	if moved != counts["c"] {
		t.Errorf("moved %d keys, want %d", moved, counts["c"])
	}
}

func TestWeights(t *testing.T) {
	var tb Table
	tb.Set(map[string]float64{"light": 1, "heavy": 3, "ignored": 0})
	counts := map[string]int{}
	for _, k := range keys(40000) {
		m, _ := tb.Pick(k)
		counts[m]++
	}
	if counts["ignored"] != 0 {
		t.Errorf("zero-weight member picked %d times", counts["ignored"])
	}
	ratio := float64(counts["heavy"]) / float64(counts["light"])
	if ratio < 2.7 || ratio > 3.3 {
		t.Errorf("heavy/light = %.2f (heavy %d, light %d), want about 3", ratio, counts["heavy"], counts["light"])
	}
}
