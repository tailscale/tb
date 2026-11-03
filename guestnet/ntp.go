// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package guestnet

import (
	"encoding/binary"
	"net"
	"time"
)

// ntpEpochOffset is the number of seconds between the NTP epoch (1900) and
// the Unix epoch (1970).
const ntpEpochOffset = 2208988800

// ntpIdleTimeout ends an NTP session after this long without a request.
const ntpIdleTimeout = 30 * time.Second

// serveNTP answers SNTP requests on one guest UDP session with the host's
// time. A guest restored from a memory snapshot has a clock that is stale by
// the snapshot's age; macOS's timed corrects it on its next exchange with
// what it believes is time.apple.com, which this intercepts.
func (n *Net) serveNTP(conn net.Conn) error {
	buf := make([]byte, 128)
	for {
		conn.SetReadDeadline(time.Now().Add(ntpIdleTimeout))
		m, err := conn.Read(buf)
		if err != nil {
			return nil
		}
		if m < 48 {
			continue
		}
		resp := ntpResponse(buf[:48], n.cfg.Now())
		if _, err := conn.Write(resp); err != nil {
			return err
		}
	}
}

// ntpResponse builds a 48-byte server reply to req with all server
// timestamps set to now.
func ntpResponse(req []byte, now time.Time) []byte {
	resp := make([]byte, 48)
	vn := (req[0] >> 3) & 0x07
	if vn < 3 || vn > 4 {
		vn = 4
	}
	resp[0] = vn<<3 | 4 // LI=0, mode 4 (server)
	resp[1] = 2         // stratum
	resp[2] = req[2]    // poll, echoed
	resp[3] = 0xEC      // precision: about 2^-20 seconds
	// Root delay and dispersion: zero.
	copy(resp[12:16], "LOCL")
	ts := ntpTimestamp(now)
	binary.BigEndian.PutUint64(resp[16:], ts) // reference
	copy(resp[24:32], req[40:48])             // originate = client transmit
	binary.BigEndian.PutUint64(resp[32:], ts) // receive
	binary.BigEndian.PutUint64(resp[40:], ts) // transmit
	return resp
}

func ntpTimestamp(t time.Time) uint64 {
	secs := uint64(t.Unix()) + ntpEpochOffset
	frac := uint64(t.Nanosecond()) * (1 << 32) / 1e9
	return secs<<32 | frac
}
