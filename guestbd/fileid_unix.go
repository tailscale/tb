//go:build unix

package guestbd

import (
	"os"
	"syscall"
)

// fileIdentity returns a BaseImageKey identifying the open file f,
// described by fi, by its device and inode.
func fileIdentity(f *os.File, fi os.FileInfo) any {
	st := fi.Sys().(*syscall.Stat_t)
	return fileIdentityKey{dev: uint64(st.Dev), ino: st.Ino} // Dev is int32 on darwin
}
