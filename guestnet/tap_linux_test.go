// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package guestnet

import (
	"bytes"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/tailscale/netlink"
	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/checksum"
	"gvisor.dev/gvisor/pkg/tcpip/header"
)

func TestCompleteChecksum(t *testing.T) {
	for _, payloadLen := range []int{0, 1, 2, 3, 1000, 1461} {
		src := tcpip.AddrFrom4([4]byte{192, 168, 64, 2})
		dst := tcpip.AddrFrom4([4]byte{1, 2, 3, 4})
		seg := make([]byte, header.TCPMinimumSize+payloadLen)
		for i := header.TCPMinimumSize; i < len(seg); i++ {
			seg[i] = byte(i * 7)
		}
		tcp := header.TCP(seg)
		tcp.Encode(&header.TCPFields{
			SrcPort:    1234,
			DstPort:    443,
			SeqNum:     1,
			DataOffset: header.TCPMinimumSize,
			Flags:      header.TCPFlagAck,
			WindowSize: 65535,
		})
		pseudo := header.PseudoHeaderChecksum(header.TCPProtocolNumber, src, dst, uint16(len(seg)))

		// What a NIC doing checksum offload is handed: the folded
		// pseudo-header sum, not complemented.
		tcp.SetChecksum(pseudo)
		completeChecksum(seg, header.TCPChecksumOffset)
		got := tcp.Checksum()

		tcp.SetChecksum(0)
		want := ^checksum.Checksum(seg, pseudo)
		if got != want {
			t.Errorf("payload %d: checksum = %#04x; want %#04x", payloadLen, got, want)
		}
	}
}

// openTAP creates a TAP named name the way a VMM does, returning the VMM's
// end of it. The TAP goes away when the returned file is closed.
func openTAP(t *testing.T, name string) *os.File {
	t.Helper()
	fd, err := unix.Open("/dev/net/tun", unix.O_RDWR|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	ifr, err := unix.NewIfreq(name)
	if err != nil {
		t.Fatal(err)
	}
	ifr.SetUint16(unix.IFF_TAP | unix.IFF_NO_PI | unix.IFF_VNET_HDR)
	if err := unix.IoctlIfreq(fd, unix.TUNSETIFF, ifr); err != nil {
		unix.Close(fd)
		t.Fatalf("TUNSETIFF: %v", err)
	}
	f := os.NewFile(uintptr(fd), "vmm:"+name)
	t.Cleanup(func() { f.Close() })
	return f
}

// TestTAPLink runs a TAPLink on a real TAP, playing the VMM with its own file
// descriptor for the TAP. It needs CAP_NET_ADMIN and CAP_NET_RAW, so it's
// skipped unless run as root.
func TestTAPLink(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root (CAP_NET_ADMIN, CAP_NET_RAW)")
	}
	name := fmt.Sprintf("gntest%d", os.Getpid()%100000)
	vmm := openTAP(t, name)
	link, err := netlink.LinkByName(name)
	if err != nil {
		t.Fatal(err)
	}
	if err := netlink.LinkSetUp(link); err != nil {
		t.Fatal(err)
	}
	tl, err := NewTAPLink(name)
	if err != nil {
		t.Fatal(err)
	}
	defer tl.Close()

	// The host's stack is kept off the TAP by an ingress drop-all filter.
	filters, err := netlink.FilterList(link, netlink.HANDLE_MIN_INGRESS)
	if err != nil {
		t.Fatal(err)
	}
	if len(filters) != 1 {
		t.Fatalf("got %d ingress filters, want 1: %+v", len(filters), filters)
	}
	if ma, ok := filters[0].(*netlink.MatchAll); !ok || len(ma.Actions) != 1 || ma.Actions[0].Attrs().Action != netlink.TC_ACT_SHOT {
		t.Fatalf("ingress filter = %+v, want a match-all drop", filters[0])
	}

	// frame returns an Ethernet frame with an experimental EtherType, which
	// nothing else on the TAP sends.
	frame := func(payload string) []byte {
		f := append([]byte{
			0x02, 0x76, 0x7a, 0x00, 0x00, 0x01, // dst: gateway
			0x02, 0x76, 0x7a, 0x00, 0x00, 0x02, // src: guest
			0x88, 0xb5, // local experimental EtherType
		}, payload...)
		return f
	}

	// Guest to host: the VMM writes a frame (with its virtio header), and
	// ReadFrame returns just the frame.
	fromGuest := frame("from the guest")
	if _, err := vmm.Write(append(make([]byte, virtioNetHdrLen), fromGuest...)); err != nil {
		t.Fatal(err)
	}
	tl.f.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 65536+14)
	for {
		n, err := tl.ReadFrame(buf)
		if err != nil {
			t.Fatalf("ReadFrame: %v", err)
		}
		if bytes.Equal(buf[:n], fromGuest) {
			break
		}
	}

	// Host to guest: WriteFrame's frame reaches the VMM after a virtio
	// header. Skip anything else the host sends before it.
	toGuest := frame("to the guest")
	if err := tl.WriteFrame(toGuest); err != nil {
		t.Fatal(err)
	}
	vmm.SetReadDeadline(time.Now().Add(5 * time.Second))
	for {
		n, err := vmm.Read(buf)
		if err != nil {
			t.Fatalf("VMM read: %v", err)
		}
		if n > virtioNetHdrLen && bytes.Equal(buf[virtioNetHdrLen:n], toGuest) {
			break
		}
	}
}
