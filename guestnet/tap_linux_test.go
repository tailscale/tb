// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package guestnet

import (
	"testing"

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
