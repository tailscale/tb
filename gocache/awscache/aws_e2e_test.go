// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package awscache

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"io"
	"os"
	"testing"
	"time"
)

// TestRealAWS runs against a real DynamoDB table and S3 bucket, named by
// $AWSCACHE_TEST_TABLE and $AWSCACHE_TEST_BUCKET, with credentials and
// region from the environment as usual. It's skipped without them.
func TestRealAWS(t *testing.T) {
	table, bucket := os.Getenv("AWSCACHE_TEST_TABLE"), os.Getenv("AWSCACHE_TEST_BUCKET")
	if table == "" || bucket == "" {
		t.Skip("AWSCACHE_TEST_TABLE and AWSCACHE_TEST_BUCKET not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	c, err := New(ctx, Config{
		Region:     os.Getenv("AWS_REGION"),
		Table:      table,
		Bucket:     bucket,
		BlobPrefix: "awscache-test/",
		Logf:       t.Logf,
		Retention:  2 * 24 * time.Hour, // let test items expire soon
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close(ctx)
	if err := c.Check(ctx); err != nil {
		t.Fatal(err)
	}

	ns := Namespace("awscache-test")
	g := Grant{CanWrite: true, WriteNamespace: ns, ExtraReadNamespace: ns}
	for _, size := range []int{0, 10, 1 << 20} {
		id := make([]byte, 32)
		rand.Read(id)
		action := hex.EncodeToString(id)
		data := make([]byte, size)
		rand.Read(data)
		if err := c.Put(ctx, g, action, "abcd", int64(size), bytes.NewReader(data)); err != nil {
			t.Fatalf("Put(%d bytes): %v", size, err)
		}
		o, err := c.Get(ctx, g, action)
		if err != nil || o == nil {
			t.Fatalf("Get(%d bytes) = %v, %v", size, o, err)
		}
		r, err := o.Reader()
		if err != nil {
			t.Fatal(err)
		}
		got, err := io.ReadAll(r)
		o.Close()
		if err != nil || !bytes.Equal(got, data) || o.OutputID != "abcd" {
			t.Fatalf("Get(%d bytes): got %d bytes, output %q, err %v", size, len(got), o.OutputID, err)
		}
		if o, err := c.Get(ctx, Grant{}, action); err != nil || o != nil {
			t.Fatalf("Get from global = %v, %v; want miss", o, err)
		}
	}
}
