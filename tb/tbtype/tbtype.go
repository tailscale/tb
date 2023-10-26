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
