// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package guestlet

import (
	"net/netip"
	"time"

	"github.com/tailscale/tb/ci/ciid"
	"github.com/tailscale/tb/ci/guestlettype"
	"github.com/tailscale/tb/ci/labels"
	"github.com/tailscale/tb/ci/statustype"
	"github.com/tailscale/tb/ci/vmimage"
)

// Purpose describes why a VM was created.
type Purpose string

const (
	// PurposeGitHubBaseline are VMs configured by cihostlet's flags. They are
	// pre-created before any GitHub Actions jobs are queued, and once they
	// exit, they are restarted with the same configuration. As of today (2026-03-20)
	// they are running production CI jobs, but they will get getting replaced
	// by on-demand API-created VMs in the future. See https://github.com/tailscale/corp/issues/31758.
	PurposeGitHubBaseline Purpose = "github-baseline"
	// PurposeGitHub are VMs created on-demand via cihostlet's API with
	// ciguestlet's --runner=true flag to register as a GitHub Actions runner.
	// Once they complete a job, they exit and are garbage collected.
	PurposeGitHub Purpose = "github"
	// PurposeInteractive are VMs created for interactive use, e.g. for debugging
	// the CI environment. They run until they are manually shutdown or are
	// deleted via the API, and are garbage collected after exiting.
	PurposeInteractive Purpose = "interactive"
	// PurposeBuild are VMs created by cmd/binbucket to run a single build.
	// Like interactive VMs, they boot to a ready state and wait for SSH
	// rather than registering as a GitHub Actions runner. Their creator
	// deletes them when the build is done, and they are garbage collected
	// after exiting.
	PurposeBuild Purpose = "build"
)

// RunsGitHubRunner reports whether VMs of this purpose register as a GitHub
// Actions runner. Interactive VMs do not; they boot to a ready state and wait
// for an SSH session instead.
func (p Purpose) RunsGitHubRunner() bool {
	return p == PurposeGitHubBaseline || p == PurposeGitHub
}

// Opts is the request body for cihostlet's API POST /api/vms.
type Opts struct {
	Purpose      Purpose         `json:"purpose,omitzero"`       // One of "github", "interactive", or "build". The API rejects type "github-baseline", which can only be configured via cihostlet flags. Defaults to "interactive".
	RunnerLabels labels.Labels   `json:"runner_labels,omitzero"` // GitHub Actions runner labels. Required if Purpose is "github".
	OS           guestlettype.OS `json:"os,omitzero"`            // Guest OS: OSLinux (Firecracker), OSWindows (QEMU), or OSFreeBSD (QEMU). Optional; defaults to cihostlet's own runtime.GOOS.
	CPUs         int             `json:"cpus,omitzero"`          // Number of vCPUs to assign to the VM. Optional; defaults to cihostlet's --guest-vm-cpus flag.
	RAMGiB       int             `json:"ram_gib,omitzero"`       // Memory limit (in GiB) for the VM. Optional; defaults to cihostlet's --guest-vm-ram-gb flag.

	// ImageVersion selects a guest base image from the ci-guest-vm-images
	// bucket. Use "stable" for the latest production image, "unstable" for the
	// newest candidate, or a version directory name to pin a stable or unstable
	// image, e.g. "2026-06-04T091200Z-a1b2c3d4e5" or
	// "2026-06-04T091200Z-a1b2c3d4e5-unstable". An empty value means "stable".
	ImageVersion string `json:"image_version,omitzero"`

	// TTLSeconds is how long the runner will live before cihostlet starts
	// shutting it down. If it has picked up a GitHub job by that time, it will
	// finish the job before exiting. Optional; defaults to unlimited.
	TTLSeconds int `json:"ttl_seconds,omitzero"`

	// GitProxy is whether ciguestlet should listen on the guest-facing
	// gateway IP on the git port (9418) and proxy connections to the
	// rogitproxy service, letting the guest clone private GitHub repos
	// without credentials (e.g. git clone git://192.168.101.1/tailscale/corp).
	GitProxy bool `json:"git_proxy,omitzero"`

	// CreatedBy is the Tailscale login name of the user who created this VM via
	// the API. It is not set by API clients; instead, cihostlet populates it
	// based on the authenticated user making the API request.
	CreatedBy string `json:"-"`

	// SSHBearerToken is a per-VM secret that authorizes callers to use
	// cihostlet's SSH proxy for this VM. It is set by trusted schedulers such
	// as cimgr and is never exposed by cihostlet status APIs.
	SSHBearerToken string `json:"ssh_bearer_token,omitzero"`

	// AllowVPCAccess controls whether the guest can connect to private VPC addresses.
	// If true, the DNS proxy will resolve the configured DNS VPC suffixes using
	// the VPC resolver instead of public upstream resolvers.
	// TODO(tomhjp): currently (2026-08-19) this only changes DNS proxy behaviour;
	// make it actually drop VPC-bound traffic when false in a follow-up.
	AllowVPCAccess bool `json:"allow_vpc_access,omitzero"`

	// AllowTestStatsDBAccess is whether the guest may read and write the CI
	// test history through ciguestlet.
	//
	// The guest never holds database credentials. gotst in the guest uses HTTP
	// to ciguestlet which will proxy this to a testhistoryd service.
	//
	// TODO(samw): as of 2026-09-18 nothing consumes this field; see
	// tailscale/corp#31723.
	AllowTestStatsDBAccess bool `json:"allow_test_stats_db_access,omitzero"`
}

// Guestlet describes a VM managed by cihostlet, combining the hostlet-known
// resource configuration with the guestlet-reported runtime status. It is the
// response body for cihostlet's /api/vms APIs.
type Guestlet struct {
	Name         ciid.GuestletName      `json:"name"`                   // A globally unique name for the VM, e.g. ci-linux-1-2-1773972710 is ci-linux-1's guest number 2 started at unix time 1773972710. Guestlet flag --name.
	Number       int                    `json:"number"`                 // The 1-indexed slot number of the VM, used in its base name.
	BaseName     string                 `json:"base_name"`              // e.g. "ci-linux-1-2", which is ci-linux-1's guest number 2. Guestlet flag --base-name.
	OS           guestlettype.OS        `json:"os,omitzero"`            // Guest OS: OSLinux, OSWindows, or OSFreeBSD. Empty means OSLinux.
	CreatedBy    string                 `json:"created_by,omitzero"`    // Tailscale login name of the user who created this VM via the API. Empty for baseline VMs.
	PrivateIP    netip.Addr             `json:"private_ip"`             // A 192.168 private RFC 1918 address for the VM, only unique per host. Guestlet flag --vm-ip.
	APIPort      int                    `json:"api_port"`               // The port the guestlet listens on to serve its status API. Guestlet flag --listen=:<PORT>.
	Purpose      Purpose                `json:"purpose"`                // One of "github-baseline", "github", or "interactive".
	RunnerLabels labels.Labels          `json:"runner_labels,omitzero"` // GitHub Actions runner labels. Required if Purpose is "github". Guestlet flag --runner-labels.
	CPUs         int                    `json:"cpus"`                   // vCPUs for the VM. Guestlet flag --vm-cpus.
	RAMGiB       int                    `json:"ram_gib"`                // Memory limit (in GiB) for the VM. Guestlet flag --vm-ram-gb.
	GitProxy     bool                   `json:"git_proxy,omitzero"`     // Whether ciguestlet proxies the guest-facing gateway's git port (9418) to rogitproxy. See Opts.GitProxy. Guestlet flag --git-proxy.
	Image        *vmimage.Image         `json:"image,omitzero"`         // The resolved base image and its host paths. It is nil for darwin guests.
	CreatedAt    time.Time              `json:"created_at"`             // Timestamp when the VM was created, according to the hostlet.
	TTLDeadline  time.Time              `json:"ttl_deadline,omitzero"`  // When the hostlet stops the VM if it is still idle, from Opts.TTLSeconds and later extensions. Zero if it has no TTL.
	Status       statustype.GuestStatus `json:"status"`                 // Status is the latest status reported by the guestlet itself, which may be nil if the guestlet hasn't yet started or can't be reached.
	DebugServer  string                 `json:"debug_server"`           // DebugServer is the tailnet address of the guestlet's debug server.
	StatusError  string                 `json:"status_error,omitzero"`  // StatusError, if non-empty, is any error encountered while fetching the guestlet's status.

	// AllowVPCAccess controls whether the guest is allowed to connect to private
	// addresses in the VPC.
	AllowVPCAccess bool `json:"allow_vpc_access,omitzero"`

	// AllowTestStatsDBAccess is whether the guest may read and write the CI
	// test history through ciguestlet. See Opts.AllowTestStatsDBAccess.
	AllowTestStatsDBAccess bool `json:"allow_test_stats_db_access,omitzero"`

	// SSHBearerToken authorizes direct SSH proxy access through the owning
	// cihostlet. It is process-local secret state and intentionally omitted
	// from JSON, including HostInfo updates sent to cimgr.
	SSHBearerToken string `json:"-"`
}

// SSHPortForOS returns the TCP port on which to reach SSH for the given guest
// OS. Most guests run an SSH server on the standard port 22, but plan9 has no
// native SSH and uses a host-side proxy on port 2222 to avoid colliding with
// the cihostlet host's own sshd on port 22.
func SSHPortForOS(os guestlettype.OS) int {
	if os == guestlettype.OSPlan9 {
		return 2222
	}
	return 22
}

// SSHCredsForOS returns the username and password to use when connecting to a
// guest through cihostlet's SSH proxy. hostGOOS is the cihostlet's runtime.GOOS
// value; macOS guests are identified by running on a darwin hostlet.
func SSHCredsForOS(os guestlettype.OS, hostGOOS string) (user, pass string) {
	switch {
	case os == guestlettype.OSWindows:
		return "Administrator", "admin"
	case os == guestlettype.OSFreeBSD:
		return "freebsd", "admin"
	case os == guestlettype.OSPlan9:
		// Plan 9 has no SSH server; ciguestlet runs a host-side SSH-to-serial
		// console proxy that authenticates with these credentials.
		return "glenda", "glenda123"
	case hostGOOS == "darwin":
		return "admin", "admin"
	default:
		return "ubuntu", "admin"
	}
}
