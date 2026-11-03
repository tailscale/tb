// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package ocidl

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/pierrec/lz4/v4"
	"github.com/tailscale/tb/mantsana/image"
)

// encodeAppleLZ4 frames data as Apple LZ4 blocks of blockSize, compressing
// each block independently (or storing it raw if it doesn't compress).
func encodeAppleLZ4(t testing.TB, data []byte, blockSize int) []byte {
	t.Helper()
	var out bytes.Buffer
	var ht [1 << 16]int
	for len(data) > 0 {
		blk := data[:min(blockSize, len(data))]
		data = data[len(blk):]
		dst := make([]byte, lz4.CompressBlockBound(len(blk)))
		n, err := lz4.CompressBlock(blk, dst, ht[:])
		if err != nil {
			t.Fatal(err)
		}
		if n == 0 || n >= len(blk) {
			out.WriteString(magicRawBlock)
			binary.Write(&out, binary.LittleEndian, uint32(len(blk)))
			out.Write(blk)
			continue
		}
		out.WriteString(magicLZ4Block)
		binary.Write(&out, binary.LittleEndian, uint32(len(blk)))
		binary.Write(&out, binary.LittleEndian, uint32(n))
		out.Write(dst[:n])
	}
	out.WriteString(magicEnd)
	return out.Bytes()
}

func decodeAll(t testing.TB, stream []byte) ([]byte, error) {
	t.Helper()
	var out bytes.Buffer
	err := decodeAppleLZ4(bytes.NewReader(stream), func(b []byte) error {
		out.Write(b)
		return nil
	})
	return out.Bytes(), err
}

func TestDecodeAppleLZ4(t *testing.T) {
	data := make([]byte, 300<<10)
	for i := range data {
		data[i] = byte(i / 7) // compressible
	}
	copy(data[100<<10:], bytes.Repeat([]byte{0xff, 0x13, 0x77}, 1000))
	rnd := make([]byte, 5000) // incompressible, to get a raw block
	for i := range rnd {
		rnd[i] = byte(i*7919 + i*i*31)
	}
	data = append(data, rnd...)
	got, err := decodeAll(t, encodeAppleLZ4(t, data, 64<<10))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, data) {
		t.Fatal("round trip mismatch")
	}

	if _, err := decodeAll(t, []byte("bv4X")); err == nil {
		t.Error("unknown magic accepted")
	}
	if _, err := decodeAll(t, encodeAppleLZ4(t, data, 64<<10)[:1000]); err == nil {
		t.Error("truncated stream accepted")
	}
}

// TestDecodeAppleLZ4Dict checks that a block may copy from the previous
// block's output.
func TestDecodeAppleLZ4Dict(t *testing.T) {
	first := []byte("0123456789abcdefghij")
	// Second block: a match of 8 bytes at offset 10 (into the first block),
	// then the required trailing literals.
	blk := []byte{
		0x04,  // token: 0 literals, match length 4+4
		10, 0, // offset 10
		0x50,                    // token: 5 literals, no match
		'V', 'W', 'X', 'Y', 'Z', // literals
	}
	var s bytes.Buffer
	s.WriteString(magicRawBlock)
	binary.Write(&s, binary.LittleEndian, uint32(len(first)))
	s.Write(first)
	s.WriteString(magicLZ4Block)
	binary.Write(&s, binary.LittleEndian, uint32(13))
	binary.Write(&s, binary.LittleEndian, uint32(len(blk)))
	s.Write(blk)
	s.WriteString(magicEnd)
	got, err := decodeAll(t, s.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	want := string(first) + "abcdefgh" + "VWXYZ"
	if string(got) != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestParseRef(t *testing.T) {
	for _, tt := range []struct{ in, reg, repo, ref string }{
		{"cirruslabs/macos-tahoe-xcode:26.5", "ghcr.io", "cirruslabs/macos-tahoe-xcode", "26.5"},
		{"ghcr.io/cirruslabs/macos-tahoe-xcode:26.5", "ghcr.io", "cirruslabs/macos-tahoe-xcode", "26.5"},
		{"ghcr.io/cirruslabs/macos-tahoe-xcode", "ghcr.io", "cirruslabs/macos-tahoe-xcode", "latest"},
		{"ghcr.io/cirruslabs/x@sha256:abcd", "ghcr.io", "cirruslabs/x", "sha256:abcd"},
		{"localhost:5000/foo/bar:v1", "localhost:5000", "foo/bar", "v1"},
	} {
		r, err := ParseRef(tt.in)
		if err != nil {
			t.Errorf("ParseRef(%q): %v", tt.in, err)
			continue
		}
		if r.Registry != tt.reg || r.Repo != tt.repo || r.TagOrDigest != tt.ref {
			t.Errorf("ParseRef(%q) = %+v", tt.in, r)
		}
	}
}

func sha(b []byte) string {
	s := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(s[:])
}

// fakeRegistry serves one image, requiring a bearer token obtained through
// the WWW-Authenticate challenge.
type fakeRegistry struct {
	blobs      map[string][]byte
	manifest   []byte
	failBlob   atomic.Pointer[string] // blob digest to fail once
	blobGets   atomic.Int64
	corruptOne atomic.Bool
}

func (f *fakeRegistry) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/token" {
		if r.URL.Query().Get("scope") != "repository:test/vm:pull" {
			http.Error(w, "bad scope", 400)
			return
		}
		json.NewEncoder(w).Encode(map[string]string{"token": "tok123"})
		return
	}
	if r.Header.Get("Authorization") != "Bearer tok123" {
		w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer realm="http://%s/token",service="fake",scope="repository:test/vm:pull"`, r.Host))
		http.Error(w, "unauthorized", 401)
		return
	}
	switch {
	case r.URL.Path == "/v2/test/vm/manifests/v1":
		w.Header().Set("Content-Type", mediaTypeManifest)
		w.Write(f.manifest)
	case strings.HasPrefix(r.URL.Path, "/v2/test/vm/blobs/"):
		d := strings.TrimPrefix(r.URL.Path, "/v2/test/vm/blobs/")
		b, ok := f.blobs[d]
		if !ok {
			http.NotFound(w, r)
			return
		}
		f.blobGets.Add(1)
		if p := f.failBlob.Load(); p != nil && *p == d {
			f.failBlob.Store(nil)
			w.Write(b[:len(b)/2]) // truncated
			return
		}
		if f.corruptOne.CompareAndSwap(true, false) && len(b) > 100 {
			c := bytes.Clone(b)
			c[len(c)-10] ^= 0xff
			w.Write(c)
			return
		}
		w.Write(b)
	default:
		http.NotFound(w, r)
	}
}

// newFakeImage builds a fake VM image with a disk of three chunks, one of
// them all zeros.
func newFakeImage(t *testing.T) (*fakeRegistry, []byte) {
	const chunkSize = 256 << 10
	disk := make([]byte, 3*chunkSize-8192) // last chunk is short
	for i := range chunkSize {
		disk[i] = byte(i % 251)
	}
	// Chunk 1 stays zero; chunk 2 has data in its second page only.
	copy(disk[2*chunkSize+pageSize:], bytes.Repeat([]byte("mantsana"), 512))

	f := &fakeRegistry{blobs: map[string][]byte{}}
	add := func(b []byte) string {
		d := sha(b)
		f.blobs[d] = b
		return d
	}
	config := []byte(`{"os":"darwin","arch":"arm64","cpuCount":2,"memorySize":4294967296,` +
		`"diskFormat":"raw","hardwareModel":"aHc=","ecid":"aWQ=","display":{"width":1024,"height":768}}`)
	nvram := []byte("nvram bytes")
	m := manifest{
		MediaType: mediaTypeManifest,
		Config:    descriptor{MediaType: "application/vnd.oci.image.config.v1+json", Digest: add([]byte("{}")), Size: 2},
		Layers: []descriptor{
			{MediaType: mediaTypeConfig, Digest: add(config), Size: int64(len(config))},
		},
	}
	for off := 0; off < len(disk); off += chunkSize {
		part := disk[off:min(off+chunkSize, len(disk))]
		z := encodeAppleLZ4(t, part, 64<<10)
		m.Layers = append(m.Layers, descriptor{
			MediaType: mediaTypeDisk, Digest: add(z), Size: int64(len(z)),
			Annotations: map[string]string{
				annotUncompressedSize:   strconv.Itoa(len(part)),
				annotUncompressedDigest: sha(part),
			},
		})
	}
	m.Layers = append(m.Layers, descriptor{MediaType: mediaTypeNVRAM, Digest: add(nvram), Size: int64(len(nvram))})
	var err error
	if f.manifest, err = json.Marshal(m); err != nil {
		t.Fatal(err)
	}
	return f, disk
}

func testRef(t *testing.T, srv *httptest.Server) Ref {
	r, err := ParseRef(strings.TrimPrefix(srv.URL, "http://") + "/test/vm:v1")
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// withHTTP makes clients use plain HTTP for the duration of the test, for
// httptest servers.
func withHTTP(t *testing.T) {
	old := scheme
	scheme = "http"
	t.Cleanup(func() { scheme = old })
}

func TestDownload(t *testing.T) {
	withHTTP(t)
	f, disk := newFakeImage(t)
	srv := httptest.NewServer(f)
	defer srv.Close()
	dir := filepath.Join(t.TempDir(), "img")
	ctx := context.Background()

	// One truncated chunk response and one corrupted one are retried.
	var chunk2 string
	var m manifest
	json.Unmarshal(f.manifest, &m)
	for _, l := range m.Layers {
		if l.MediaType == mediaTypeDisk {
			chunk2 = l.Digest
		}
	}
	f.failBlob.Store(&chunk2)
	f.corruptOne.Store(true)

	if err := Download(ctx, testRef(t, srv), dir, Options{Logf: t.Logf}); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "disk.img"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, disk) {
		t.Fatal("disk.img differs from the image's disk")
	}
	if b, _ := os.ReadFile(filepath.Join(dir, image.AuxFile)); string(b) != "nvram bytes" {
		t.Errorf("%s = %q", image.AuxFile, b)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, ociManifestFile)); !bytes.Equal(b, f.manifest) {
		t.Error("OCI manifest not saved")
	}
	if _, err := os.Stat(filepath.Join(dir, progressFile)); !os.IsNotExist(err) {
		t.Error("progress file left behind")
	}
	// APFS allocates holes of up to about 16 MiB between written extents
	// (observed on macOS 26.5), so this tiny disk is fully allocated there.
	if blocks := allocatedBlocks(t, filepath.Join(dir, image.DiskFile)); runtime.GOOS != "darwin" && blocks >= 0 && blocks*512 >= int64(len(disk)) {
		t.Errorf("disk.img is not sparse: %d bytes allocated for %d", blocks*512, len(disk))
	}

	// The directory is a mantsana image.
	im, err := (&image.Store{Root: filepath.Dir(dir)}).Get(filepath.Base(dir))
	if err != nil {
		t.Fatalf("loading downloaded image: %v", err)
	}
	mm := im.Manifest
	if mm.OS != "macos" || mm.CPUs != 2 || mm.MemoryBytes != 4<<30 || string(mm.HardwareModel) != "hw" ||
		string(mm.MachineID) != "id" || mm.Display.Width != 1024 || mm.DiskBytes != int64(len(disk)) {
		t.Errorf("image manifest = %+v", mm)
	}

	// A complete image isn't overwritten.
	if err := Download(ctx, testRef(t, srv), dir, Options{Logf: t.Logf}); !errors.Is(err, ErrComplete) {
		t.Errorf("second download: %v", err)
	}
}

func TestBadConfig(t *testing.T) {
	withHTTP(t)
	f, _ := newFakeImage(t)
	var m manifest
	json.Unmarshal(f.manifest, &m)
	for i, l := range m.Layers {
		if l.MediaType == mediaTypeConfig {
			linux := []byte(`{"os":"linux","arch":"arm64","cpuCount":2,"memorySize":1}`)
			m.Layers[i].Digest, m.Layers[i].Size = sha(linux), int64(len(linux))
			f.blobs[sha(linux)] = linux
		}
	}
	f.manifest, _ = json.Marshal(m)
	srv := httptest.NewServer(f)
	defer srv.Close()
	before := f.blobGets.Load()
	err := Download(context.Background(), testRef(t, srv), filepath.Join(t.TempDir(), "img"), Options{Logf: t.Logf})
	if err == nil || !strings.Contains(err.Error(), "only darwin/arm64") {
		t.Fatalf("download of a linux VM: %v", err)
	}
	if n := f.blobGets.Load() - before; n != 1 {
		t.Errorf("fetched %d blobs before rejecting the config, want 1", n)
	}
}

func TestResume(t *testing.T) {
	withHTTP(t)
	f, disk := newFakeImage(t)
	srv := httptest.NewServer(f)
	defer srv.Close()
	dir := filepath.Join(t.TempDir(), "img")
	r := testRef(t, srv)

	// Pretend chunk 0 was already done, with its data in place.
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, progressFile), []byte(sha(f.manifest)+"\n0\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "disk.img"), disk[:256<<10], 0o644)

	before := f.blobGets.Load()
	if err := Download(context.Background(), r, dir, Options{Logf: t.Logf}); err != nil {
		t.Fatal(err)
	}
	// config, nvram, and two of the three chunks.
	if n := f.blobGets.Load() - before; n != 4 {
		t.Errorf("fetched %d blobs on resume, want 4", n)
	}
	got, _ := os.ReadFile(filepath.Join(dir, "disk.img"))
	if !bytes.Equal(got, disk) {
		t.Fatal("resumed disk.img differs")
	}
}
