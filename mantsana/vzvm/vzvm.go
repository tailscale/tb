// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

//go:build darwin && arm64

// Package vzvm is the Virtualization.framework backend for package guest:
// it turns a guest.BootSpec into a running macOS VM using
// github.com/Code-Hex/vz/v3.
//
// The device set is fixed and identical for cold boots, warm-up boots and
// restores, because a saved state only restores into a matching
// configuration. See DESIGN.md section 4.2.
package vzvm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/Code-Hex/vz/v3"
	"github.com/tailscale/tb/mantsana/guest"
	"golang.org/x/sys/unix"
)

// Backend is a guest.Backend using Virtualization.framework.
type Backend struct {
	// NoSerial omits the virtio serial console, in case it is found to
	// block save/restore.
	NoSerial bool
	// NBDTimeout is how long the framework's NBD client waits before
	// reconnecting. Default 5s.
	NBDTimeout time.Duration
}

var _ guest.Backend = (*Backend)(nil)

// HostOSBuild implements guest.Backend.
func (b *Backend) HostOSBuild() string {
	s, _ := unix.Sysctl("kern.osversion")
	return s
}

// VMLimit implements guest.Backend. The framework allows two macOS VMs.
func (b *Backend) VMLimit() int { return 2 }

// config is a built VM configuration and the host resources it references.
type config struct {
	vc      *vz.VirtualMachineConfiguration
	consR   *os.File // our end of the serial console output pipe
	consW   *os.File // the framework's end
	devNull *os.File // the serial console's input
}

func (c *config) close() {
	for _, f := range []*os.File{c.consR, c.consW, c.devNull} {
		if f != nil {
			f.Close()
		}
	}
}

func (b *Backend) buildConfig(spec *guest.BootSpec) (_ *config, retErr error) {
	m := spec.Manifest
	if m.OS != "macos" {
		return nil, fmt.Errorf("vzvm: unsupported guest OS %q", m.OS)
	}
	c := new(config)
	defer func() {
		if retErr != nil {
			c.close()
		}
	}()

	bl, err := vz.NewMacOSBootLoader()
	if err != nil {
		return nil, fmt.Errorf("boot loader: %w", err)
	}
	hw, err := vz.NewMacHardwareModelWithData(m.HardwareModel)
	if err != nil {
		return nil, fmt.Errorf("hardware model: %w", err)
	}
	if !hw.Supported() {
		return nil, errors.New("image's hardware model is not supported on this host")
	}
	id, err := vz.NewMacMachineIdentifierWithData(m.MachineID)
	if err != nil {
		return nil, fmt.Errorf("machine identifier: %w", err)
	}
	aux, err := vz.NewMacAuxiliaryStorage(spec.AuxPath)
	if err != nil {
		return nil, fmt.Errorf("auxiliary storage %s: %w", spec.AuxPath, err)
	}
	platform, err := vz.NewMacPlatformConfiguration(
		vz.WithMacHardwareModel(hw),
		vz.WithMacMachineIdentifier(id),
		vz.WithMacAuxiliaryStorage(aux),
	)
	if err != nil {
		return nil, fmt.Errorf("platform: %w", err)
	}

	vc, err := vz.NewVirtualMachineConfiguration(bl, uint(spec.CPUs), spec.MemoryBytes)
	if err != nil {
		return nil, fmt.Errorf("configuration: %w", err)
	}
	vc.SetPlatformVirtualMachineConfiguration(platform)

	// Disk.
	var att vz.StorageDeviceAttachment
	switch {
	case spec.NBDURL != "":
		timeout := b.NBDTimeout
		if timeout == 0 {
			timeout = 5 * time.Second
		}
		att, err = vz.NewNetworkBlockDeviceStorageDeviceAttachment(spec.NBDURL, timeout, false, vz.DiskSynchronizationModeNone)
	case spec.DiskPath != "":
		att, err = vz.NewDiskImageStorageDeviceAttachmentWithCacheAndSync(spec.DiskPath, false, vz.DiskImageCachingModeCached, vz.DiskImageSynchronizationModeFsync)
	default:
		err = errors.New("no disk")
	}
	if err != nil {
		return nil, fmt.Errorf("disk attachment: %w", err)
	}
	blk, err := vz.NewVirtioBlockDeviceConfiguration(att)
	if err != nil {
		return nil, fmt.Errorf("block device: %w", err)
	}
	vc.SetStorageDevicesVirtualMachineConfiguration([]vz.StorageDeviceConfiguration{blk})

	// Network.
	natt, err := vz.NewFileHandleNetworkDeviceAttachment(spec.Net)
	if err != nil {
		return nil, fmt.Errorf("network attachment: %w", err)
	}
	if mtu := int(m.MTU); mtu != 0 && mtu != 1500 {
		if err := natt.SetMaximumTransmissionUnit(mtu); err != nil {
			return nil, fmt.Errorf("network MTU %d: %w", mtu, err)
		}
	}
	nic, err := vz.NewVirtioNetworkDeviceConfiguration(natt)
	if err != nil {
		return nil, fmt.Errorf("network device: %w", err)
	}
	if len(m.MAC) > 0 {
		mac, err := vz.NewMACAddress(net.HardwareAddr(m.MAC))
		if err != nil {
			return nil, fmt.Errorf("MAC: %w", err)
		}
		nic.SetMACAddress(mac)
	}
	vc.SetNetworkDevicesVirtualMachineConfiguration([]*vz.VirtioNetworkDeviceConfiguration{nic})

	// Display, needed for screenshots even though there is no window.
	gfx, err := vz.NewMacGraphicsDeviceConfiguration()
	if err != nil {
		return nil, fmt.Errorf("graphics: %w", err)
	}
	disp, err := vz.NewMacGraphicsDisplayConfiguration(int64(m.Display.Width), int64(m.Display.Height), int64(m.Display.PPI))
	if err != nil {
		return nil, fmt.Errorf("display: %w", err)
	}
	gfx.SetDisplays(disp)
	vc.SetGraphicsDevicesVirtualMachineConfiguration([]vz.GraphicsDeviceConfiguration{gfx})

	// Mac keyboard and trackpad, which macOS 13+ guests use. This device set
	// passes ValidateSaveRestoreSupport on hardware (DESIGN.md section 9).
	kbd, err := vz.NewMacKeyboardConfiguration()
	if err != nil {
		return nil, fmt.Errorf("keyboard: %w", err)
	}
	vc.SetKeyboardsVirtualMachineConfiguration([]vz.KeyboardConfiguration{kbd})
	pad, err := vz.NewMacTrackpadConfiguration()
	if err != nil {
		return nil, fmt.Errorf("trackpad: %w", err)
	}
	vc.SetPointingDevicesVirtualMachineConfiguration([]vz.PointingDeviceConfiguration{pad})

	// vsock, for a future guest agent.
	vsock, err := vz.NewVirtioSocketDeviceConfiguration()
	if err != nil {
		return nil, fmt.Errorf("vsock: %w", err)
	}
	vc.SetSocketDevicesVirtualMachineConfiguration([]vz.SocketDeviceConfiguration{vsock})

	// Serial console.
	if !b.NoSerial {
		if c.devNull, err = os.Open(os.DevNull); err != nil {
			return nil, err
		}
		if c.consR, c.consW, err = os.Pipe(); err != nil {
			return nil, err
		}
		satt, err := vz.NewFileHandleSerialPortAttachment(c.devNull, c.consW)
		if err != nil {
			return nil, fmt.Errorf("serial attachment: %w", err)
		}
		serial, err := vz.NewVirtioConsoleDeviceSerialPortConfiguration(satt)
		if err != nil {
			return nil, fmt.Errorf("serial port: %w", err)
		}
		vc.SetSerialPortsVirtualMachineConfiguration([]*vz.VirtioConsoleDeviceSerialPortConfiguration{serial})
	}

	c.vc = vc
	return c, nil
}

// Validation is the result of validating a configuration.
type Validation struct {
	Valid       error // nil if the configuration is valid
	SaveRestore error // nil if it supports save and restore
}

// Validate builds the configuration for spec and validates it without
// creating a VM.
func (b *Backend) Validate(spec *guest.BootSpec) (Validation, error) {
	c, err := b.buildConfig(spec)
	if err != nil {
		return Validation{}, err
	}
	defer c.close()
	var v Validation
	if ok, err := c.vc.Validate(); !ok || err != nil {
		v.Valid = orErr(err, "invalid configuration")
	}
	if ok, err := c.vc.ValidateSaveRestoreSupport(); !ok || err != nil {
		v.SaveRestore = orErr(err, "save/restore not supported")
	}
	return v, nil
}

func orErr(err error, msg string) error {
	if err != nil {
		return err
	}
	return errors.New(msg)
}

// Boot implements guest.Backend.
func (b *Backend) Boot(ctx context.Context, spec *guest.BootSpec) (_ guest.Machine, retErr error) {
	c, err := b.buildConfig(spec)
	if err != nil {
		return nil, err
	}
	defer func() {
		if retErr != nil {
			c.close()
		}
	}()
	if ok, err := c.vc.Validate(); !ok || err != nil {
		return nil, fmt.Errorf("invalid VM configuration: %w", orErr(err, "invalid"))
	}
	if ok, err := c.vc.ValidateSaveRestoreSupport(); !ok || err != nil {
		spec.Logf("vzvm %s: configuration does not support save/restore: %v", spec.Name, orErr(err, "unsupported"))
	}
	vm, err := vz.NewVirtualMachine(c.vc)
	if err != nil {
		return nil, fmt.Errorf("NewVirtualMachine: %w", err)
	}
	m := &machine{vm: vm, cfg: c, name: spec.Name, logf: spec.Logf, done: make(chan struct{})}
	if c.consR != nil && spec.Console != nil {
		go io.Copy(spec.Console, c.consR)
	}

	if spec.RestoreState != "" {
		if err := vm.RestoreMachineStateFromURL(spec.RestoreState); err != nil {
			return nil, &guest.RestoreError{Err: err, Transient: isKeyUnavailable(err)}
		}
		if err := vm.Resume(); err != nil {
			vm.Stop()
			return nil, &guest.RestoreError{Err: fmt.Errorf("resume: %w", err)}
		}
	} else if err := vm.Start(); err != nil {
		return nil, wrapStartErr(err)
	}
	go m.watch()
	return m, nil
}

// isKeyUnavailable reports whether a restore error is the framework failing
// to get at the saved state's key, which is what happens (observed; Apple
// doesn't document it) when the user's session is locked or the process
// isn't in the user's launchd domain. The saved state itself is fine then.
func isKeyUnavailable(err error) bool {
	var nse *vz.NSError
	if !errors.As(err, &nse) || nse.Code != int(vz.ErrorRestore) {
		return false
	}
	return strings.Contains(nse.LocalizedDescription+nse.UserInfo, "permission denied")
}

// wrapStartErr wraps the framework's VM-limit error in guest.ErrVMLimit.
func wrapStartErr(err error) error {
	var nse *vz.NSError
	if errors.As(err, &nse) && nse.Code == int(vz.ErrorVirtualMachineLimitExceeded) {
		return fmt.Errorf("%w: %v", guest.ErrVMLimit, err)
	}
	return fmt.Errorf("start: %w", err)
}

// machine is a running VM. It implements guest.Machine.
type machine struct {
	vm   *vz.VirtualMachine // kept reachable; vz finalizers release the ObjC object
	cfg  *config
	name string
	logf func(string, ...any)

	mu   sync.Mutex
	err  error
	done chan struct{}
}

// watch drains the VM's state notifications (a single-consumer channel)
// until the VM stops.
func (m *machine) watch() {
	for st := range m.vm.StateChangedNotify() {
		switch st {
		case vz.VirtualMachineStateStopped:
			m.finish(nil)
			return
		case vz.VirtualMachineStateError:
			m.finish(errors.New("virtual machine entered the error state"))
			return
		}
	}
	m.finish(errors.New("state notifications ended"))
}

func (m *machine) finish(err error) {
	m.mu.Lock()
	m.err = err
	m.mu.Unlock()
	m.cfg.close()
	close(m.done)
}

func (m *machine) Done() <-chan struct{} { return m.done }

func (m *machine) Err() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.err
}

func (m *machine) RequestStop() error {
	if !m.vm.CanRequestStop() {
		return errors.New("VM cannot request stop in its current state")
	}
	_, err := m.vm.RequestStop()
	return err
}

func (m *machine) Stop() error {
	if !m.vm.CanStop() {
		return nil
	}
	return m.vm.Stop()
}

func (m *machine) SaveState(path string) error {
	if m.vm.State() != vz.VirtualMachineStatePaused {
		if err := m.vm.Pause(); err != nil {
			return fmt.Errorf("pause: %w", err)
		}
	}
	return m.vm.SaveMachineStateToPath(path)
}
