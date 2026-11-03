// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

//go:build unix

package guest

import (
	"context"
	"errors"
	"io"
	"os"

	"github.com/tailscale/tb/mantsana/image"
	"tailscale.com/types/logger"
)

// Backend boots VMs. The real one is package vzvm (macOS only); tests use a
// fake. Everything else about a VM (its network, IMDS, NBD disk server, event
// log) is portable and owned by this package, so a backend only has to turn
// a BootSpec into a running machine.
type Backend interface {
	// HostOSBuild returns the host OS build, such as "25A354". Warm
	// snapshots are only valid on the build that saved them.
	HostOSBuild() string
	// VMLimit returns how many VMs may run at once (2 for macOS guests).
	VMLimit() int
	// Boot creates and starts (or restores and resumes) a VM. It returns
	// once the VM is running. Errors wrapping ErrVMLimit or *RestoreError
	// are reported specially.
	Boot(ctx context.Context, spec *BootSpec) (Machine, error)
}

// BootSpec is everything a backend needs to boot one VM.
type BootSpec struct {
	Name        string
	Manifest    *image.Manifest
	CPUs        int
	MemoryBytes uint64

	// Exactly one of NBDURL and DiskPath is set. NBDURL is an
	// "nbd+unix:///?socket=/path" URL for the server's discard-writes NBD
	// server; it is used for all normal VMs. DiskPath is a raw disk image
	// to attach read-write, used when building a warm snapshot.
	NBDURL   string
	DiskPath string

	// AuxPath is the macOS auxiliary storage (NVRAM) file. It is this VM's
	// private copy and may be written.
	AuxPath string

	// Net is the VM side of a SOCK_DGRAM socketpair carrying Ethernet
	// frames. The backend must not close it; the caller closes it after
	// Boot returns.
	Net *os.File

	// RestoreState, if non-empty, is a saved state file to restore and
	// resume instead of cold booting.
	RestoreState string

	// Console receives the guest's serial console output.
	Console io.Writer

	Logf logger.Logf
}

// Machine is a running VM.
type Machine interface {
	// Done is closed when the VM has stopped for any reason.
	Done() <-chan struct{}
	// Err returns why the VM stopped, or nil for a clean guest shutdown. It
	// is only valid after Done is closed.
	Err() error
	// RequestStop asks the guest OS to shut down.
	RequestStop() error
	// Stop stops the VM immediately, like pulling the power.
	Stop() error
	// SaveState pauses the VM and saves its memory and device state to path.
	// The VM stays paused; the caller then stops it.
	SaveState(path string) error
}

// ErrVMLimit is wrapped by Backend.Boot errors when the framework refuses
// to start another VM.
var ErrVMLimit = errors.New("virtualization framework VM limit exceeded")

// RestoreError is returned by Backend.Boot when restoring RestoreState
// failed, so the caller can cold boot instead, and mark the snapshot stale
// unless the failure is Transient.
type RestoreError struct {
	Err error
	// Transient reports that the snapshot itself is probably fine and a later
	// restore may succeed: on macOS, restoring needs the user's session to be
	// unlocked (DESIGN.md section 9).
	Transient bool
}

func (e *RestoreError) Error() string { return "restore failed: " + e.Err.Error() }
func (e *RestoreError) Unwrap() error { return e.Err }
