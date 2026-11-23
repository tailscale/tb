package ufs

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"
	"testing"
)

// fixtureContent returns the contents of a file in testdata/ufs2.img.gz.
func fixtureContent(name string, n int) []byte {
	var sb strings.Builder
	for i := 0; sb.Len() < n; i++ {
		fmt.Fprintf(&sb, "%s %08d\n", name, i)
	}
	return []byte(sb.String()[:n])
}

// fixtureFiles returns the files in testdata/ufs2.img.gz and their sizes.
//
// The image was made by writing these files to a directory and running, on
// FreeBSD 15.0:
//
//	makefs -t ffs -o version=2,bsize=4096,fsize=512 -s 2m ufs2.img DIR
func fixtureFiles() map[string]int {
	files := map[string]int{
		"hello.txt":        13,
		"empty":            0,
		"a/b/c/nested.txt": 100,
		"frag.bin":         5000,   // ends in a partial block
		"single.bin":       150000, // uses the single indirect block
		"boot/kernel/fake": 70000,
	}
	for i := range 200 {
		files[fmt.Sprintf("many/file%03d", i)] = 20 // spans several directory blocks
	}
	return files
}

func openFixture(t *testing.T) *FS {
	t.Helper()
	gz, err := os.ReadFile("testdata/ufs2.img.gz")
	if err != nil {
		t.Fatal(err)
	}
	zr, err := gzip.NewReader(bytes.NewReader(gz))
	if err != nil {
		t.Fatal(err)
	}
	img, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	fsys, err := Open(bytes.NewReader(img))
	if err != nil {
		t.Fatal(err)
	}
	return fsys
}

func TestFixture(t *testing.T) {
	fsys := openFixture(t)
	for name, n := range fixtureFiles() {
		for _, path := range []string{name, "/" + name} {
			got, err := fsys.ReadFile(path)
			if err != nil {
				t.Errorf("ReadFile(%q): %v", path, err)
				continue
			}
			if want := fixtureContent(name, n); !bytes.Equal(got, want) {
				t.Errorf("ReadFile(%q) = %d bytes, want %d bytes matching", path, len(got), len(want))
			}
		}
	}
}

func TestFixtureReadAt(t *testing.T) {
	fsys := openFixture(t)
	f, err := fsys.Open("single.bin")
	if err != nil {
		t.Fatal(err)
	}
	want := fixtureContent("single.bin", 150000)
	if f.Size() != int64(len(want)) {
		t.Fatalf("Size = %d, want %d", f.Size(), len(want))
	}
	// Reads that straddle block boundaries, including direct to indirect.
	for _, tt := range []struct{ off, n int }{
		{0, 1}, {4090, 20}, {12*4096 - 7, 30}, {100000, 8192}, {149990, 10},
	} {
		buf := make([]byte, tt.n)
		n, err := f.ReadAt(buf, int64(tt.off))
		if err != nil || n != tt.n {
			t.Errorf("ReadAt(%d bytes at %d) = %d, %v", tt.n, tt.off, n, err)
			continue
		}
		if !bytes.Equal(buf, want[tt.off:tt.off+tt.n]) {
			t.Errorf("ReadAt(%d bytes at %d) returned wrong data", tt.n, tt.off)
		}
	}
	buf := make([]byte, 20)
	if n, err := f.ReadAt(buf, 149990); n != 10 || err != io.EOF {
		t.Errorf("ReadAt past end = %d, %v; want 10, EOF", n, err)
	}
}

func TestFixtureErrors(t *testing.T) {
	fsys := openFixture(t)
	if _, err := fsys.Open("nope"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Open(nope) = %v, want ErrNotExist", err)
	}
	if _, err := fsys.Open("a/b"); err == nil {
		t.Error("Open(directory) succeeded")
	}
	if _, err := fsys.Open("hello.txt/x"); err == nil {
		t.Error("Open(file/x) succeeded")
	}
	if _, err := Open(bytes.NewReader(make([]byte, 1<<20))); err == nil {
		t.Error("Open(zeros) succeeded")
	}
}

// synthDisk is an in-memory disk whose fragments are allocated on demand,
// used to exercise block maps too large for a checked-in fixture.
type synthDisk struct {
	fsys *FS
	data []byte
}

func newSynthDisk() *synthDisk {
	d := &synthDisk{}
	d.fsys = &FS{
		r:      bytes.NewReader(nil), // replaced by reader
		bsize:  512,
		fsize:  512,
		frag:   1,
		iblkno: 1,
		ipg:    4,
		fpg:    64,
		inopb:  2,
		nindir: 64,
	}
	d.data = make([]byte, 4*64*512) // 4 cylinder groups
	return d
}

// alloc returns the fragment address of a new block filled with b.
func (d *synthDisk) alloc(b []byte) int64 {
	addr := int64(len(d.data)) / d.fsys.fsize
	blk := make([]byte, d.fsys.bsize)
	copy(blk, b)
	d.data = append(d.data, blk...)
	return addr
}

func (d *synthDisk) reader() *FS {
	d.fsys.r = bytes.NewReader(d.data)
	return d.fsys
}

func ptrBlock(ptrs map[int]int64) []byte {
	b := make([]byte, 512)
	for i, p := range ptrs {
		binary.LittleEndian.PutUint64(b[8*i:], uint64(p))
	}
	return b
}

func TestIndirectLevels(t *testing.T) {
	d := newSynthDisk()
	const nindir = 64
	// Logical block numbers that land at each level of the block map.
	direct := int64(3)
	single := int64(12 + 5)
	double := int64(12 + nindir + 2*nindir + 7)
	triple := int64(12 + nindir + nindir*nindir + 1*nindir*nindir + 3*nindir + 9)

	blockData := func(lbn int64) []byte {
		return bytes.Repeat([]byte(fmt.Sprintf("<%d>", lbn)), 512)[:512]
	}
	f := &File{size: (triple + 2) * 512}
	f.db[direct] = d.alloc(blockData(direct))
	f.ib[0] = d.alloc(ptrBlock(map[int]int64{5: d.alloc(blockData(single))}))
	f.ib[1] = d.alloc(ptrBlock(map[int]int64{
		2: d.alloc(ptrBlock(map[int]int64{7: d.alloc(blockData(double))})),
	}))
	f.ib[2] = d.alloc(ptrBlock(map[int]int64{
		1: d.alloc(ptrBlock(map[int]int64{
			3: d.alloc(ptrBlock(map[int]int64{9: d.alloc(blockData(triple))})),
		})),
	}))
	f.fs = d.reader()

	for _, lbn := range []int64{direct, single, double, triple} {
		buf := make([]byte, 512)
		if _, err := f.ReadAt(buf, lbn*512); err != nil {
			t.Fatalf("ReadAt block %d: %v", lbn, err)
		}
		if !bytes.Equal(buf, blockData(lbn)) {
			t.Errorf("block %d = %q..., want %q...", lbn, buf[:16], blockData(lbn)[:16])
		}
	}
	// Unallocated blocks at every level read as zeros.
	for _, lbn := range []int64{0, 13, double - 1, triple - 1, triple + 1} {
		buf := bytes.Repeat([]byte{0xff}, 512)
		if _, err := f.ReadAt(buf, lbn*512); err != nil {
			t.Fatalf("ReadAt hole %d: %v", lbn, err)
		}
		if !bytes.Equal(buf, make([]byte, 512)) {
			t.Errorf("hole at block %d not zero", lbn)
		}
	}
}

func TestInodeInLaterCylinderGroup(t *testing.T) {
	d := newSynthDisk()
	fsys := d.reader()
	// Inode 7 is in cylinder group 7/ipg = 1, inode block (7%4)/2 = 1,
	// slot 7%2 = 1.
	const ino = 7
	off := (1*fsys.fpg+fsys.iblkno+1*fsys.frag)*fsys.fsize + 1*inodeSize
	blk := d.alloc([]byte("in cg 1"))
	binary.LittleEndian.PutUint16(d.data[off:], modeReg|0o644)
	binary.LittleEndian.PutUint64(d.data[off+16:], 512)
	binary.LittleEndian.PutUint64(d.data[off+112:], uint64(blk))
	fsys = d.reader()

	f, mode, err := fsys.inode(ino)
	if err != nil {
		t.Fatal(err)
	}
	if mode&modeFmt != modeReg || f.Size() != 512 {
		t.Fatalf("inode %d: mode %o size %d", ino, mode, f.Size())
	}
	buf := make([]byte, 7)
	if _, err := f.ReadAt(buf, 0); err != nil || string(buf) != "in cg 1" {
		t.Errorf("ReadAt = %q, %v", buf, err)
	}
}
