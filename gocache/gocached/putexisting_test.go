// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package gocached

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"math"
	"net/http"
	"os"
	"strings"
	"testing"
)

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// putNoDrain PUTs val as actionID's output without draining the put queue,
// so the entry stays pending.
func (st *tester) putNoDrain(actionID, outputID, val string) {
	st.t.Helper()
	c := st.mkClient()
	if _, err := c.Put(context.Background(), actionID, outputID, int64(len(val)), strings.NewReader(val)); err != nil {
		st.t.Fatalf("Put: %v", err)
	}
	st.wantMetric(&st.srv.m.Puts, 1)
}

// countActions returns the number of Actions rows.
func (st *tester) countActions() int {
	st.t.Helper()
	var n int
	if err := st.srv.db.QueryRow("SELECT COUNT(*) FROM Actions").Scan(&n); err != nil {
		st.t.Fatal(err)
	}
	return n
}

// TestPutExistingFastPath checks that a PUT of a blob already in the hot
// tier is verified by hash and queued without any bytes of its own, is
// readable while pending, and commits like any other PUT.
func TestPutExistingFastPath(t *testing.T) {
	for _, tc := range []struct {
		name string
		val  string
	}{
		{"raw", strings.Repeat("r", smallObjectSize+1)},          // stored uncompressed
		{"lz4", strings.Repeat("compressible", smallObjectSize)}, // stored lz4
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := newServerTester(t, WithHotDir(t.TempDir()), WithHotCapacity(1<<20))
			q := st.srv.putq
			c := st.mkClient()
			oid := sha256Hex(tc.val)

			// The first PUT stores the blob and installs it in the hot tier.
			st.wantPut(c, "aa01", oid, tc.val)
			st.wantMetric(&st.srv.m.PutExisting, 0)
			if got := st.hotFiles(); len(got) != 1 {
				t.Fatalf("hot files = %v; want the one blob", got)
			}

			// A different action with the same output takes the fast path.
			st.putNoDrain("aa02", oid, tc.val)
			st.wantMetric(&st.srv.m.PutExisting, 1)
			p, ok := q.lookup(actionKey{NamespaceID: st.srv.globalNamespaceID, ActionID: "aa02"})
			if !ok {
				t.Fatal("second PUT not pending")
			}
			if !p.existing || p.queueFile != "" || p.smallData != nil || !p.reservation.inline {
				t.Errorf("pending entry = %+v; want existing, no bytes, inline-lane reservation", p)
			}
			if ents, err := os.ReadDir(q.dir); err != nil || len(ents) != 0 {
				t.Errorf("spool dir has %d entries, err=%v; want none", len(ents), err)
			}

			// It's readable while pending, from the stored blob.
			st.wantGet(c, "aa02", oid, tc.val)

			st.drain()
			if n := st.countActions(); n != 2 {
				t.Errorf("Actions rows = %d; want 2", n)
			}
			st.wantMetric(&st.srv.m.PutExistingGone, 0)
			st.wantGet(c, "aa02", oid, tc.val)
		})
	}
}

// TestPutExistingMismatch checks that a PUT whose outputID names a hot blob
// but whose body hashes differently is reported as successful but stores
// nothing.
func TestPutExistingMismatch(t *testing.T) {
	st := newServerTester(t, WithHotDir(t.TempDir()), WithHotCapacity(1<<20))
	c := st.mkClient()
	val := strings.Repeat("v", smallObjectSize+1)
	oid := sha256Hex(val)
	st.wantPut(c, "bb01", oid, val)

	// Send it as a raw request, not through a cachers.HTTPClient: on
	// Windows the client's local disk cache verifies that content hashes
	// to its outputID, so the client can't send a mismatched body.
	other := strings.Repeat("w", len(val)) // same size, different content
	req, err := http.NewRequest("PUT", st.hs.URL+"/bb02/"+oid, strings.NewReader(other))
	if err != nil {
		t.Fatal(err)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusNoContent {
		t.Fatalf("PUT status = %v; want 204", res.Status)
	}
	st.wantMetric(&st.srv.m.PutExistingMismatches, 1)
	st.wantMetric(&st.srv.m.Puts, 0)
	st.wantMetric(&st.srv.m.PutExisting, 0)
	if _, ok := st.srv.putq.lookup(actionKey{NamespaceID: st.srv.globalNamespaceID, ActionID: "bb02"}); ok {
		t.Error("mismatched PUT is pending")
	}
	st.drain()
	st.wantGetMiss(st.mkClient(), "bb02") // a fresh client, without c's local copy
	if n := st.countActions(); n != 1 {
		t.Errorf("Actions rows = %d; want 1", n)
	}
}

// TestPutExistingGone checks that a PUT of an already-stored blob is
// dropped, not committed, if the blob is no longer stored when its metadata
// would commit, and that the blob is then stored again by the next PUT.
func TestPutExistingGone(t *testing.T) {
	t.Run("evicted", func(t *testing.T) {
		st := newServerTester(t, WithHotDir(t.TempDir()), WithHotCapacity(1<<20))
		c := st.mkClient()
		val := strings.Repeat("e", smallObjectSize+1)
		oid := sha256Hex(val)
		st.wantPut(c, "cc01", oid, val)

		st.putNoDrain("cc02", oid, val)
		st.wantMetric(&st.srv.m.PutExisting, 1)

		// Evict the first action, and with it the blob, before the second
		// PUT's metadata commits.
		res, err := st.srv.evictOldestActions(context.Background(), math.MaxInt64, 10, math.MaxInt64)
		if err != nil {
			t.Fatal(err)
		}
		if res.Count != 1 {
			t.Fatalf("evicted %d actions; want 1", res.Count)
		}
		if got := st.hotFiles(); len(got) != 0 {
			t.Fatalf("hot files after eviction = %v; want none", got)
		}
		st.drain()
		st.wantMetric(&st.srv.m.PutExistingGone, 1)
		if n := st.countActions(); n != 0 {
			t.Errorf("Actions rows = %d; want 0", n)
		}
		st.wantGetMiss(c, "cc02")
	})

	t.Run("stale-hot-copy", func(t *testing.T) {
		// A crash in the middle of an eviction can leave a hot copy whose
		// Blobs row (and main copy) are gone.
		st := newServerTester(t, WithHotDir(t.TempDir()), WithHotCapacity(1<<20))
		c := st.mkClient()
		val := strings.Repeat("s", smallObjectSize+1)
		oid := sha256Hex(val)
		st.wantPut(c, "dd01", oid, val)
		for _, q := range []string{"DELETE FROM Actions", "DELETE FROM Blobs"} {
			if _, err := st.srv.db.Exec(q); err != nil {
				t.Fatal(err)
			}
		}

		// The next PUT takes the fast path and is dropped, which also
		// drops the stale hot copy...
		st.putNoDrain("dd02", oid, val)
		st.wantMetric(&st.srv.m.PutExisting, 1)
		st.drain()
		st.wantMetric(&st.srv.m.PutExistingGone, 1)
		if got := st.hotFiles(); len(got) != 0 {
			t.Errorf("hot files = %v; want the stale copy removed", got)
		}

		// ... so the one after that stores the blob again.
		st.wantPut(c, "dd03", oid, val)
		st.wantMetric(&st.srv.m.PutExisting, 0)
		st.wantGet(c, "dd03", oid, val)
	})
}
