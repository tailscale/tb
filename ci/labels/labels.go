// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

// Package labels provides a comparable type for an unordered set of
// GitHub Actions runner labels.
package labels

import (
	jsonv1 "encoding/json"
	"fmt"
	"iter"
	"slices"
	"strings"
)

// Labels is a comparable set of GitHub Actions runner labels.
type Labels struct {
	// s is the labels sorted, deduplicated, and comma-joined. GitHub
	// Actions labels cannot contain commas, so the delimiter cannot
	// collide.
	s string
}

// Of returns [Labels] containing the given strings.
func Of(s ...string) Labels {
	return canonicalize(s)
}

// Parse parses a comma-separated label list. Empty input returns the
// zero [Labels].
func Parse(s string) Labels {
	if s == "" {
		return Labels{}
	}
	return canonicalize(strings.Split(s, ","))
}

// canonicalize sorts and deduplicates s and returns a [Labels] with the
// canonical comma-joined form. The input slice may be mutated.
func canonicalize(s []string) Labels {
	if len(s) == 0 {
		return Labels{}
	}
	slices.Sort(s)
	s = slices.Compact(s)
	return Labels{s: strings.Join(s, ",")}
}

// Len returns the number of labels.
func (lb Labels) Len() int {
	if lb.s == "" {
		return 0
	}
	return strings.Count(lb.s, ",") + 1
}

// Contains reports whether lb contains label.
func (lb Labels) Contains(label string) bool {
	for x := range lb.All() {
		if x == label {
			return true
		}
	}
	return false
}

// SupersetOf reports whether lb is a superset of other. If lb and other are
// equal, or if other is empty, this function returns true.
func (lb Labels) SupersetOf(other Labels) bool {
	for member := range other.All() {
		if !lb.Contains(member) {
			return false
		}
	}
	return true
}

// Slice returns the labels as a sorted slice.
func (lb Labels) Slice() []string {
	if lb.s == "" {
		return nil
	}
	return strings.Split(lb.s, ",")
}

// All iterates the labels in sorted order.
func (lb Labels) All() iter.Seq[string] {
	return func(yield func(string) bool) {
		if lb.s == "" {
			return
		}
		for label := range strings.SplitSeq(lb.s, ",") {
			if !yield(label) {
				return
			}
		}
	}
}

// String returns the labels as a comma-separated string in sorted order.
func (lb Labels) String() string { return lb.s }

// MarshalText implements [encoding.TextMarshaler]. It returns the
// labels as a comma-separated string. This makes [Labels] usable as
// the key type of a JSON-encoded map.
func (lb Labels) MarshalText() ([]byte, error) {
	return []byte(lb.s), nil
}

// UnmarshalText implements [encoding.TextUnmarshaler]. It parses the
// labels from a comma-separated string.
func (lb *Labels) UnmarshalText(b []byte) error {
	*lb = Parse(string(b))
	return nil
}

// MarshalJSON marshals lb as a sorted JSON array.
func (lb Labels) MarshalJSON() ([]byte, error) {
	return jsonv1.Marshal(lb.Slice())
}

// UnmarshalJSON unmarshals lb from a JSON array (the canonical form
// produced by [Labels.MarshalJSON]) or from a JSON string (the form
// used when [Labels] is a JSON map key, via [Labels.MarshalText]).
func (lb *Labels) UnmarshalJSON(b []byte) error {
	if len(b) == 0 {
		*lb = Labels{}
		return nil
	}
	switch b[0] {
	case '[':
		var s []string
		if err := jsonv1.Unmarshal(b, &s); err != nil {
			return err
		}
		*lb = canonicalize(s)
		return nil
	case '"':
		var s string
		if err := jsonv1.Unmarshal(b, &s); err != nil {
			return err
		}
		*lb = Parse(s)
		return nil
	case 'n':
		*lb = Labels{}
		return nil
	default:
		return fmt.Errorf("labels: cannot unmarshal %s into Labels", b)
	}
}
