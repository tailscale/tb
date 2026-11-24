// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

//go:build unix

// The lanlord command dials LAN TCP connections for local processes that
// may not, and hands them the connected sockets. Run it as root; see the
// lanlord package. It dials only IPv4 addresses.
package main

import (
	"flag"
	"fmt"
	"log"
	"net"
	"net/netip"
	"slices"
	"strconv"
	"strings"

	"github.com/tailscale/tb/lanlord"
	"tailscale.com/util/set"
)

var (
	socket        = flag.String("socket", lanlord.DefaultSocket, "unix socket path to listen on")
	allowUsers    = flag.String("allow-users", "", "comma-separated usernames or uids whose processes may request connections; users missing on this host are ignored; empty means any local user")
	allowPorts    = flag.String("allow-ports", "", "comma-separated TCP ports that may be dialed (required)")
	allowPrefixes = flag.String("allow-prefixes", "", "comma-separated IPv4 prefixes whose addresses may be dialed; empty means the private (RFC 1918) prefixes of this host's interfaces, checked per request")
)

func main() {
	flag.Parse()
	users := set.Of(splitList(*allowUsers)...)
	pol, err := parsePolicy(*allowPorts, *allowPrefixes)
	if err != nil {
		log.Fatal(err)
	}
	ln, err := lanlord.Listen(*socket)
	if err != nil {
		log.Fatal(err)
	}
	who := "any local user"
	if len(users) > 0 {
		names := users.Slice()
		slices.Sort(names)
		who = "users " + strings.Join(names, ",")
	}
	log.Printf("lanlord: listening on %s for %s, to %v", *socket, who, pol)
	s := &lanlord.Server{
		AllowUsers: users,
		Allow:      pol.allow,
		Logf:       log.Printf,
	}
	log.Fatal(s.Serve(ln))
}

// policy is the set of destinations that may be dialed.
type policy struct {
	ports []uint16

	// prefixes are the allowed IPv4 prefixes. If nil, the private prefixes
	// of this host's interfaces are allowed, read at each request so that an
	// interface that gets its address after boot counts.
	prefixes []netip.Prefix

	// interfacePrefixes, if non-nil, replaces localPrivatePrefixes in tests.
	interfacePrefixes func() ([]netip.Prefix, error)
}

// parsePolicy parses the --allow-ports and --allow-prefixes flag values.
func parsePolicy(ports, prefixes string) (*policy, error) {
	p := new(policy)
	for _, f := range splitList(ports) {
		n, err := strconv.ParseUint(f, 10, 16)
		if err != nil {
			return nil, fmt.Errorf("--allow-ports: %v", err)
		}
		p.ports = append(p.ports, uint16(n))
	}
	if len(p.ports) == 0 {
		return nil, fmt.Errorf("--allow-ports is required")
	}
	for _, f := range splitList(prefixes) {
		pfx, err := netip.ParsePrefix(f)
		if err != nil || !pfx.Addr().Is4() {
			return nil, fmt.Errorf("--allow-prefixes: %q is not an IPv4 prefix", f)
		}
		p.prefixes = append(p.prefixes, pfx.Masked())
	}
	return p, nil
}

func (p *policy) String() string {
	nets := "this host's private interface prefixes"
	if p.prefixes != nil {
		nets = fmt.Sprint(p.prefixes)
	}
	return fmt.Sprintf("ports %v on %s", p.ports, nets)
}

// allow reports whether ap may be dialed.
func (p *policy) allow(ap netip.AddrPort) bool {
	if !ap.Addr().Is4() || !slices.Contains(p.ports, ap.Port()) {
		return false
	}
	prefixes := p.prefixes
	if prefixes == nil {
		f := p.interfacePrefixes
		if f == nil {
			f = localPrivatePrefixes
		}
		var err error
		if prefixes, err = f(); err != nil {
			log.Printf("lanlord: reading interface addresses: %v", err)
			return false
		}
	}
	return slices.ContainsFunc(prefixes, func(pfx netip.Prefix) bool { return pfx.Contains(ap.Addr()) })
}

// localPrivatePrefixes returns the private IPv4 prefixes of this host's
// interfaces.
func localPrivatePrefixes() ([]netip.Prefix, error) {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil, err
	}
	var out []netip.Prefix
	for _, a := range addrs {
		ipn, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		ip, ok := netip.AddrFromSlice(ipn.IP)
		if !ok {
			continue
		}
		ip = ip.Unmap()
		bits, _ := ipn.Mask.Size()
		if ip.Is4() && ip.IsPrivate() && bits > 0 {
			out = append(out, netip.PrefixFrom(ip, bits).Masked())
		}
	}
	return out, nil
}

// splitList returns the non-empty, space-trimmed fields of a comma-separated
// list.
func splitList(s string) []string {
	var out []string
	for f := range strings.SplitSeq(s, ",") {
		if f = strings.TrimSpace(f); f != "" {
			out = append(out, f)
		}
	}
	return out
}
