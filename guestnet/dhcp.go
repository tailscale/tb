// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package guestnet

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"net"
	"time"

	"github.com/insomniacslk/dhcp/dhcpv4"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/checksum"
	"gvisor.dev/gvisor/pkg/tcpip/header"
)

// handleDHCP answers DHCPv4 requests found in frame. It reports whether the
// frame was a DHCP request (and so must not be injected into the stack).
//
// DHCP is answered outside gVisor because the client has no address yet and
// the reply rules (unicast to the client MAC vs. broadcast) are about the
// link layer, which is simplest to control when we build the frame ourselves.
func (n *Net) handleDHCP(frame []byte) bool {
	ip := header.IPv4(frame[header.EthernetMinimumSize:])
	if len(ip) < header.IPv4MinimumSize || !ip.IsValid(len(ip)) || ip.Protocol() != uint8(header.UDPProtocolNumber) {
		return false
	}
	if ip.More() || ip.FragmentOffset() != 0 {
		return false
	}
	udpPkt := header.UDP(ip.Payload())
	if len(udpPkt) < header.UDPMinimumSize || udpPkt.DestinationPort() != dhcpv4.ServerPort {
		return false
	}
	req, err := dhcpv4.FromBytes(udpPkt.Payload())
	if err != nil {
		n.cfg.Logf("guestnet: dhcp: bad request: %v", err)
		return true
	}
	if req.OpCode != dhcpv4.OpcodeBootRequest {
		return true
	}

	var replyType dhcpv4.MessageType
	switch req.MessageType() {
	case dhcpv4.MessageTypeDiscover:
		replyType = dhcpv4.MessageTypeOffer
		n.Events.Add(Event{Time: n.cfg.Now(), Kind: "dhcp-discover", Msg: req.ClientHWAddr.String()})
	case dhcpv4.MessageTypeRequest:
		// A REQUEST for some other address (a lease carried over from
		// elsewhere) gets a NAK so the client starts over and takes ours.
		if want := req.RequestedIPAddress(); want != nil && !want.IsUnspecified() && !want.Equal(n.cfg.GuestIP.AsSlice()) {
			replyType = dhcpv4.MessageTypeNak
		} else if !req.ClientIPAddr.IsUnspecified() && !req.ClientIPAddr.Equal(n.cfg.GuestIP.AsSlice()) {
			replyType = dhcpv4.MessageTypeNak
		} else {
			replyType = dhcpv4.MessageTypeAck
		}
	case dhcpv4.MessageTypeInform:
		replyType = dhcpv4.MessageTypeAck
	case dhcpv4.MessageTypeDecline:
		n.cfg.Logf("guestnet: dhcp: guest declined %v", n.cfg.GuestIP)
		return true
	default:
		return true
	}

	resp, err := n.buildDHCPReply(req, replyType)
	if err != nil {
		n.cfg.Logf("guestnet: dhcp: build %v: %v", replyType, err)
		return true
	}

	// Where to send it (RFC 2131 section 4.1). A NAK is always broadcast.
	// A client that already has an address (renewing) is answered unicast.
	// Otherwise honor the broadcast flag; a client without the flag is
	// answered unicast to its MAC with its new address as the IP destination.
	dstMAC := net.HardwareAddr(req.ClientHWAddr)
	dstIP := n.cfg.GuestIP
	switch {
	case replyType == dhcpv4.MessageTypeNak, req.IsBroadcast() && req.ClientIPAddr.IsUnspecified():
		dstMAC = net.HardwareAddr{0xff, 0xff, 0xff, 0xff, 0xff, 0xff}
		dstIP = netipBroadcast
	case !req.ClientIPAddr.IsUnspecified():
		if a, ok := netipFromIP(req.ClientIPAddr); ok {
			dstIP = a
		}
	}

	srcIP := n.cfg.GatewayIP
	if replyType == dhcpv4.MessageTypeAck || replyType == dhcpv4.MessageTypeOffer {
		if replyType == dhcpv4.MessageTypeAck && req.MessageType() == dhcpv4.MessageTypeRequest {
			n.dhcpLeasedOnce.Do(func() { close(n.dhcpLeased) })
			n.Events.Add(Event{Time: n.cfg.Now(), Kind: "dhcp-lease", Msg: fmt.Sprintf("%v -> %v", req.ClientHWAddr, n.cfg.GuestIP)})
		}
	}
	n.writeFrame(buildUDPFrame(n.cfg.GatewayMAC, dstMAC, srcIP.As4(), dstIP.As4(), dhcpv4.ServerPort, dhcpv4.ClientPort, resp.ToBytes()))
	return true
}

func (n *Net) buildDHCPReply(req *dhcpv4.DHCPv4, mt dhcpv4.MessageType) (*dhcpv4.DHCPv4, error) {
	gw := net.IP(n.cfg.GatewayIP.AsSlice())
	mods := []dhcpv4.Modifier{
		dhcpv4.WithMessageType(mt),
		dhcpv4.WithServerIP(gw),
		dhcpv4.WithOption(dhcpv4.OptServerIdentifier(gw)),
	}
	if mt != dhcpv4.MessageTypeNak {
		var mtu [2]byte
		binary.BigEndian.PutUint16(mtu[:], uint16(n.cfg.MTU))
		mods = append(mods,
			dhcpv4.WithYourIP(n.cfg.GuestIP.AsSlice()),
			dhcpv4.WithOption(dhcpv4.OptSubnetMask(net.CIDRMask(n.cfg.Subnet.Bits(), 32))),
			dhcpv4.WithOption(dhcpv4.OptRouter(gw)),
			dhcpv4.WithOption(dhcpv4.OptDNS(net.IP(DNSAddr.AsSlice()))),
			dhcpv4.WithOption(dhcpv4.OptBroadcastAddress(broadcastOf(n.cfg.Subnet))),
			dhcpv4.WithOption(dhcpv4.OptIPAddressLeaseTime(n.cfg.LeaseTime)),
			dhcpv4.WithOption(dhcpv4.OptGeneric(dhcpv4.OptionRenewTimeValue, secs(n.cfg.LeaseTime/2))),
			dhcpv4.WithOption(dhcpv4.OptGeneric(dhcpv4.OptionRebindingTimeValue, secs(n.cfg.LeaseTime*7/8))),
			dhcpv4.WithOption(dhcpv4.OptGeneric(dhcpv4.OptionInterfaceMTU, mtu[:])),
		)
		if mt == dhcpv4.MessageTypeAck && req.MessageType() == dhcpv4.MessageTypeInform {
			// INFORM replies must not carry a lease.
			mods = append(mods, func(d *dhcpv4.DHCPv4) {
				d.YourIPAddr = net.IPv4zero
				d.DeleteOption(dhcpv4.OptionIPAddressLeaseTime)
				d.DeleteOption(dhcpv4.OptionRenewTimeValue)
				d.DeleteOption(dhcpv4.OptionRebindingTimeValue)
			})
		}
	}
	return dhcpv4.NewReplyFromRequest(req, mods...)
}

func secs(d time.Duration) []byte {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], uint32(d/time.Second))
	return b[:]
}

// buildUDPFrame assembles an Ethernet/IPv4/UDP frame with checksums.
func buildUDPFrame(srcMAC, dstMAC net.HardwareAddr, srcIP, dstIP [4]byte, srcPort, dstPort uint16, payload []byte) []byte {
	udpLen := header.UDPMinimumSize + len(payload)
	ipLen := header.IPv4MinimumSize + udpLen
	frame := make([]byte, header.EthernetMinimumSize+ipLen)

	eth := header.Ethernet(frame)
	eth.Encode(&header.EthernetFields{
		SrcAddr: tcpip.LinkAddress(srcMAC),
		DstAddr: tcpip.LinkAddress(dstMAC),
		Type:    header.IPv4ProtocolNumber,
	})

	ip := header.IPv4(frame[header.EthernetMinimumSize:])
	ip.Encode(&header.IPv4Fields{
		TotalLength: uint16(ipLen),
		TTL:         64,
		Protocol:    uint8(header.UDPProtocolNumber),
		SrcAddr:     tcpip.AddrFrom4(srcIP),
		DstAddr:     tcpip.AddrFrom4(dstIP),
	})
	ip.SetChecksum(^ip.CalculateChecksum())

	u := header.UDP(ip.Payload())
	u.Encode(&header.UDPFields{
		SrcPort: srcPort,
		DstPort: dstPort,
		Length:  uint16(udpLen),
	})
	copy(u.Payload(), payload)
	xsum := header.PseudoHeaderChecksum(header.UDPProtocolNumber, tcpip.AddrFrom4(srcIP), tcpip.AddrFrom4(dstIP), uint16(udpLen))
	xsum = checksum.Checksum(payload, xsum)
	u.SetChecksum(^u.CalculateChecksum(xsum))
	return frame
}

// parseUDPFrame extracts the addressing and payload of an IPv4 UDP frame.
// It is used by tests and by the DHCP path's counterpart in a fake guest.
func parseUDPFrame(frame []byte) (src, dst [4]byte, srcPort, dstPort uint16, payload []byte, ok bool) {
	if len(frame) < header.EthernetMinimumSize+header.IPv4MinimumSize+header.UDPMinimumSize {
		return
	}
	eth := header.Ethernet(frame)
	if eth.Type() != header.IPv4ProtocolNumber {
		return
	}
	ip := header.IPv4(frame[header.EthernetMinimumSize:])
	if !ip.IsValid(len(ip)) || ip.Protocol() != uint8(header.UDPProtocolNumber) {
		return
	}
	u := header.UDP(ip.Payload())
	if len(u) < header.UDPMinimumSize {
		return
	}
	return ip.SourceAddress().As4(), ip.DestinationAddress().As4(), u.SourcePort(), u.DestinationPort(), bytes.Clone(u.Payload()), true
}
