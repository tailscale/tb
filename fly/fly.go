// Package fly is a Fly client.
package fly

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
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
	Guest       *MachineGuest     `json:"guest,omitempty"`
}

type ImageRef struct {
	Registry   string `json:"registry"`
	Repository string `json:"repository"`
	Tag        string `json:"tag"`
}

type MachineGuest struct {
	MemoryMB int    `json:"memory_mb,omitempty"`
	CPUs     int    `json:"cpus,omitempty"`
	CPUKind  string `json:"cpu_kind,omitempty"` // must be "shared" or "performance"
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
	CreatedAt  string         `json:"created_at,omitempty"`
}

type CreateMachineRequest struct {
	Config                  *MachineConfig `json:"config,omitempty"`
	Name                    string         `json:"name,omitempty"`
	Region                  string         `json:"region,omitempty"`
	SkipLaunch              bool           `json:"skip_launch,omitempty"`
	SkipServiceRegistration bool           `json:"skip_service_registration,omitempty"`
}

type CreateVolumeRequest struct {
	Compute           *MachineGuest `json:"compute,omitempty"`
	Encrypted         bool          `json:"encrypted,omitempty"`
	FSType            string        `json:"fstype,omitempty"`
	MachinesOnly      bool          `json:"machines_only,omitempty"`
	Name              string        `json:"name,omitempty"` // not a primary key; can be used by multiple at once
	Region            string        `json:"region,omitempty"`
	RequireUniqueZone bool          `json:"require_unique_zone,omitempty"`
	SizeGB            int           `json:"size_gb,omitempty"`
	SnapshotID        string        `json:"snapshot_id,omitempty"`
	SnapshotRetention int           `json:"snapshot_retention,omitempty"`
	SourceVolumeID    string        `json:"source_volume_id,omitempty"`
}

type Volume struct {
	AttachedAllocID   string     `json:"attached_alloc_id,omitempty"`
	AttachedMachineID string     `json:"attached_machine_id,omitempty"`
	BlockSize         int        `json:"block_size,omitempty"`
	Blocks            int        `json:"blocks,omitempty"`
	BlocksAvail       int        `json:"blocks_avail,omitempty"`
	BlocksFree        int        `json:"blocks_free,omitempty"`
	CreatedAt         *time.Time `json:"created_at,omitempty"` // string technically?
	Encrypted         bool       `json:"encrypted,omitempty"`
	FSType            string     `json:"fstype,omitempty"`
	ID                string     `json:"id,omitempty"`
	Name              string     `json:"name,omitempty"` // not a primary key; can be used by multiple at once
	Region            string     `json:"region,omitempty"`
	SizeGB            int        `json:"size_gb,omitempty"`
	SnapshotRetention int        `json:"snapshot_retention,omitempty"`
	State             string     `json:"state,omitempty"`
	Zone              string     `json:"zone,omitempty"`
}

func (c *Client) CreateMachine(ctx context.Context, r *CreateMachineRequest) (*Machine, error) {
	return doFlyRequest[*Machine](ctx, c, "POST", "/v1/apps/"+c.App+"/machines", jsonBody(r))
}

func (c *Client) ListMachines(ctx context.Context) ([]*Machine, error) {
	return doFlyRequest[[]*Machine](ctx, c, "GET", c.path("machines"), nil)
}

func (c *Client) ListVolumes(ctx context.Context) ([]*Volume, error) {
	return doFlyRequest[[]*Volume](ctx, c, "GET", c.path("volumes"), nil)
}

type StopParam struct {
	Signal  string
	Timeout time.Duration
}

func (c *Client) StopMachine(ctx context.Context, id MachineID, optParam *StopParam) error {
	var getBody func() (io.ReadCloser, error)
	if p := optParam; p != nil {
		var s struct {
			Signal  string `json:"signal,omitempty"`
			Timeout string `json:"timeout,omitempty"`
		}
		s.Signal = p.Signal
		if p.Timeout != 0 {
			s.Timeout = p.Timeout.String()
		}
		getBody = jsonBody(&s)
	}
	return c.toMachineID(ctx, "POST", id, "/stop", getBody)
}

func (c *Client) StartMachine(ctx context.Context, id MachineID) error {
	return c.toMachineID(ctx, "POST", id, "/start", nil)
}

func (c *Client) DeleteMachine(ctx context.Context, id MachineID) error {
	// Note the undocumented ?force=true query param. :)
	return c.toMachineID(ctx, "DELETE", id, "?force=true", nil)
}

func (c *Client) toMachineID(ctx context.Context, method string, id MachineID, suffix string, getBody func() (io.ReadCloser, error)) error {
	_, err := doFlyRequest[any](ctx, c, method, "/v1/apps/"+c.App+"/machines/"+string(id)+suffix, getBody)
	return err
}

func (c *Client) CreateVolume(ctx context.Context, r *CreateVolumeRequest) (*Volume, error) {
	return doFlyRequest[*Volume](ctx, c, "POST", "/v1/apps/"+c.App+"/volumes", jsonBody(r))
}

func (c *Client) DeleteVolume(ctx context.Context, volumeID string) error {
	_, err := doFlyRequest[any](ctx, c, "DELETE", "/v1/apps/"+c.App+"/volumes/"+volumeID, nil)
	return err

}

func doFlyRequest[Res any](ctx context.Context, c *Client, method, path string, getBody func() (io.ReadCloser, error)) (Res, error) {
	var zero Res
	hreq, err := http.NewRequestWithContext(ctx, method, c.base()+path, nil)
	if err != nil {
		return zero, err
	}
	if getBody != nil {
		hreq.GetBody = getBody
		hreq.Body, err = getBody()
		if err != nil {
			return zero, err
		}
	}
	hreq.Header.Set("Authorization", "Bearer "+c.Token)
	res, err := c.httpc().Do(hreq)
	if err != nil {
		return zero, err
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		body, _ := io.ReadAll(res.Body)
		var resj struct {
			Error string
		}
		if err := json.Unmarshal(body, &resj); err == nil {
			return zero, errors.New(resj.Error)
		}
		return zero, fmt.Errorf("%s: %s", res.Status, body)
	}
	var ret Res
	err = json.NewDecoder(res.Body).Decode(&ret)
	return ret, err
}

func (c *Client) path(suffix string) string {
	return "/v1/apps/" + c.App + "/" + suffix
}

func jsonBody(s any) (getBody func() (io.ReadCloser, error)) {
	return func() (io.ReadCloser, error) {
		j, err := json.Marshal(s)
		if err != nil {
			return nil, err
		}
		return io.NopCloser(bytes.NewReader(j)), nil
	}
}
