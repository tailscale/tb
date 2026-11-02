// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package awscache

// This file has the storage layer: the DynamoDB metadata store and the S3
// blob store, behind small interfaces so tests can use in-memory fakes.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

// actionEntry is the metadata stored for one cached action in one
// namespace: one DynamoDB item.
type actionEntry struct {
	OutputID   string // Go output ID (hex) the client gave on PUT
	Size       int64  // uncompressed object size in bytes
	CreateTime int64  // unix seconds
	AccessTime int64  // unix seconds; bumped relatime-style by [atimeBumper]
	TTL        int64  // unix seconds; DynamoDB TTL deletes the item after this

	// Inline is whether the object's bytes are stored in Data, in the
	// metadata item itself, rather than in the blob store.
	Inline bool
	Data   []byte // object bytes if Inline (nil for an empty object)

	// SHA256 and StoredSize describe the object in the blob store, for
	// non-inline objects. SHA256 is the lowercase hex SHA-256 of the
	// uncompressed content, computed by the writer (never trusted from the
	// client), and StoredSize is the size of the lz4-compressed blob.
	SHA256     string
	StoredSize int64
}

// metaStore is the durable metadata store shared by every client. In
// production it's DynamoDB ([dynamoStore]).
//
// Keys are opaque strings built by [actionKey].
type metaStore interface {
	// GetAction returns the action stored at key, or (nil, nil) if there
	// is none.
	GetAction(ctx context.Context, key string) (*actionEntry, error)

	// ActionExists reports whether an action is stored at key. It's
	// cheaper than GetAction, as it doesn't fetch inline data.
	ActionExists(ctx context.Context, key string) (bool, error)

	// PutActionIfAbsent stores e at key unless an action already exists
	// there, reporting whether it stored it. The first PUT wins.
	PutActionIfAbsent(ctx context.Context, key string, e *actionEntry) (created bool, err error)

	// BumpAccessTime sets the access time of the action at key to now and
	// its TTL to ttl if it exists and its access time is before olderThan.
	// It's not an error if the condition doesn't hold.
	BumpAccessTime(ctx context.Context, key string, now, ttl, olderThan int64) error
}

// errBlobNotFound is returned by [blobStore.Get] for a missing blob.
var errBlobNotFound = errors.New("blob not found")

// blobStore is the durable content-addressed object store shared by every
// client. In production it's S3 ([s3Store]).
type blobStore interface {
	// Exists reports whether a blob is stored at key.
	Exists(ctx context.Context, key string) (bool, error)

	// Put stores size bytes from r at key. Because keys are content
	// addresses, it's not an error if the blob already exists; Put leaves
	// the existing blob in place.
	Put(ctx context.Context, key string, r io.ReadSeeker, size int64) error

	// Get opens the blob at key and returns its size, or returns
	// [errBlobNotFound] if it doesn't exist.
	Get(ctx context.Context, key string) (_ io.ReadCloser, size int64, _ error)
}

// Item key prefixes in the single DynamoDB table. The separator '#' and the
// '!' that starts global namespace names are outside the namespace character
// set (see [ValidateNamespace]), so keys can't collide.
//
// TODO(bradfitz): "m#cache#<az>" items listing the members of the optional
// read-through NVMe blob cache pool, per ci/doc/aws-native-gocached.md.
const actionKeyPrefix = "a#"

// storageNS is a namespace name as used in storage keys: either a validated
// [Namespace] or the synthetic name of a global generation.
type storageNS string

// globalStorageNS returns the storage name of the given generation of the
// global namespace. Generations 0 and 1 are the same.
func globalStorageNS(gen int) storageNS {
	return storageNS("!global:" + strconv.Itoa(max(gen, 1)))
}

// actionKey returns the metaStore key for actionID in namespace ns.
func actionKey(ns storageNS, actionID string) string {
	return actionKeyPrefix + string(ns) + "#" + actionID
}

// blobKey returns the blobStore key for the lz4-compressed blob with the
// given SHA-256 hex. The leading two hex digits spread keys across S3's
// prefix partitions and let the scrubber list in parallel.
func blobKey(prefix, sha256hex string) string {
	if len(sha256hex) != 64 {
		panic(fmt.Sprintf("bad sha256 hex %q", sha256hex))
	}
	return prefix + sha256hex[:2] + "/" + sha256hex + ".lz4"
}

// DynamoDB attribute names. Short, since they're stored in every item.
const (
	attrKey        = "k"   // partition key (S)
	attrTTL        = "ttl" // DynamoDB TTL, unix seconds (N)
	attrOutputID   = "o"   // output ID hex (S)
	attrSize       = "n"   // uncompressed size (N)
	attrCreateTime = "ct"  // unix seconds (N)
	attrAccessTime = "at"  // unix seconds (N)
	attrInline     = "i"   // whether attrData holds the object (BOOL)
	attrData       = "d"   // inline object bytes (B); absent if empty
	attrSHA256     = "h"   // blob SHA-256 hex (S)
	attrStored     = "z"   // blob stored (lz4) size (N)
)

// dynamoStore is a [metaStore] backed by a single DynamoDB table whose
// partition key is the string attribute [attrKey], with no sort key, and
// with TTL enabled on [attrTTL].
type dynamoStore struct {
	c     *dynamodb.Client
	table string
	m     *metrics

	// consistentReads makes action lookups strongly consistent. Eventually
	// consistent reads cost half as much and are almost always fresh within
	// a second, which is plenty for a cache.
	consistentReads bool
}

var _ metaStore = (*dynamoStore)(nil)

func avS(s string) ddbtypes.AttributeValue { return &ddbtypes.AttributeValueMemberS{Value: s} }
func avN(n int64) ddbtypes.AttributeValue {
	return &ddbtypes.AttributeValueMemberN{Value: strconv.FormatInt(n, 10)}
}
func avBool(b bool) ddbtypes.AttributeValue { return &ddbtypes.AttributeValueMemberBOOL{Value: b} }

func keyOf(key string) map[string]ddbtypes.AttributeValue {
	return map[string]ddbtypes.AttributeValue{attrKey: avS(key)}
}

// item is a decoded DynamoDB item with lenient typed getters: a missing or
// mistyped attribute reads as the zero value.
type item map[string]ddbtypes.AttributeValue

func (it item) s(name string) string {
	v, _ := it[name].(*ddbtypes.AttributeValueMemberS)
	if v == nil {
		return ""
	}
	return v.Value
}

func (it item) n(name string) int64 {
	v, _ := it[name].(*ddbtypes.AttributeValueMemberN)
	if v == nil {
		return 0
	}
	n, _ := strconv.ParseInt(v.Value, 10, 64)
	return n
}

func (it item) b(name string) []byte {
	v, _ := it[name].(*ddbtypes.AttributeValueMemberB)
	if v == nil {
		return nil
	}
	return v.Value
}

func (it item) bool(name string) bool {
	v, _ := it[name].(*ddbtypes.AttributeValueMemberBOOL)
	return v != nil && v.Value
}

func isConditionFailed(err error) bool {
	var ccf *ddbtypes.ConditionalCheckFailedException
	return errors.As(err, &ccf)
}

// cmpResult returns "error" if err is non-nil, else result.
func cmpResult(result string, err error) string {
	if err != nil {
		return "error"
	}
	return result
}

// checkTable verifies the table exists and has the right key schema.
func (d *dynamoStore) checkTable(ctx context.Context) error {
	out, err := d.c.DescribeTable(ctx, &dynamodb.DescribeTableInput{TableName: &d.table})
	if err != nil {
		return err
	}
	if st := out.Table.TableStatus; st != ddbtypes.TableStatusActive && st != ddbtypes.TableStatusUpdating {
		return fmt.Errorf("table %q status is %v", d.table, st)
	}
	for _, ks := range out.Table.KeySchema {
		if aws.ToString(ks.AttributeName) == attrKey && ks.KeyType == ddbtypes.KeyTypeHash {
			return nil
		}
	}
	return fmt.Errorf("table %q must have string partition key %q", d.table, attrKey)
}

func (d *dynamoStore) GetAction(ctx context.Context, key string) (_ *actionEntry, retErr error) {
	start := time.Now()
	result := "ok"
	defer func() { d.m.observeOp("dynamodb", "GetAction", cmpResult(result, retErr), start) }()
	out, err := d.c.GetItem(ctx, &dynamodb.GetItemInput{
		TableName:      &d.table,
		Key:            keyOf(key),
		ConsistentRead: aws.Bool(d.consistentReads),
	})
	if err != nil {
		return nil, err
	}
	if out.Item == nil {
		result = "miss"
		return nil, nil
	}
	it := item(out.Item)
	return &actionEntry{
		OutputID:   it.s(attrOutputID),
		Size:       it.n(attrSize),
		CreateTime: it.n(attrCreateTime),
		AccessTime: it.n(attrAccessTime),
		TTL:        it.n(attrTTL),
		Inline:     it.bool(attrInline),
		Data:       it.b(attrData),
		SHA256:     it.s(attrSHA256),
		StoredSize: it.n(attrStored),
	}, nil
}

func (d *dynamoStore) ActionExists(ctx context.Context, key string) (_ bool, retErr error) {
	start := time.Now()
	result := "ok"
	defer func() { d.m.observeOp("dynamodb", "ActionExists", cmpResult(result, retErr), start) }()
	out, err := d.c.GetItem(ctx, &dynamodb.GetItemInput{
		TableName:                &d.table,
		Key:                      keyOf(key),
		ConsistentRead:           aws.Bool(d.consistentReads),
		ProjectionExpression:     aws.String("#k"),
		ExpressionAttributeNames: map[string]string{"#k": attrKey},
	})
	if err != nil {
		return false, err
	}
	if out.Item == nil {
		result = "miss"
	}
	return out.Item != nil, nil
}

func (d *dynamoStore) PutActionIfAbsent(ctx context.Context, key string, e *actionEntry) (created bool, retErr error) {
	start := time.Now()
	result := "ok"
	defer func() { d.m.observeOp("dynamodb", "PutAction", cmpResult(result, retErr), start) }()
	it := map[string]ddbtypes.AttributeValue{
		attrKey:        avS(key),
		attrOutputID:   avS(e.OutputID),
		attrSize:       avN(e.Size),
		attrCreateTime: avN(e.CreateTime),
		attrAccessTime: avN(e.AccessTime),
		attrTTL:        avN(e.TTL),
		attrInline:     avBool(e.Inline),
	}
	if e.Inline {
		if len(e.Data) > 0 {
			it[attrData] = &ddbtypes.AttributeValueMemberB{Value: e.Data}
		}
	} else {
		it[attrSHA256] = avS(e.SHA256)
		it[attrStored] = avN(e.StoredSize)
	}
	_, err := d.c.PutItem(ctx, &dynamodb.PutItemInput{
		TableName:                &d.table,
		Item:                     it,
		ConditionExpression:      aws.String("attribute_not_exists(#k)"),
		ExpressionAttributeNames: map[string]string{"#k": attrKey},
	})
	if isConditionFailed(err) {
		result = "cond"
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

func (d *dynamoStore) BumpAccessTime(ctx context.Context, key string, now, ttl, olderThan int64) (retErr error) {
	start := time.Now()
	result := "ok"
	defer func() { d.m.observeOp("dynamodb", "BumpAccessTime", cmpResult(result, retErr), start) }()
	_, err := d.c.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: &d.table,
		Key:       keyOf(key),
		// The condition keeps this from creating an item for a key that
		// doesn't exist (or that TTL just deleted), and makes bumps racing
		// across hosts cheap no-ops after the first.
		UpdateExpression:         aws.String("SET #at = :now, #ttl = :ttl"),
		ConditionExpression:      aws.String("attribute_exists(#k) AND #at < :old"),
		ExpressionAttributeNames: map[string]string{"#k": attrKey, "#at": attrAccessTime, "#ttl": attrTTL},
		ExpressionAttributeValues: map[string]ddbtypes.AttributeValue{
			":now": avN(now),
			":ttl": avN(ttl),
			":old": avN(olderThan),
		},
	})
	if isConditionFailed(err) {
		result = "cond"
		return nil
	}
	return err
}

// s3Store is a [blobStore] backed by an S3 bucket.
type s3Store struct {
	c      *s3.Client
	bucket string
	m      *metrics
}

var _ blobStore = (*s3Store)(nil)

func isHTTPStatus(err error, code int) bool {
	var re *awshttp.ResponseError
	return errors.As(err, &re) && re.HTTPStatusCode() == code
}

func isAPIErrorCode(err error, codes ...string) bool {
	var ae smithy.APIError
	if !errors.As(err, &ae) {
		return false
	}
	for _, c := range codes {
		if ae.ErrorCode() == c {
			return true
		}
	}
	return false
}

// checkBucket verifies the bucket is reachable with our credentials.
func (s *s3Store) checkBucket(ctx context.Context) error {
	_, err := s.c.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: &s.bucket})
	return err
}

func (s *s3Store) Exists(ctx context.Context, key string) (_ bool, retErr error) {
	start := time.Now()
	result := "ok"
	defer func() { s.m.observeOp("s3", "HeadObject", cmpResult(result, retErr), start) }()
	_, err := s.c.HeadObject(ctx, &s3.HeadObjectInput{Bucket: &s.bucket, Key: &key})
	if err != nil {
		var nf *s3types.NotFound
		if errors.As(err, &nf) || isHTTPStatus(err, http.StatusNotFound) {
			result = "miss"
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func (s *s3Store) Put(ctx context.Context, key string, r io.ReadSeeker, size int64) (retErr error) {
	start := time.Now()
	result := "ok"
	defer func() { s.m.observeOp("s3", "PutObject", cmpResult(result, retErr), start) }()
	_, err := s.c.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        &s.bucket,
		Key:           &key,
		Body:          r,
		ContentLength: aws.Int64(size),
		ContentType:   aws.String("application/x-lz4"),
		// Content-addressed: never overwrite. If another client wrote the
		// same blob first (412) or is writing it concurrently (409), the
		// bytes are identical, so that's success. The bucket policy should
		// also require this (the s3:if-none-match condition key).
		IfNoneMatch: aws.String("*"),
	})
	if err != nil && (isAPIErrorCode(err, "PreconditionFailed", "ConditionalRequestConflict") ||
		isHTTPStatus(err, http.StatusPreconditionFailed) || isHTTPStatus(err, http.StatusConflict)) {
		result = "cond"
		return nil
	}
	return err
}

func (s *s3Store) Get(ctx context.Context, key string) (_ io.ReadCloser, size int64, retErr error) {
	start := time.Now()
	result := "ok"
	// This measures time to first byte; the body streams afterwards.
	defer func() { s.m.observeOp("s3", "GetObject", cmpResult(result, retErr), start) }()
	out, err := s.c.GetObject(ctx, &s3.GetObjectInput{Bucket: &s.bucket, Key: &key})
	if err != nil {
		var nsk *s3types.NoSuchKey
		if errors.As(err, &nsk) || isHTTPStatus(err, http.StatusNotFound) {
			result = "miss"
			return nil, 0, errBlobNotFound
		}
		return nil, 0, err
	}
	return out.Body, aws.ToInt64(out.ContentLength), nil
}
