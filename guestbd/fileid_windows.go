package guestbd

import (
	"os"
	"syscall"
)

// fileIdentity returns a BaseImageKey identifying the open file f by its
// volume serial number and file index, Windows' equivalent of a device
// and inode. It returns nil, disabling coalescing, if they can't be read.
func fileIdentity(f *os.File, fi os.FileInfo) any {
	var d syscall.ByHandleFileInformation
	if err := syscall.GetFileInformationByHandle(syscall.Handle(f.Fd()), &d); err != nil {
		return nil
	}
	return fileIdentityKey{
		dev: uint64(d.VolumeSerialNumber),
		ino: uint64(d.FileIndexHigh)<<32 | uint64(d.FileIndexLow),
	}
}
