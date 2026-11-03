// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

//go:build unix

package guest

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/tailscale/tb/mantsana/image"
)

// WarmConfig configures building a warm snapshot.
type WarmConfig struct {
	// Config is the VM to boot. Boot and Services are ignored.
	Config

	// ReadyPort is the guest TCP port whose reachability means the guest
	// is usable, if it doesn't post to IMDS /v0/ready first. Default 22.
	ReadyPort uint16
	// Settle is how long to wait after the guest is usable before saving,
	// so background startup work finishes. Default 10s.
	Settle time.Duration
}

// Warm builds the image's warm snapshot: it cold boots a VM, waits until
// the guest is usable, saves the machine state, persists the disk's writes
// onto a clone of the image's disk, and publishes the disk, aux and state
// files as the image's warm directory, replacing any previous one.
//
// The VM is configured exactly like a normal VM, including its disk being
// served over NBD, because the framework only restores a state into a
// configuration with the same kind of disk attachment it was saved with.
func Warm(ctx context.Context, wc WarmConfig) (*image.WarmInfo, error) {
	cfg := wc.Config
	cfg.Boot = BootCold
	cfg.Services = nil
	cfg.withDefaults()
	if wc.ReadyPort == 0 {
		wc.ReadyPort = 22
	}
	if wc.Settle == 0 {
		wc.Settle = 10 * time.Second
	}
	im := cfg.Image
	hash, err := im.ManifestHash()
	if err != nil {
		return nil, err
	}

	final := im.WarmPath("")
	tmp := filepath.Clean(final) + ".tmp"
	if err := os.RemoveAll(tmp); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(tmp, 0o755); err != nil {
		return nil, err
	}
	ok := false
	defer func() {
		if !ok {
			os.RemoveAll(tmp)
		}
	}()
	aux := filepath.Join(tmp, image.AuxFile)
	if err := image.CloneFile(im.AuxPath(), aux); err != nil {
		return nil, err
	}

	t0 := time.Now()
	v, err := start(ctx, cfg, []bootPlan{{
		diskPath:    im.DiskPath(),
		auxPath:     aux,
		persistPath: filepath.Join(tmp, image.DiskFile),
	}})
	if err != nil {
		return nil, err
	}
	// Stopping includes persisting the disk, which copies every page the
	// guest wrote.
	stopCtx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	defer v.Stop(stopCtx, true)

	if err := v.WaitReady(ctx, wc.ReadyPort); err != nil {
		return nil, fmt.Errorf("waiting for guest: %w", err)
	}
	v.logf("warm-up guest ready after %v; settling for %v", time.Since(t0).Round(time.Millisecond), wc.Settle)
	select {
	case <-time.After(wc.Settle):
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	t1 := time.Now()
	if err := v.machine.SaveState(filepath.Join(tmp, image.StateFile)); err != nil {
		return nil, fmt.Errorf("saving state: %w", err)
	}
	v.logf("saved state in %v", time.Since(t1).Round(time.Millisecond))
	v.mu.Lock()
	v.persist = true
	v.mu.Unlock()
	v.Stop(stopCtx, true)
	select {
	case <-v.Done():
	default:
		return nil, errors.New("timed out stopping the warm-up VM and persisting its disk")
	}
	v.mu.Lock()
	err = v.persistErr
	v.mu.Unlock()
	if err != nil {
		return nil, err
	}

	if err := os.RemoveAll(final); err != nil {
		return nil, err
	}
	if err := os.Rename(tmp, final); err != nil {
		return nil, err
	}
	ok = true
	wi := &image.WarmInfo{
		HostOSBuild:  cfg.Backend.HostOSBuild(),
		ManifestHash: hash,
		CPUs:         cfg.CPUs,
		MemoryBytes:  cfg.MemoryBytes,
		Created:      cfg.Now().UTC(),
	}
	if err := im.SaveWarmInfo(wi); err != nil {
		return nil, err
	}
	return wi, nil
}
