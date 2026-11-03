// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

//go:build unix

package guestnet

import (
	"fmt"
	"os"
	"syscall"
)

// Buffer sizes for the guest datagram link. Apple documents
// (VZFileHandleNetworkDeviceAttachment.maximumTransmissionUnit) that
// SO_RCVBUF must be at least twice SO_SNDBUF, and recommends four times.
// The absolute sizes are our guess, room for a few hundred full-size frames,
// and well under macOS's default kern.ipc.maxsockbuf of 8 MiB. Tune them
// with throughput measurements.
const (
	linkSendBuf = 512 << 10
	linkRecvBuf = 4 * linkSendBuf
)

// NewSocketPairLink creates a connected SOCK_DGRAM unix socketpair. The
// returned *os.File is for the VM side (hand it to the network device
// attachment; the framework dups it, so close it after the VM is created).
// The returned FileLink is our side.
func NewSocketPairLink() (vmSide *os.File, host *FileLink, err error) {
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_DGRAM, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("socketpair: %w", err)
	}
	for _, fd := range fds {
		if err := syscall.SetsockoptInt(fd, syscall.SOL_SOCKET, syscall.SO_SNDBUF, linkSendBuf); err != nil {
			closeFDs(fds)
			return nil, nil, fmt.Errorf("SO_SNDBUF: %w", err)
		}
		if err := syscall.SetsockoptInt(fd, syscall.SOL_SOCKET, syscall.SO_RCVBUF, linkRecvBuf); err != nil {
			closeFDs(fds)
			return nil, nil, fmt.Errorf("SO_RCVBUF: %w", err)
		}
		syscall.CloseOnExec(fd)
	}
	// Our end must be non-blocking before os.NewFile sees it, so the file
	// uses the runtime poller and Close unblocks a pending ReadFrame. The
	// VM end stays blocking, as the framework expects.
	if err := syscall.SetNonblock(fds[1], true); err != nil {
		closeFDs(fds)
		return nil, nil, fmt.Errorf("SetNonblock: %w", err)
	}
	vmSide = os.NewFile(uintptr(fds[0]), "guestnet-vm")
	hostFile := os.NewFile(uintptr(fds[1]), "guestnet-host")
	return vmSide, NewFileLink(hostFile), nil
}

func closeFDs(fds [2]int) {
	for _, fd := range fds {
		syscall.Close(fd)
	}
}
