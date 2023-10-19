// The pilot command flies Fly.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"log"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"tailscale.com/types/logger"
	"tailscale.com/util/cmpx"
	"tailscale.com/util/must"
)

/*
flyctl machines -a tb run --restart=no --region=sea --name=base
--autostart=false --autostop=false .

{"error":"failed_precondition: unable to destroy machine, not currently
stopped"}

flyctl proxy 2201:22 fdaa:0:4551:a7b:1a9:f253:809f:2 Proxying local port 2201 to
remote [fdaa:0:4551:a7b:1a9:f253:809f:2]:22


fly apps create tb-no-secrets --network=tb-no-secrets-net

https://fly.io/docs/machines/working-with-machines/
	"To segment the app into its own network, you can pass a network argument in the
	JSON body, e.g. "network": "some-arbitrary-name". Any machine started in such an
	app will not be able to access other apps within their organization over the
	private network. However, machines within such an app can communicate to each
	other, and the fly-replay header can still be used to route requests to machines
	within a segmented app."

fly-replay https://fly.io/docs/reference/dynamic-request-routing/

https://fly.io/docs/reference/private-networking/
   fly ips allocate-v6 --private
   "If you want to expose services to another Fly organization you have access to, use the --org flag."

*/

var (
	deleteAll = flag.Bool("delete-all", false, "delete all machines and exit")
	stopAll   = flag.Bool("stop-all", false, "stop all machines and exit")
	startAll  = flag.Bool("start-all", false, "start all machines and exit")
	create    = flag.Bool("create", false, "make a thing")
	app       = flag.String("app", "tb", "fly app name")
)

func main() {
	flag.Parse()

	c := &Client{
		App: *app,
		//Token: strings.TrimSpace(string(must.Get(exec.Command("flyctl", "auth", "token").Output()))),
		Token: strings.TrimSpace(string(must.Get(os.ReadFile(filepath.Join(os.Getenv("HOME"), "keys", "fly-ci-token"))))),
	}
	ctx := context.Background()

	if *create {
		m, err := c.CreateMachine(ctx, &CreateMachineRequest{
			Region: "sea",
			Config: &MachineConfig{
				AutoDestroy: true,
				Env: map[string]string{
					"FOO_BAR_ENV": "baz",
				},
				Image: "registry.fly.io/tb-no-secrets:deployment-01HD4WY9DEAESP8Z61377X84ZN",
				Restart: &MachineRestart{
					Policy: "no",
				},
			},
		})
		if err != nil {
			log.Fatal(err)
		}
		log.Printf("Made a machine: %v", logger.AsJSON(m))
	}

	machines := must.Get(c.ListMachines(ctx))
	log.Printf("%d machines: ", len(machines))
	for i, m := range machines {
		j, _ := json.MarshalIndent(m, "", "  ")
		log.Printf("[%d] %s\n", i, j)
		if *startAll {
			err := c.StartMachine(ctx, m.ID)
			log.Printf("start of %v: %v", m.ID, err)
		}
		if *stopAll {
			err := c.StopMachine(ctx, m.ID, nil)
			log.Printf("stop of %v: %v", m.ID, err)
		}
		if *deleteAll {
			err := c.DeleteMachine(ctx, m.ID)
			log.Printf("delete of %v: %v", m.ID, err)
		}
	}
}

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
		req.Write(os.Stderr)
		res.Write(os.Stderr)
		return errors.New(res.Status)
	}
	return nil
}
