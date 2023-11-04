// The pilot command flies Fly.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/tailscale/tb/fly"
	"tailscale.com/types/logger"
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

	c := &fly.Client{
		App: *app,
		//Token: strings.TrimSpace(string(must.Get(exec.Command("flyctl", "auth", "token").Output()))),
		Token: strings.TrimSpace(string(must.Get(os.ReadFile(filepath.Join(os.Getenv("HOME"), "keys", "fly-ci-token"))))),
	}
	ctx := context.Background()

	if os.Getenv("VOLTEST") == "1" {
		v, err := c.CreateVolume(ctx, &fly.CreateVolumeRequest{
			Name:              "tbw_vol01",
			SizeGB:            10,
			Region:            "sea",
			RequireUniqueZone: true,
		})
		if err != nil {
			log.Fatal(err)
		}
		log.Printf("Made a volume: %v", logger.AsJSON(v))

		if err := c.DeleteVolume(ctx, v.ID); err != nil {
			log.Fatalf("DeleteVolume: %v", err)
		}
		log.Printf("and deleted it.")
		// {"created_at":"2023-11-04T16:19:03.89Z","encrypted":true,"id":"vol_nvxywlzzl7l72884","name":"tbw_vol01","region":"phx","size_gb":10,"snapshot_retention":5,"state":"created","zone":"4514"}
		return
	}

	if *create {
		m, err := c.CreateMachine(ctx, &fly.CreateMachineRequest{
			Region: "sea",
			Config: &fly.MachineConfig{
				AutoDestroy: true,
				Env: map[string]string{
					"FOO_BAR_ENV": "baz",
				},
				Image: "registry.fly.io/tb-no-secrets:deployment-01HD6ZCSWVFAADHDDF4CJZKHAR",
				Restart: &fly.MachineRestart{
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
