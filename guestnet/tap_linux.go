// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package guestnet

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"syscall"

	"github.com/tailscale/netlink"
	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/tcpip/checksum"
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
// addresses, frames are read and written with an AF_PACKET socket bound to
// it, and the host's network stack never handles guest traffic (see
// [NewTAPLink]).
//
// The VMM typically enables checksum and segmentation offloads on the TAP, so
// guest frames can arrive with a partial checksum or as one large TCP
// segment. The socket uses PACKET_VNET_HDR to learn which; TAPLink completes
// partial checksums itself and passes large segments through whole.
type TAPLink struct {
	f  *os.File
	rc syscall.RawConn
}

// NewTAPLink opens a TAPLink on the existing interface ifname. The TAP should
// be up. It needs CAP_NET_RAW and CAP_NET_ADMIN.
//
// It keeps the host's network stack off the interface entirely: a tc clsact
// qdisc with a match-all drop filter on the interface's ingress drops every
// frame from the guest right after the AF_PACKET socket sees it, before any
// protocol handler does. As defense in depth, it also disables IPv6, ARP
// replies and forwarding on the interface. It fails if it can't do any of
// that, except for settings that don't exist (such as IPv6's, if IPv6 is
// disabled host-wide). Deleting the interface removes all of it.
func NewTAPLink(ifname string) (*TAPLink, error) {
	link, err := netlink.LinkByName(ifname)
	if err != nil {
		return nil, fmt.Errorf("guestnet: TAP %q: %w", ifname, err)
	}
	if err := dropIngress(link); err != nil {
		return nil, fmt.Errorf("guestnet: TAP %q: %w", ifname, err)
	}
	if err := quietInterface(ifname); err != nil {
		return nil, fmt.Errorf("guestnet: TAP %q: %w", ifname, err)
	}

	// Protocol 0 receives nothing until Bind names the protocol along with
	// the interface, so no other interface's frames are queued in between.
	fd, err := unix.Socket(unix.AF_PACKET, unix.SOCK_RAW|unix.SOCK_NONBLOCK|unix.SOCK_CLOEXEC, 0)
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
		Ifindex:  link.Attrs().Index,
	}
	if err := unix.Bind(fd, sa); err != nil {
		return nil, fmt.Errorf("guestnet: binding AF_PACKET socket to %q: %w", ifname, err)
	}
	// The fd is non-blocking, so os.NewFile puts it in the runtime poller and
	// Close unblocks a pending ReadFrame.
	ok = true
	f := os.NewFile(uintptr(fd), "tap:"+ifname)
	rc, err := f.SyscallConn()
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("guestnet: TAP %q: %w", ifname, err)
	}
	return &TAPLink{f: f, rc: rc}, nil
}

// dropIngress adds a clsact qdisc to link with an ingress filter that drops
// everything. AF_PACKET sockets see frames before tc ingress does, so
// TAPLink still reads them; the host's protocol handlers come after it, so
// they never do.
func dropIngress(link netlink.Link) error {
	idx := link.Attrs().Index
	qdisc := &netlink.GenericQdisc{
		QdiscAttrs: netlink.QdiscAttrs{
			LinkIndex: idx,
			Handle:    netlink.MakeHandle(0xffff, 0),
			Parent:    netlink.HANDLE_CLSACT,
		},
		QdiscType: "clsact",
	}
	if err := netlink.QdiscReplace(qdisc); err != nil {
		return fmt.Errorf("adding clsact qdisc: %w", err)
	}
	filter := &netlink.MatchAll{
		FilterAttrs: netlink.FilterAttrs{
			LinkIndex: idx,
			Parent:    netlink.HANDLE_MIN_INGRESS,
			Handle:    netlink.MakeHandle(0, 1),
			Priority:  1,
			Protocol:  unix.ETH_P_ALL,
		},
		Actions: []netlink.Action{
			&netlink.GenericAction{ActionAttrs: netlink.ActionAttrs{Action: netlink.TC_ACT_SHOT}},
		},
	}
	if err := netlink.FilterReplace(filter); err != nil {
		return fmt.Errorf("adding ingress drop filter: %w", err)
	}
	return nil
}

// quietInterface keeps the host's network stack from talking on, or
// answering for, the interface ifname. Settings that don't exist are
// skipped.
func quietInterface(ifname string) error {
	for _, s := range []struct{ path, val string }{
		{"/proc/sys/net/ipv6/conf/" + ifname + "/disable_ipv6", "1"},
		{"/proc/sys/net/ipv4/conf/" + ifname + "/arp_ignore", "8"}, // never reply
		{"/proc/sys/net/ipv4/conf/" + ifname + "/forwarding", "0"},
	} {
		if err := os.WriteFile(s.path, []byte(s.val), 0); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return nil
}

func htons(v uint16) uint16 { return v<<8 | v>>8 }

// ReadFrame implements FrameLink. The virtio header is read into a buffer of
// its own, so all of buf is available for the frame.
func (tl *TAPLink) ReadFrame(buf []byte) (int, error) {
	var hdr [virtioNetHdrLen]byte
	iov := [][]byte{hdr[:], buf}
	for {
		var n int
		var rerr error
		err := tl.rc.Read(func(fd uintptr) bool {
			for {
				n, rerr = unix.Readv(int(fd), iov)
				if rerr != unix.EINTR {
					break
				}
			}
			return rerr != unix.EAGAIN
		})
		if err == nil {
			err = rerr
		}
		if err != nil {
			return 0, err
		}
		if n < virtioNetHdrLen {
			continue
		}
		frame := buf[:n-virtioNetHdrLen]
		if hdr[0]&virtioNetHdrFNeedsCsum != 0 {
			start := int(binary.LittleEndian.Uint16(hdr[6:8]))
			off := int(binary.LittleEndian.Uint16(hdr[8:10]))
			if start+off+2 > len(frame) {
				continue // malformed; drop
			}
			completeChecksum(frame[start:], off)
		}
		return len(frame), nil
	}
}

// completeChecksum finishes a partial internet checksum: b starts at the
// checksummed region, whose checksum field at off already holds the folded
// pseudo-header sum.
func completeChecksum(b []byte, off int) {
	binary.BigEndian.PutUint16(b[off:], ^checksum.Checksum(b, 0))
}

// WriteFrame implements FrameLink.
func (tl *TAPLink) WriteFrame(frame []byte) error {
	var hdr [virtioNetHdrLen]byte // zero: no offloads
	iov := [][]byte{hdr[:], frame}
	var werr error
	err := tl.rc.Write(func(fd uintptr) bool {
		for {
			_, werr = unix.Writev(int(fd), iov)
			if werr != unix.EINTR {
				break
			}
		}
		return werr != unix.EAGAIN
	})
	if err == nil {
		err = werr
	}
	return err
}

// Close implements FrameLink.
func (tl *TAPLink) Close() error { return tl.f.Close() }
