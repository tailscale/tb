// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package image

import (
	"errors"
	"fmt"

	"golang.org/x/sys/unix"
)

// CloneFile makes dst a copy-on-write clone of src. On APFS this is O(1)
// regardless of file size and consumes no space until written. dst must not
// exist. It falls back to a regular copy if the filesystem can't clone.
func CloneFile(src, dst string) error {
	err := unix.Clonefile(src, dst, 0)
	if err == nil {
		return nil
	}
	if errors.Is(err, unix.ENOTSUP) || errors.Is(err, unix.EXDEV) {
		return copyFile(src, dst)
	}
	return fmt.Errorf("clonefile %s -> %s: %w", src, dst, err)
}
