// Package fly is a Fly client.
package fly

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/netip"
	"os"
	"reflect"
	"time"

	"tailscale.com/util/cmpx"
)

type MachineID string

type Client struct {
	App   string
	Token string
	Base  string // or empty for "https://api.machines.dev"
}

func (c *Client) base() string {
	return cmpx.Or(c.Base, "https://api.machines.dev")
}

func (c *Client) httpc() *http.Client {
	return http.DefaultClient
}

type CheckStatus struct {
	Name      string `json:"name,omitempty"`
	Output    string `json:"output,omitempty"`
	Status    string `json:"status,omitempty"`
	UpdatedAt string `json:"updated_at,omitempty"`
}

type MachineRestart struct {
	MaxRetries int    `json:"max_retries,omitempty"`
	Policy     string `json:"policy,omitempty"` // "no", "always", "on-fail"
}

type MachineConfig struct {
	AutoDestroy bool              `json:"auto_destroy,omitempty"`
	Env         map[string]string `json:"env,omitempty"`
	Image       string            `json:"image,omitempty"`
	Restart     *MachineRestart   `json:"restart,omitempty"`
}

type ImageRef struct {
	Registry   string `json:"registry"`
	Repository string `json:"repository"`
	Tag        string `json:"tag"`
}

type Machine struct {
	Checks     []*CheckStatus `json:"checks,omitempty"`
	Config     *MachineConfig `json:"config,omitempty"`
	ImageRef   *ImageRef      `json:"image_ref,omitempty"`
	ID         MachineID      `json:"id,omitempty"`
	Name       string         `json:"name,omitempty"`
	State      string         `json:"state,omitempty"`
	InstanceID string         `json:"instance_id,omitempty"`
	Nonce      string         `json:"nonce,omitempty"`
	Region     string         `json:"region,omitempty"`
	PrivateIP  *netip.Addr    `json:"private_ip,omitempty"`
}

type CreateMachineRequest struct {
	Config                  *MachineConfig `json:"config,omitempty"`
	Name                    string         `json:"name,omitempty"`
	Region                  string         `json:"region,omitempty"`
	SkipLaunch              bool           `json:"skip_launch,omitempty"`
	SkipServiceRegistration bool           `json:"skip_service_registration,omitempty"`
}

func (c *Client) CreateMachine(ctx context.Context, r *CreateMachineRequest) (*Machine, error) {
	j, err := json.Marshal(r)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", c.base()+"/v1/apps/"+c.App+"/machines", bytes.NewReader(j))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	res, err := c.httpc().Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		res.Write(os.Stderr)
		return nil, errors.New(res.Status)
	}
	m := new(Machine)
	if err := json.NewDecoder(res.Body).Decode(m); err != nil {
		return nil, err
	}
	return m, nil
}

func (c *Client) ListMachines(ctx context.Context) ([]*Machine, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", c.base()+"/v1/apps/"+c.App+"/machines", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	res, err := c.httpc().Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return nil, errors.New(res.Status)
	}
	var ret []*Machine
	err = json.NewDecoder(res.Body).Decode(&ret)
	return ret, err
}

type StopParam struct {
	Signal  string
	Timeout time.Duration
}

func (c *Client) StopMachine(ctx context.Context, id MachineID, optParam *StopParam) error {
	var body any
	if p := optParam; p != nil {
		var s struct {
			Signal  string `json:"signal,omitempty"`
			Timeout string `json:"timeout,omitempty"`
		}
		s.Signal = p.Signal
		if p.Timeout != 0 {
			s.Timeout = p.Timeout.String()
		}
		body = s
	}
	return c.toMachineID(ctx, "POST", id, "/stop", body)
}

func (c *Client) StartMachine(ctx context.Context, id MachineID) error {
	return c.toMachineID(ctx, "POST", id, "/start", nil)
}

func (c *Client) DeleteMachine(ctx context.Context, id MachineID) error {
	return c.toMachineID(ctx, "DELETE", id, "", nil)
}

func isNilPtr(v any) bool {
	rv := reflect.ValueOf(v)
	return rv.Kind() == reflect.Pointer && rv.IsZero()
}

func (c *Client) toMachineID(ctx context.Context, method string, id MachineID, suffix string, optBody any) error {
	var body io.Reader
	if optBody != nil && !isNilPtr(optBody) {
		j, err := json.Marshal(optBody)
		if err != nil {
			return err
		}
		body = bytes.NewReader(j)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base()+"/v1/apps/"+c.App+"/machines/"+string(id)+suffix, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	res, err := c.httpc().Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return errors.New(res.Status)
	}
	return nil
}
