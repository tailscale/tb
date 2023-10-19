// The tb command is the Tailscale Build server controller, cache, and frontend.
//
// It runs in a Fly app that has access to secrets (Fly API token, GitHub Apps
// private key, Tailscale auth key) and creates Fly Machines in another app
// (that does not have access to secrets) where builds are run.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"

	"tailscale.com/tsnet"
	"tailscale.com/util/cmpx"
)

var (
	stateDir  = flag.String("state", "/persist/tsnet", "state directory")
	cacheDir  = flag.String("cache", "/persist/cache", "cache directory")
	workerApp = flag.String("worker-app", cmpx.Or(os.Getenv("TB_WORKER_APP"), "tb-worker-no-secrets"), "the untrusted, secret-less Fly app in which to create machines for CI builds")
)

func main() {
	flag.Parse()
	if strings.HasPrefix(*stateDir, *cacheDir) {
		log.Fatalf("state and cache directories must be different")
	}
	s := &tsnet.Server{
		Dir:      *stateDir,
		Hostname: "tb",
	}
	ctx := context.Background()
	if _, err := s.Up(ctx); err != nil {
		log.Fatal(err)
	}

	ln, err := s.Listen("tcp", ":80")
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("Listening on %v", ln.Addr())

	http.Serve(ln, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {

		fmt.Fprintf(w, "Hello, world")
	}))
}
