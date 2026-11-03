// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package guestnet

import (
	"encoding/binary"
	"fmt"
	"net"
	"os"
	"sync"

	"golang.org/x/sys/unix"
)

// virtioNetHdrLen is the size of struct virtio_net_hdr (without the
// mergeable-buffers num_buffers field), which PACKET_VNET_HDR prepends to
// every frame read from and written to the socket.
const virtioNetHdrLen = 10

// virtioNetHdrFNeedsCsum is VIRTIO_NET_HDR_F_NEEDS_CSUM: the frame's L4
// checksum is partial (the pseudo-header sum only) and must be completed
// over [csum_start, end) and stored at csum_start+csum_offset.
const virtioNetHdrFNeedsCsum = 1

// TAPLink is a [FrameLink] over the host side of a Linux TAP device whose
// other side belongs to a VMM that opens the TAP by name, such as Firecracker,
// which has no other network backend. The TAP is a pure L2 pipe: it has no
// addresses, and frames are read and written with an AF_PACKET socket bound
// to it, so the host's network stack never handles guest traffic.
//
// The VMM typically enables checksum and segmentation offloads on the TAP, so
// guest frames can arrive with a partial checksum or as one large TCP
// segment. The socket uses PACKET_VNET_HDR to learn which; TAPLink completes
// partial checksums itself and passes large segments through whole.
type TAPLink struct {
	f       *os.File
	bufPool sync.Pool // of *[]byte, for WriteFrame
}

// NewTAPLink opens a TAPLink on the existing interface ifname. The TAP should
// be up. It needs CAP_NET_RAW, plus CAP_NET_ADMIN to tune the interface's
// sysctls (best effort).
//
// To keep the host's stack off the interface, it disables IPv6 and ARP
// replies on it, ignoring errors (for example, if IPv6 is disabled
// host-wide).
func NewTAPLink(ifname string) (*TAPLink, error) {
	ifi, err := net.InterfaceByName(ifname)
	if err != nil {
		return nil, fmt.Errorf("guestnet: TAP %q: %w", ifname, err)
	}
	quietInterface(ifname)

	fd, err := unix.Socket(unix.AF_PACKET, unix.SOCK_RAW|unix.SOCK_NONBLOCK|unix.SOCK_CLOEXEC, int(htons(unix.ETH_P_ALL)))
	if err != nil {
		return nil, fmt.Errorf("guestnet: AF_PACKET socket: %w", err)
	}
	ok := false
	defer func() {
		if !ok {
			unix.Close(fd)
		}
	}()
	if err := unix.SetsockoptInt(fd, unix.SOL_PACKET, unix.PACKET_VNET_HDR, 1); err != nil {
		return nil, fmt.Errorf("guestnet: PACKET_VNET_HDR: %w", err)
	}
	// Without this, we'd read back every frame we write.
	if err := unix.SetsockoptInt(fd, unix.SOL_PACKET, unix.PACKET_IGNORE_OUTGOING, 1); err != nil {
		return nil, fmt.Errorf("guestnet: PACKET_IGNORE_OUTGOING: %w", err)
	}
	for _, o := range []struct{ opt, size int }{{unix.SO_SNDBUF, linkSendBuf}, {unix.SO_RCVBUF, linkRecvBuf}} {
		if err := unix.SetsockoptInt(fd, unix.SOL_SOCKET, o.opt, o.size); err != nil {
			return nil, fmt.Errorf("guestnet: setting socket buffer: %w", err)
		}
	}
	sa := &unix.SockaddrLinklayer{
		Protocol: htons(unix.ETH_P_ALL),
		Ifindex:  ifi.Index,
	}
	if err := unix.Bind(fd, sa); err != nil {
		return nil, fmt.Errorf("guestnet: binding AF_PACKET socket to %q: %w", ifname, err)
	}
	ok = true
	// The fd is non-blocking, so os.NewFile puts it in the runtime poller and
	// Close unblocks a pending ReadFrame.
	return &TAPLink{f: os.NewFile(uintptr(fd), "tap:"+ifname)}, nil
}

// quietInterface makes a best effort to keep the host's network stack from
// talking on, or answering for, the interface ifname.
func quietInterface(ifname string) {
	for _, s := range []struct{ path, val string }{
		{"/proc/sys/net/ipv6/conf/" + ifname + "/disable_ipv6", "1"},
		{"/proc/sys/net/ipv4/conf/" + ifname + "/arp_ignore", "8"}, // never reply
		{"/proc/sys/net/ipv4/conf/" + ifname + "/forwarding", "0"},
	} {
		// Errors aren't fatal: the interface has no addresses either way.
		os.WriteFile(s.path, []byte(s.val), 0)
	}
}

func htons(v uint16) uint16 { return v<<8 | v>>8 }

// ReadFrame implements FrameLink.
func (l *TAPLink) ReadFrame(buf []byte) (int, error) {
	for {
		// Read the virtio header into buf too, then slide the frame down.
		// Frames are at most 64 KiB, so the copy is cheap next to the
		// syscall.
		n, err := l.f.Read(buf)
		if err != nil {
			return 0, err
		}
		if n < virtioNetHdrLen {
			continue
		}
		hdr := buf[:virtioNetHdrLen]
		frame := buf[virtioNetHdrLen:n]
		if hdr[0]&virtioNetHdrFNeedsCsum != 0 {
			start := int(binary.LittleEndian.Uint16(hdr[6:8]))
			off := int(binary.LittleEndian.Uint16(hdr[8:10]))
			if start+off+2 > len(frame) {
				continue // malformed; drop
			}
			completeChecksum(frame[start:], off)
		}
		return copy(buf, frame), nil
	}
}

// completeChecksum finishes a partial internet checksum: b starts at the
// checksummed region, whose checksum field at off already holds the folded
// pseudo-header sum.
func completeChecksum(b []byte, off int) {
	var sum uint32
	i := 0
	for ; i+1 < len(b); i += 2 {
		sum += uint32(b[i])<<8 | uint32(b[i+1])
	}
	if i < len(b) {
		sum += uint32(b[i]) << 8
	}
	for sum>>16 != 0 {
		sum = sum&0xffff + sum>>16
	}
	binary.BigEndian.PutUint16(b[off:], ^uint16(sum))
}

// WriteFrame implements FrameLink.
func (l *TAPLink) WriteFrame(frame []byte) error {
	bp, _ := l.bufPool.Get().(*[]byte)
	if bp == nil {
		bp = new([]byte)
	}
	b := append((*bp)[:0], make([]byte, virtioNetHdrLen)...) // zero header: no offloads
	b = append(b, frame...)
	_, err := l.f.Write(b)
	*bp = b
	l.bufPool.Put(bp)
	return err
}

// Close implements FrameLink.
func (l *TAPLink) Close() error { return l.f.Close() }
