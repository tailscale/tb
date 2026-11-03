// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

//go:build darwin && arm64

package main

import (
	"errors"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/Code-Hex/vz/v3"
)

// cmdEntitlementProbe checks that this binary may use Virtualization.framework
// (that is, it carries the com.apple.security.virtualization entitlement),
// without needing an image: it validates, creates and starts a minimal EFI
// VM with no devices, then stops it. It is for checking that a binary signed
// on one Mac works on another.
func cmdEntitlementProbe(args []string) error {
	if len(args) != 0 {
		return errors.New("usage: entitlementprobe")
	}
	dir, err := os.MkdirTemp("", "mantsana-entitlementprobe")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)

	store, err := vz.NewEFIVariableStore(filepath.Join(dir, "efivars"), vz.WithCreatingEFIVariableStore())
	if err != nil {
		return err
	}
	bl, err := vz.NewEFIBootLoader(vz.WithEFIVariableStore(store))
	if err != nil {
		return err
	}
	vc, err := vz.NewVirtualMachineConfiguration(bl, 1, 512<<20)
	if err != nil {
		return err
	}
	platform, err := vz.NewGenericPlatformConfiguration()
	if err != nil {
		return err
	}
	vc.SetPlatformVirtualMachineConfiguration(platform)
	if ok, err := vc.Validate(); !ok || err != nil {
		return orErr(err, "invalid configuration")
	}
	log.Printf("configuration valid")
	vm, err := vz.NewVirtualMachine(vc)
	if err != nil {
		return err
	}
	if err := vm.Start(); err != nil {
		return err
	}
	log.Printf("VM started (state %v)", vm.State())
	time.Sleep(time.Second)
	// With no boot device the firmware may already have stopped it.
	if vm.CanStop() {
		if err := vm.Stop(); err != nil {
			return err
		}
	}
	log.Printf("VM stopped (state %v); entitlement OK", vm.State())
	return nil
}

func orErr(err error, msg string) error {
	if err != nil {
		return err
	}
	return errors.New(msg)
}
