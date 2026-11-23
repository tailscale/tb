package ufs

import (
	"encoding/binary"
	"fmt"
	"io"
	"unicode/utf16"
)

// sectorSize is the logical sector size assumed for GPT disks.
const sectorSize = 512

// OpenGPT returns the UFS2 file system in the partition labeled label
// (for example "rootfs", as in /dev/gpt/rootfs) of the GPT-partitioned
// disk r.
func OpenGPT(r io.ReaderAt, label string) (*FS, error) {
	hdr := make([]byte, 92)
	if _, err := r.ReadAt(hdr, sectorSize); err != nil {
		return nil, fmt.Errorf("ufs: reading GPT header: %w", err)
	}
	if string(hdr[:8]) != "EFI PART" {
		return nil, fmt.Errorf("ufs: no GPT header")
	}
	le := binary.LittleEndian
	entriesLBA := int64(le.Uint64(hdr[72:]))
	numEntries := int64(le.Uint32(hdr[80:]))
	entrySize := int64(le.Uint32(hdr[84:]))
	if entrySize < 128 || numEntries > 1024 {
		return nil, fmt.Errorf("ufs: implausible GPT header")
	}
	ents := make([]byte, numEntries*entrySize)
	if _, err := r.ReadAt(ents, entriesLBA*sectorSize); err != nil {
		return nil, fmt.Errorf("ufs: reading GPT entries: %w", err)
	}
	for i := range numEntries {
		e := ents[i*entrySize : (i+1)*entrySize]
		first, last := int64(le.Uint64(e[32:])), int64(le.Uint64(e[40:]))
		if first == 0 || gptName(e[56:128]) != label {
			continue
		}
		return Open(io.NewSectionReader(r, first*sectorSize, (last-first+1)*sectorSize))
	}
	return nil, fmt.Errorf("ufs: no GPT partition labeled %q", label)
}

// gptName decodes a NUL-padded UTF-16LE GPT partition name.
func gptName(b []byte) string {
	u := make([]uint16, 0, len(b)/2)
	for i := 0; i+1 < len(b); i += 2 {
		c := binary.LittleEndian.Uint16(b[i:])
		if c == 0 {
			break
		}
		u = append(u, c)
	}
	return string(utf16.Decode(u))
}
