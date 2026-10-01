//go:build !unix && !windows

package guestbd

import "os"

// fileIdentity returns nil, as files have no device and inode identity
// here. Base images opened from files are then never coalesced.
func fileIdentity(f *os.File, fi os.FileInfo) any {
	return nil
}
