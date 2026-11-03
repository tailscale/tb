// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package ocidl

import (
	"encoding/binary"
	"fmt"
	"io"
	"slices"

	"github.com/pierrec/lz4/v4"
)

// Block magics of Apple's libcompression LZ4 stream format, as found in the
// disk layers of these images.
const (
	magicLZ4Block = "bv41" // u32le decoded size, u32le encoded size, LZ4 block
	magicRawBlock = "bv4-" // u32le size, uncompressed bytes
	magicEnd      = "bv4$" // end of stream
)

// lz4Window is how far back an LZ4 block may refer.
const lz4Window = 64 << 10

// decodeAppleLZ4 decodes an Apple LZ4 stream from r, calling emit with each
// decoded block in order. The slice passed to emit is only valid during the
// call. Blocks are decoded with the preceding 64 KiB of output as their
// dictionary, since a block may refer back into earlier output.
func decodeAppleLZ4(r io.Reader, emit func([]byte) error) error {
	var (
		hdr      [12]byte
		src, dst []byte
		dict     []byte
	)
	for {
		if _, err := io.ReadFull(r, hdr[:4]); err != nil {
			return fmt.Errorf("reading block header: %w", err)
		}
		switch string(hdr[:4]) {
		case magicEnd:
			return nil
		case magicLZ4Block:
			if _, err := io.ReadFull(r, hdr[4:12]); err != nil {
				return fmt.Errorf("reading block header: %w", err)
			}
			dn := int(binary.LittleEndian.Uint32(hdr[4:8]))
			en := int(binary.LittleEndian.Uint32(hdr[8:12]))
			src = slices.Grow(src[:0], en)[:en]
			if _, err := io.ReadFull(r, src); err != nil {
				return fmt.Errorf("reading block: %w", err)
			}
			dst = slices.Grow(dst[:0], dn)[:dn]
			n, err := lz4.UncompressBlockWithDict(src, dst, dict)
			if err != nil {
				return fmt.Errorf("decoding LZ4 block: %w", err)
			}
			if n != dn {
				return fmt.Errorf("LZ4 block decoded to %d bytes, want %d", n, dn)
			}
		case magicRawBlock:
			if _, err := io.ReadFull(r, hdr[4:8]); err != nil {
				return fmt.Errorf("reading block header: %w", err)
			}
			dn := int(binary.LittleEndian.Uint32(hdr[4:8]))
			dst = slices.Grow(dst[:0], dn)[:dn]
			if _, err := io.ReadFull(r, dst); err != nil {
				return fmt.Errorf("reading block: %w", err)
			}
		default:
			return fmt.Errorf("unknown block magic %q", hdr[:4])
		}
		if err := emit(dst); err != nil {
			return err
		}
		dict = append(dict, dst...)
		if len(dict) > lz4Window {
			dict = append(dict[:0], dict[len(dict)-lz4Window:]...)
		}
	}
}
