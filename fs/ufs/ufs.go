// Package ufs provides read-only access to files in a FreeBSD UFS2 file
// system.
//
// It supports only what's needed to pull files out of a cleanly unmounted
// image: superblock and inode lookup, directory traversal, and the direct
// and indirect block maps. It ignores soft updates journals, snapshots,
// extended attributes, and UFS1.
package ufs

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"strings"
)

const (
	sblockOffset = 65536      // SBLOCK_UFS2
	ufs2Magic    = 0x19540119 // FS_UFS2_MAGIC
	rootIno      = 2          // UFS_ROOTINO
	inodeSize    = 256        // sizeof(struct ufs2_dinode)
	nDirect      = 12         // UFS_NDADDR
	nIndirect    = 3          // UFS_NIADDR

	modeFmt = 0o170000 // IFMT
	modeDir = 0o040000 // IFDIR
	modeReg = 0o100000 // IFREG
)

// FS is a UFS2 file system.
type FS struct {
	r io.ReaderAt

	bsize  int64  // block size in bytes
	fsize  int64  // fragment size in bytes
	frag   int64  // fragments per block
	iblkno int64  // fragment offset of the inode blocks within a cylinder group
	ipg    uint64 // inodes per cylinder group
	fpg    int64  // fragments per cylinder group
	inopb  uint64 // inodes per block
	nindir int64  // block pointers per indirect block
}

// Open returns the UFS2 file system whose first byte is at offset 0 of r.
func Open(r io.ReaderAt) (*FS, error) {
	sb := make([]byte, 1376)
	if _, err := r.ReadAt(sb, sblockOffset); err != nil {
		return nil, fmt.Errorf("ufs: reading superblock: %w", err)
	}
	le := binary.LittleEndian
	if m := le.Uint32(sb[1372:]); m != ufs2Magic {
		return nil, fmt.Errorf("ufs: bad superblock magic %#x", m)
	}
	f := &FS{
		r:      r,
		iblkno: int64(int32(le.Uint32(sb[16:]))),
		bsize:  int64(int32(le.Uint32(sb[48:]))),
		fsize:  int64(int32(le.Uint32(sb[52:]))),
		frag:   int64(int32(le.Uint32(sb[56:]))),
		nindir: int64(int32(le.Uint32(sb[116:]))),
		inopb:  uint64(le.Uint32(sb[120:])),
		ipg:    uint64(le.Uint32(sb[184:])),
		fpg:    int64(int32(le.Uint32(sb[188:]))),
	}
	if f.bsize <= 0 || f.fsize <= 0 || f.bsize%f.fsize != 0 || f.bsize/f.fsize != f.frag ||
		f.ipg == 0 || f.inopb == 0 || f.fpg <= 0 || f.nindir != f.bsize/8 {
		return nil, errors.New("ufs: implausible superblock geometry")
	}
	return f, nil
}

// File is a regular file within a FS.
type File struct {
	fs   *FS
	size int64
	db   [nDirect]int64   // direct block fragment addresses
	ib   [nIndirect]int64 // indirect block fragment addresses

	// ind caches the most recently read indirect block at each level, keyed
	// by fragment address, so sequential reads don't re-read it per block.
	ind [nIndirect]struct {
		addr int64
		ptrs []int64
	}
}

// Size returns the size of the file in bytes.
func (f *File) Size() int64 { return f.size }

// Open returns the regular file at name, a slash-separated path relative
// to the root of the file system. Symlinks are not followed.
func (fsys *FS) Open(name string) (*File, error) {
	ino := uint64(rootIno)
	for elem := range strings.SplitSeq(strings.Trim(name, "/"), "/") {
		if elem == "" {
			continue
		}
		dir, mode, err := fsys.inode(ino)
		if err != nil {
			return nil, err
		}
		if mode&modeFmt != modeDir {
			return nil, &fs.PathError{Op: "open", Path: name, Err: errors.New("not a directory")}
		}
		ino, err = dir.lookup(elem)
		if err != nil {
			return nil, &fs.PathError{Op: "open", Path: name, Err: err}
		}
	}
	f, mode, err := fsys.inode(ino)
	if err != nil {
		return nil, err
	}
	if mode&modeFmt != modeReg {
		return nil, &fs.PathError{Op: "open", Path: name, Err: errors.New("not a regular file")}
	}
	return f, nil
}

// ReadFile returns the contents of the regular file at name.
func (fsys *FS) ReadFile(name string) ([]byte, error) {
	f, err := fsys.Open(name)
	if err != nil {
		return nil, err
	}
	buf := make([]byte, f.size)
	if _, err := f.ReadAt(buf, 0); err != nil && err != io.EOF {
		return nil, err
	}
	return buf, nil
}

// inode reads inode number ino, returning it as a File and its mode bits.
func (fsys *FS) inode(ino uint64) (*File, uint16, error) {
	cg := int64(ino / fsys.ipg)
	blk := int64((ino % fsys.ipg) / fsys.inopb)
	fragAddr := cg*fsys.fpg + fsys.iblkno + blk*fsys.frag
	off := fragAddr*fsys.fsize + int64(ino%fsys.inopb)*inodeSize

	buf := make([]byte, inodeSize)
	if _, err := fsys.r.ReadAt(buf, off); err != nil {
		return nil, 0, fmt.Errorf("ufs: reading inode %d: %w", ino, err)
	}
	le := binary.LittleEndian
	f := &File{fs: fsys, size: int64(le.Uint64(buf[16:]))}
	if f.size < 0 {
		return nil, 0, fmt.Errorf("ufs: inode %d has negative size", ino)
	}
	for i := range f.db {
		f.db[i] = int64(le.Uint64(buf[112+8*i:]))
	}
	for i := range f.ib {
		f.ib[i] = int64(le.Uint64(buf[208+8*i:]))
	}
	return f, le.Uint16(buf[0:]), nil
}

// lookup returns the inode number of the directory entry named name.
func (f *File) lookup(name string) (uint64, error) {
	data := make([]byte, f.size)
	if _, err := f.ReadAt(data, 0); err != nil && err != io.EOF {
		return 0, err
	}
	le := binary.LittleEndian
	for len(data) >= 8 {
		ino := le.Uint32(data[0:])
		reclen := int(le.Uint16(data[4:]))
		namlen := int(data[7])
		if reclen < 8 || reclen > len(data) || 8+namlen > reclen {
			return 0, errors.New("ufs: corrupt directory entry")
		}
		if ino != 0 && string(data[8:8+namlen]) == name {
			return uint64(ino), nil
		}
		data = data[reclen:]
	}
	return 0, fs.ErrNotExist
}

// ReadAt implements io.ReaderAt.
func (f *File) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, errors.New("ufs: negative offset")
	}
	n := 0
	for len(p) > 0 {
		if off >= f.size {
			return n, io.EOF
		}
		lbn := off / f.fs.bsize
		blkOff := off % f.fs.bsize
		chunk := min(int64(len(p)), f.fs.bsize-blkOff, f.size-off)

		addr, err := f.bmap(lbn)
		if err != nil {
			return n, err
		}
		if addr == 0 {
			clear(p[:chunk]) // hole
		} else if _, err := f.fs.r.ReadAt(p[:chunk], addr*f.fs.fsize+blkOff); err != nil {
			return n, fmt.Errorf("ufs: reading block: %w", err)
		}
		n += int(chunk)
		p = p[chunk:]
		off += chunk
	}
	return n, nil
}

// bmap returns the fragment address of logical block lbn, or 0 for a hole.
func (f *File) bmap(lbn int64) (int64, error) {
	if lbn < nDirect {
		return f.db[lbn], nil
	}
	lbn -= nDirect
	// span is the number of data blocks addressed by one pointer at the
	// current level: 1 for single indirect, nindir for double, and so on.
	span := int64(1)
	for level := range nIndirect {
		capacity := span * f.fs.nindir
		if lbn >= capacity {
			lbn -= capacity
			span = capacity
			continue
		}
		addr := f.ib[level]
		for depth := level; ; depth-- {
			if addr == 0 {
				return 0, nil
			}
			ptrs, err := f.indirect(depth, addr)
			if err != nil {
				return 0, err
			}
			addr = ptrs[lbn/span]
			if depth == 0 {
				return addr, nil
			}
			lbn %= span
			span /= f.fs.nindir
		}
	}
	return 0, errors.New("ufs: file offset beyond triple indirect range")
}

// indirect returns the block pointers in the indirect block at fragment
// address addr, using the per-level cache.
func (f *File) indirect(level int, addr int64) ([]int64, error) {
	c := &f.ind[level]
	if c.addr == addr && c.ptrs != nil {
		return c.ptrs, nil
	}
	buf := make([]byte, f.fs.bsize)
	if _, err := f.fs.r.ReadAt(buf, addr*f.fs.fsize); err != nil {
		return nil, fmt.Errorf("ufs: reading indirect block: %w", err)
	}
	ptrs := make([]int64, f.fs.nindir)
	if err := binary.Read(bytes.NewReader(buf), binary.LittleEndian, ptrs); err != nil {
		return nil, err
	}
	c.addr, c.ptrs = addr, ptrs
	return ptrs, nil
}
