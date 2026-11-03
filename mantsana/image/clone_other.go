// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

//go:build !darwin

package image

// CloneFile copies src to dst. Only macOS has the O(1) APFS clone; elsewhere
// this is a plain copy, which is fine for development and tests.
func CloneFile(src, dst string) error { return copyFile(src, dst) }
