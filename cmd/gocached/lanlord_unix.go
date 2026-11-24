// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

//go:build unix

package main

import (
	"github.com/tailscale/tb/lanlord"
	"tailscale.com/net/netx"
)

func init() {
	lanlordDialer = func(socket string) netx.DialFunc {
		return (&lanlord.Client{Socket: socket}).DialContext
	}
}
