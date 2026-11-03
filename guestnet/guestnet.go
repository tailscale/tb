// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

// Package guestnet is a private userspace network for one guest VM.
//
// The guest's virtual NIC hands us raw Ethernet frames (on macOS via a
// VZFileHandleNetworkDeviceAttachment over a SOCK_DGRAM socketpair; in tests
// via an in-memory link). Every frame terminates in a gVisor netstack owned by
// this package, so a guest can only ever reach what this package chooses to
// connect it to:
//
//   - The gateway, IMDS and DNS addresses live on our NIC, so ARP and ICMP
//     echo for them are answered by gVisor itself.
//   - DHCP is answered directly from the frame loop with a fixed lease, so a
//     memory snapshot restored in another slot sees the same addresses.
//   - Every TCP SYN and UDP flow is dispatched by destination: a registered
//     [Service] (IMDS, the caller's NFS or proxies), an intercepted protocol
//     (DNS on any port 53, NTP on any port 123), or egress dialed from the
//     host. Each flow is logged.
//   - The host reaches the guest only by dialing into this stack with
//     [Net.DialGuest]; there is no host interface routed to the guest.
//
// One [Net] per VM. Nothing here is process-global, so many guests can share
// a process without seeing each other.
package guestnet

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sync"
	"time"

	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/link/channel"
	"gvisor.dev/gvisor/pkg/tcpip/link/ethernet"
	"gvisor.dev/gvisor/pkg/tcpip/network/arp"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/icmp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"
	"tailscale.com/types/logger"
)

// Well-known addresses. They are the same for every guest because each guest
// has its own L2 domain; that is what makes memory snapshots slot-independent.
var (
	// DefaultSubnet is the guest-visible subnet.
	DefaultSubnet = netip.MustParsePrefix("192.168.64.0/24")
	// DefaultGatewayIP is our address on the guest subnet and the guest's
	// default router.
	DefaultGatewayIP = netip.MustParseAddr("192.168.64.1")
	// DefaultGuestIP is the one address the DHCP server hands out.
	DefaultGuestIP = netip.MustParseAddr("192.168.64.2")
	// IMDSAddr is the metadata service address, matching cloud convention and
	// the address corp CI already uses.
	IMDSAddr = netip.MustParseAddr("169.254.169.254")
	// DNSAddr is the resolver address handed out by DHCP, matching corp CI.
	DNSAddr = netip.MustParseAddr("169.254.169.253")

	// DefaultGatewayMAC is our MAC. It is constant so a restored guest's ARP
	// cache stays valid.
	DefaultGatewayMAC = net.HardwareAddr{0x02, 0x76, 0x7a, 0x00, 0x00, 0x01}
	// DefaultGuestMAC is the guest MAC used when an image doesn't specify one.
	DefaultGuestMAC = net.HardwareAddr{0x02, 0x76, 0x7a, 0x00, 0x00, 0x02}
)

// DefaultDeny is the egress deny list applied when [Config.Deny] is nil: the
// tailnet CGNAT range and its IPv6 counterpart (so a guest can't reach the
// host's tailnet), loopback, and the link-local range (IMDS and DNS are
// reached through the service table before policy applies).
var DefaultDeny = []netip.Prefix{
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("fd7a:115c:a1e0::/48"),
	netip.MustParsePrefix("127.0.0.0/8"),
	netip.MustParsePrefix("169.254.0.0/16"),
	netip.MustParsePrefix("0.0.0.0/8"),
}

const nicID tcpip.NICID = 1

// Dialer dials egress connections on behalf of the guest. *net.Dialer
// satisfies it.
type Dialer interface {
	DialContext(ctx context.Context, network, address string) (net.Conn, error)
}

// Config configures a guest network. The zero value is usable with a link.
type Config struct {
	// Link carries raw Ethernet frames to and from the guest. Required.
	Link FrameLink

	// GuestMAC is the guest's MAC address, used as the DHCP client identity
	// check and as the destination of DHCP replies. Defaults to
	// [DefaultGuestMAC].
	GuestMAC net.HardwareAddr
	// GatewayMAC is our MAC. Defaults to [DefaultGatewayMAC].
	GatewayMAC net.HardwareAddr

	// Subnet, GatewayIP and GuestIP default to the Default* values.
	Subnet    netip.Prefix
	GatewayIP netip.Addr
	GuestIP   netip.Addr

	// MTU is the link MTU advertised to the guest via DHCP. Defaults to 1500.
	MTU uint32

	// LeaseTime is the DHCP lease duration. Defaults to 24h. Restored
	// snapshots renew against this server, so long leases are fine.
	LeaseTime time.Duration

	// Dialer dials egress TCP and UDP. Defaults to a net.Dialer.
	Dialer Dialer

	// Resolver answers the guest's DNS queries. Defaults to
	// [NewUpstreamResolver] with [DefaultUpstreams].
	Resolver Resolver

	// Deny lists destinations the guest may not reach via egress. nil means
	// [DefaultDeny]; an empty non-nil slice denies nothing.
	Deny []netip.Prefix

	// Services maps guest-visible destinations to handlers. The destination
	// address must be one of ours (gateway, IMDS, DNS) or the lookup happens
	// before egress policy and would let a guest hijack a public address.
	Services map[netip.AddrPort]Service

	// Now returns the current time, for NTP and logs. Defaults to time.Now.
	Now func() time.Time

	// Logf logs. Defaults to logger.Discard.
	Logf logger.Logf
}

func (c *Config) withDefaults() {
	if c.GuestMAC == nil {
		c.GuestMAC = DefaultGuestMAC
	}
	if c.GatewayMAC == nil {
		c.GatewayMAC = DefaultGatewayMAC
	}
	if !c.Subnet.IsValid() {
		c.Subnet = DefaultSubnet
	}
	if !c.GatewayIP.IsValid() {
		c.GatewayIP = DefaultGatewayIP
	}
	if !c.GuestIP.IsValid() {
		c.GuestIP = DefaultGuestIP
	}
	if c.MTU == 0 {
		c.MTU = 1500
	}
	if c.LeaseTime == 0 {
		c.LeaseTime = 24 * time.Hour
	}
	if c.Dialer == nil {
		c.Dialer = &net.Dialer{Timeout: 10 * time.Second}
	}
	if c.Resolver == nil {
		c.Resolver = NewUpstreamResolver(DefaultUpstreams...)
	}
	if c.Deny == nil {
		c.Deny = DefaultDeny
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	if c.Logf == nil {
		c.Logf = logger.Discard
	}
}

// Net is one guest's network. Create it with [New] and shut it down with
// [Net.Close].
type Net struct {
	cfg    Config
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	stack  *stack.Stack
	linkEP *channel.Endpoint

	// Flows is the TCP/UDP flow log.
	Flows *Log[Flow]
	// DNS is the DNS query log.
	DNS *Log[DNSEvent]
	// Events records notable network events (DHCP lease, blocked flows).
	Events *Log[Event]

	// dhcpLeased is closed the first time the guest acknowledges a lease.
	dhcpLeased     chan struct{}
	dhcpLeasedOnce sync.Once

	mu       sync.Mutex
	services map[netip.AddrPort]Service
}

// New starts a guest network on cfg.Link. It returns once the stack is up;
// frames are pumped on background goroutines until [Net.Close].
func New(cfg Config) (*Net, error) {
	if cfg.Link == nil {
		return nil, errors.New("guestnet: Config.Link is required")
	}
	cfg.withDefaults()
	if !cfg.Subnet.Contains(cfg.GatewayIP) || !cfg.Subnet.Contains(cfg.GuestIP) {
		return nil, fmt.Errorf("guestnet: gateway %v and guest %v must be within %v", cfg.GatewayIP, cfg.GuestIP, cfg.Subnet)
	}
	if !cfg.GatewayIP.Is4() {
		return nil, errors.New("guestnet: only IPv4 is supported")
	}

	ctx, cancel := context.WithCancel(context.Background())
	n := &Net{
		cfg:        cfg,
		ctx:        ctx,
		cancel:     cancel,
		Flows:      NewLog[Flow](1024),
		DNS:        NewLog[DNSEvent](1024),
		Events:     NewLog[Event](256),
		dhcpLeased: make(chan struct{}),
		services:   make(map[netip.AddrPort]Service),
	}
	for ap, svc := range cfg.Services {
		n.services[ap] = svc
	}

	s := stack.New(stack.Options{
		NetworkProtocols:   []stack.NetworkProtocolFactory{ipv4.NewProtocol, arp.NewProtocol},
		TransportProtocols: []stack.TransportProtocolFactory{tcp.NewProtocol, udp.NewProtocol, icmp.NewProtocol4},
	})
	sack := tcpip.TCPSACKEnabled(true)
	if err := s.SetTransportProtocolOption(tcp.ProtocolNumber, &sack); err != nil {
		cancel()
		return nil, fmt.Errorf("guestnet: enable SACK: %v", err)
	}

	// The channel endpoint carries whole Ethernet frames; the ethernet wrapper
	// adds and strips link headers so gVisor handles ARP for us.
	n.linkEP = channel.New(4096, cfg.MTU, tcpip.LinkAddress(cfg.GatewayMAC))
	if err := s.CreateNIC(nicID, ethernet.New(n.linkEP)); err != nil {
		cancel()
		return nil, fmt.Errorf("guestnet: CreateNIC: %v", err)
	}
	// Accept packets for any destination so the forwarders see every flow,
	// and allow replies from any source address.
	if err := s.SetPromiscuousMode(nicID, true); err != nil {
		cancel()
		return nil, fmt.Errorf("guestnet: SetPromiscuousMode: %v", err)
	}
	if err := s.SetSpoofing(nicID, true); err != nil {
		cancel()
		return nil, fmt.Errorf("guestnet: SetSpoofing: %v", err)
	}

	// Our addresses. gVisor answers ARP and ICMP echo for these. The
	// link-local ones are on the NIC precisely so the guest's on-link ARP for
	// them (169.254/16 is APIPA and never routed) gets an answer.
	addrs := []struct {
		addr netip.Addr
		bits int
	}{
		{cfg.GatewayIP, cfg.Subnet.Bits()},
		{IMDSAddr, 32},
		{DNSAddr, 32},
	}
	for _, a := range addrs {
		pa := tcpip.ProtocolAddress{
			Protocol: ipv4.ProtocolNumber,
			AddressWithPrefix: tcpip.AddressWithPrefix{
				Address:   tcpip.AddrFrom4(a.addr.As4()),
				PrefixLen: a.bits,
			},
		}
		if err := s.AddProtocolAddress(nicID, pa, stack.AddressProperties{}); err != nil {
			cancel()
			return nil, fmt.Errorf("guestnet: AddProtocolAddress(%v): %v", a.addr, err)
		}
	}
	s.SetRouteTable([]tcpip.Route{{
		Destination: header.IPv4EmptySubnet,
		NIC:         nicID,
	}})

	tcpFwd := tcp.NewForwarder(s, 0, 4096, n.handleTCP)
	s.SetTransportProtocolHandler(tcp.ProtocolNumber, tcpFwd.HandlePacket)
	udpFwd := udp.NewForwarder(s, n.handleUDP)
	s.SetTransportProtocolHandler(udp.ProtocolNumber, udpFwd.HandlePacket)

	n.stack = s
	n.wg.Add(2)
	go n.pumpFromGuest()
	go n.pumpToGuest()
	return n, nil
}

// Close stops the network and closes the link. Open flows are torn down.
func (n *Net) Close() error {
	n.cancel()
	err := n.cfg.Link.Close()
	n.linkEP.Close()
	// Close every endpoint before waiting for the flow goroutines: one may be
	// in a TCP handshake with a guest that will never finish it (the VM
	// stopped just after its SYN), which only aborts when its endpoint is
	// closed, or after gVisor's handshake timeout of 30s or more.
	n.stack.Close()
	n.wg.Wait()
	n.stack.Destroy()
	return err
}

// GuestIP returns the address the guest is leased.
func (n *Net) GuestIP() netip.Addr { return n.cfg.GuestIP }

// GatewayIP returns our address on the guest subnet.
func (n *Net) GatewayIP() netip.Addr { return n.cfg.GatewayIP }

// Leased returns a channel that is closed once the guest has acknowledged a
// DHCP lease. For a restored snapshot this may not fire until the guest
// renews, so callers should also consider the guest reachable if
// [Net.DialGuest] succeeds.
func (n *Net) Leased() <-chan struct{} { return n.dhcpLeased }

// SetService registers or replaces the handler for a guest-visible
// destination. Passing a nil svc removes it.
func (n *Net) SetService(dst netip.AddrPort, svc Service) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if svc == nil {
		delete(n.services, dst)
	} else {
		n.services[dst] = svc
	}
}

func (n *Net) service(dst netip.AddrPort) (Service, bool) {
	n.mu.Lock()
	defer n.mu.Unlock()
	svc, ok := n.services[dst]
	return svc, ok
}

// DialGuest opens a TCP connection from the gateway address to port on the
// guest. It is how the host reaches sshd in the guest.
func (n *Net) DialGuest(ctx context.Context, port uint16) (net.Conn, error) {
	local := tcpip.FullAddress{NIC: nicID, Addr: tcpip.AddrFrom4(n.cfg.GatewayIP.As4())}
	remote := tcpip.FullAddress{NIC: nicID, Addr: tcpip.AddrFrom4(n.cfg.GuestIP.As4()), Port: port}
	return gonet.DialTCPWithBind(ctx, n.stack, local, remote, ipv4.ProtocolNumber)
}

// pumpFromGuest reads frames from the link, answers DHCP directly, and injects
// everything else into gVisor.
func (n *Net) pumpFromGuest() {
	defer n.wg.Done()
	buf := make([]byte, 65536+header.EthernetMinimumSize)
	for {
		m, err := n.cfg.Link.ReadFrame(buf)
		if err != nil {
			if n.ctx.Err() == nil {
				n.cfg.Logf("guestnet: link read: %v", err)
				n.cancel()
			}
			return
		}
		frame := buf[:m]
		if len(frame) < header.EthernetMinimumSize {
			continue
		}
		eth := header.Ethernet(frame)
		switch eth.Type() {
		case header.IPv4ProtocolNumber:
			if n.handleDHCP(frame) {
				continue
			}
		case header.ARPProtocolNumber:
			if !n.arpForUs(frame[header.EthernetMinimumSize:]) {
				continue
			}
		default:
			// IPv6 and everything else is dropped for now.
			continue
		}
		pkt := stack.NewPacketBuffer(stack.PacketBufferOptions{
			Payload: buffer.MakeWithData(append([]byte(nil), frame...)),
		})
		// The ethernet wrapper parses the real EtherType from the frame; the
		// protocol argument is ignored.
		n.linkEP.InjectInbound(eth.Type(), pkt)
		pkt.DecRef()
	}
}

// arpForUs reports whether an ARP packet from the guest should reach gVisor:
// replies (to our requests for the guest's address), and requests for one of
// our own addresses. Our NIC is promiscuous, so gVisor would otherwise answer
// requests for every address, including the guest's own; the guest's DHCP
// client then sees its duplicate address probe answered and declines the
// lease.
func (n *Net) arpForUs(b []byte) bool {
	arp := header.ARP(b)
	if !arp.IsValid() {
		return false
	}
	if arp.Op() != header.ARPRequest {
		return true
	}
	target := netip.AddrFrom4([4]byte(arp.ProtocolAddressTarget()))
	return target == n.cfg.GatewayIP || target == IMDSAddr || target == DNSAddr
}

// pumpToGuest writes frames produced by gVisor to the link.
func (n *Net) pumpToGuest() {
	defer n.wg.Done()
	for {
		pkt := n.linkEP.ReadContext(n.ctx)
		if pkt == nil {
			return
		}
		v := pkt.ToView()
		err := n.cfg.Link.WriteFrame(v.AsSlice())
		v.Release()
		pkt.DecRef()
		if err != nil && n.ctx.Err() == nil {
			n.cfg.Logf("guestnet: link write: %v", err)
		}
	}
}

// writeFrame sends a frame we built ourselves (DHCP replies) to the guest.
func (n *Net) writeFrame(frame []byte) {
	if err := n.cfg.Link.WriteFrame(frame); err != nil && n.ctx.Err() == nil {
		n.cfg.Logf("guestnet: link write: %v", err)
	}
}

// egressAllowed reports whether policy lets the guest reach dst.
func (n *Net) egressAllowed(dst netip.Addr) bool {
	if dst.IsMulticast() || dst.IsUnspecified() || dst == netip.AddrFrom4([4]byte{255, 255, 255, 255}) {
		return false
	}
	if n.cfg.Subnet.Contains(dst) {
		// Only the gateway (via services) and the guest itself exist here.
		return false
	}
	for _, p := range n.cfg.Deny {
		if p.Contains(dst) {
			return false
		}
	}
	return true
}

func addrFromTCPIP(a tcpip.Address) netip.Addr {
	switch a.Len() {
	case 4:
		return netip.AddrFrom4(a.As4())
	case 16:
		return netip.AddrFrom16(a.As16())
	}
	return netip.Addr{}
}
