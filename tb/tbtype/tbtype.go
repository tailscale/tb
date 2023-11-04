// Package tbtype contains type definitions for tb API calls.
package tbtype

import (
	"errors"
	"strings"
)

// FetchResponse is the JSON response of a POST /fetch call.
type FetchResponse struct {
	RemoteRef string `json:"remoteRef,omitempty"`

	Error string `json:"error,omitempty"`

	LocalRef string `json:"localRef,omitempty"`
	Hash     string `json:"hash,omitempty"`
}

type Task struct {
	Name   string `json:",omitempty"`
	GOOS   string `json:",omitempty"`
	GOARCH string `json:",omitempty"`
	Race   bool   `json:",omitempty"`

	Action   string   `json:",omitempty"` // "build", "test", "staticcheck"
	Packages []string `json:",omitempty"`
}

func (t *Task) SetPlatform(p string) error {
	p, t.Race = strings.CutSuffix(p, "/race")
	var ok bool
	t.GOOS, t.GOARCH, ok = strings.Cut(p, "/")
	if !ok {
		return errors.New("no slash in GOOS/GOARCH platform")
	}
	return nil
}

type BuildRequest struct {
	Ref      string  `json:"ref"`
	Tasks    []*Task `json:"tasks,omitempty"`
	Machines int     `json:"machines,omitempty"`
}

// ExecRequest is a request from the controller to the worker buildlet to
// execute a command. It's not sent by end users.
//
// For all args, ${CODEDIR} and ${HOME} interpolate.
type ExecRequest struct {
	Dir  string // empty means ${CODEDIR}
	Cmd  string // leading "./" means relative to ${CODEDIR}
	Args []string
	Env  []string

	IgnoreStdout bool
	IgnoreStderr bool

	MergeStderrIntoStdout bool
	TimeoutSeconds        float64 // 0 means no timeout
}

type ExecStream struct {
	O  string `json:",omitempty"` // stdout, if valid UTF-8
	OB []byte `json:",omitempty"` // stdout, if not UTF-8
	E  string `json:",omitempty"` // stderr, if valid UTF-8
	EB []byte `json:",omitempty"` // stderr, if not UTF-8

	// Final message includes:
	Exit *int    `json:",omitempty"` // exit status, if exited (pointer to zero on success)
	Dur  float64 `json:",omitempty"` // seconds it took to run, at exit
	Err  string  `json:",omitempty"` // error message, if error
}

const CmdNetMesh = "builtin:netmesh"
