// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

// Package statustype contains status types shared between different CI infrastructure components.
package statustype

import (
	"fmt"
	"net/netip"
	"slices"
	"strconv"

	"github.com/tailscale/tb/ci/ciid"
)

// GuestStatus it the status of a guestlet (Tart, Firecracker, QEMU) shared
// to cmd/cihostlet.
type GuestStatus struct {
	// Name is the guest instance name,
	// such as "mac-2-<unixtimestamp/rand>-2".
	Name ciid.GuestletName `json:"name"`

	// State is what state the guest instance is in.
	State GuestState `json:"state"`

	// IP is the guest VM's IP address once known, as reported by the guestlet.
	// It is empty until the VM has an IP. For macOS (tart) guests the address is
	// assigned dynamically by Apple's vmnet, so this is the only way cihostlet
	// learns the VM's reachable address; for other guests it matches the static
	// per-slot IP cihostlet already assigned.
	IP netip.Addr `json:"ip,omitzero"`

	// StateSeconds is how many seconds the tartup process
	// has been in State.
	StateSeconds float64 `json:"state_seconds"`

	// StateTime is the unix timestamp (seconds since epoch) when the
	// guestlet entered the current State.
	StateTime float64 `json:"state_time"`

	// UptimeSeconds is how many seconds the tartup process
	// has been running.
	UptimeSeconds float64 `json:"uptime_seconds"`

	// StartTime is the unix timestamp (seconds since epoch) when the
	// guestlet process started.
	StartTime float64 `json:"start_time"`

	// LogTail is the last few lines of its logs.
	LogTail string `json:"log_tail"`

	// GitHubRunnerID is the GitHub Actions numeric runner ID assigned by
	// GitHub when the JIT config is generated. It is zero for non-GitHub
	// guestlets and for GitHub guestlets that have not yet registered with
	// GitHub.
	GitHubRunnerID int64 `json:"github_runner_id,omitzero"`

	// GitHubEnv contains the GitHub Actions runner environment variables
	// from the guest VM. These are set once the VM receives a job and nil
	// before that.
	// See https://docs.github.com/en/actions/reference/workflows-and-actions/variables#default-environment-variables
	GitHubEnv map[string]string `json:"github_env,omitempty"`

	// StartedJob is true if the guestlet has started a GitHub Actions job. It is
	// false for non-GitHub guestlets and for GitHub guestlets that have not yet
	// started a job.
	StartedJob bool `json:"started_job"`

	// TODO(bradfitz): add guest VM resource metrics: CPU usage,
	// network?, NFS rate, etc?
}

// StateIsOneOf reports whether the GuestStatus's State is one of the given states.
func (gs *GuestStatus) StateIsOneOf(states ...GuestState) bool {
	return slices.Contains(states, gs.State)
}

// GuestState is the state of a guest instance.
// It's a string suitable for using in a Prometheus label:
// just lowercase ASCII and hyphens. No spaces, etc.
type GuestState string

const (
	StatePausedForMaintenance GuestState = "paused-for-maintenance"

	StateNew              GuestState = "new"
	StateCleanUpResources GuestState = "cleaning-up-resources"
	StateCloneImage       GuestState = "clone-image"
	StateSetCPUs          GuestState = "set-cpus"
	StateSetMemory        GuestState = "set-memory"
	StateStarting         GuestState = "starting-vm"
	StateWaitIP           GuestState = "wait-ip"
	StateWaitSSH          GuestState = "wait-ssh"
	StateMount            GuestState = "mount"
	StatePushTar          GuestState = "push-tar"
	StateReady            GuestState = "ready"

	StateGenerateJITConfig      GuestState = "generate-jit-config"
	StateGitHubRunnerWaiting    GuestState = "github-runner-waiting"
	StateGitHubRunnerRunningJob GuestState = "github-runner-running-job"
	StateGitHubCompletedJob     GuestState = "github-runner-completed-job"
	StateRunBlock               GuestState = "run-dev-mode-block"
	StateStopVM                 GuestState = "stop-vm"
	StateUnknown                GuestState = "unknown"

	// Linux VMs only
	StatePrepareVMResources              GuestState = "prepare-vm-resources"
	StateConfigureNetworking             GuestState = "configure-networking"
	StateGenerateVMConfig                GuestState = "generate-vm-config"
	StateCreateLogFile                   GuestState = "create-logs-file"
	StateStartingGHStateTransitionServer GuestState = "starting-gh-state-transition-server"
	StateWaitWork                        GuestState = "wait-work"
)

// GitHubActionsRunInfo accepts as input map of environment variable names and
// values (that should be from an environment with a GitHub Actions runner
// that's running a job) and returns GitHub Actions run ID and the URL of the
// job run.
func GitHubActionsRunInfo(env map[string]string) (runID int64, runURL string) {
	const (
		// https://docs.github.com/en/actions/reference/workflows-and-actions/variables#default-environment-variables
		gitHubRepo  = "GITHUB_REPOSITORY" // tailscale/corp
		gitHubRunID = "GITHUB_RUN_ID"     // "17272792539"
	)
	if env == nil {
		return 0, ""
	}
	repo := env[gitHubRepo]
	runIDStr := env[gitHubRunID]
	if runIDStr != "" && repo != "" {
		var err error
		runID, err = strconv.ParseInt(runIDStr, 10, 64)
		if err == nil {
			runURL = fmt.Sprintf("https://github.com/%s/actions/runs/%d", repo, runID)
			return runID, runURL
		}
	}
	return 0, ""
}
