// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

//go:build darwin && arm64

package main

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/tailscale/tb/guestnet"
	"github.com/tailscale/tb/mantsana/guest"
	"golang.org/x/crypto/ssh"
)

// guestSSH connects to the guest's sshd through its network, as corp's
// ciguestlet does (user admin, password admin; the CI images' convention).
func guestSSH(ctx context.Context, v *guest.VM) (*ssh.Client, error) {
	c, err := v.Net().DialGuest(ctx, 22)
	if err != nil {
		return nil, err
	}
	addr := guestnet.DefaultGuestIP.String() + ":22"
	cc, chans, reqs, err := ssh.NewClientConn(c, addr, &ssh.ClientConfig{
		User:            "admin",
		Auth:            []ssh.AuthMethod{ssh.Password("admin")},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         10 * time.Second,
	})
	if err != nil {
		c.Close()
		return nil, err
	}
	return ssh.NewClient(cc, chans, reqs), nil
}

// sshOutput runs cmd on the guest and returns its combined output.
func sshOutput(sc *ssh.Client, cmd string) (string, error) {
	sess, err := sc.NewSession()
	if err != nil {
		return "", err
	}
	defer sess.Close()
	out, err := sess.CombinedOutput(cmd)
	return strings.TrimSpace(string(out)), err
}

// guestClockSkew returns how far the guest's clock is behind the host's.
func guestClockSkew(sc *ssh.Client) (time.Duration, error) {
	// Nanoseconds; macOS date(1) has no %N, so use perl, which the images
	// have.
	before := time.Now()
	out, err := sshOutput(sc, `perl -MTime::HiRes=time -e 'printf "%.6f\n", time'`)
	if err != nil {
		return 0, fmt.Errorf("reading guest time: %v: %s", err, out)
	}
	rtt := time.Since(before)
	secs, err := strconv.ParseFloat(out, 64)
	if err != nil {
		return 0, fmt.Errorf("parsing guest time %q: %v", out, err)
	}
	guestNow := time.Unix(0, int64(secs*1e9))
	hostNow := before.Add(rtt / 2)
	return hostNow.Sub(guestNow), nil
}

// checkGuestClock reports the guest's clock skew, and if the guest was
// restored, fixes it with sntp against the gateway (guestnet answers NTP
// with host time) and reports it again.
func checkGuestClock(ctx context.Context, v *guest.VM) error {
	sc, err := guestSSH(ctx, v)
	if err != nil {
		return fmt.Errorf("ssh: %w", err)
	}
	defer sc.Close()
	skew, err := guestClockSkew(sc)
	if err != nil {
		return err
	}
	log.Printf("guest clock is %v behind the host", skew.Round(time.Millisecond))
	if !v.Restored() {
		return nil
	}
	t0 := time.Now()
	out, err := sshOutput(sc, "sudo -n sntp -sS "+guestnet.DefaultGatewayIP.String())
	log.Printf("sntp -sS (%v): err=%v %s", time.Since(t0).Round(time.Millisecond), err, out)
	if skew, err = guestClockSkew(sc); err != nil {
		return err
	}
	log.Printf("after sntp, guest clock is %v behind the host", skew.Round(time.Millisecond))
	return nil
}
