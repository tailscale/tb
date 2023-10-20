// The tb command is the Tailscale Build server controller, cache, and frontend.
//
// It runs in a Fly app that has access to secrets (Fly API token, GitHub Apps
// private key, Tailscale auth key) and creates Fly Machines in another app
// (that does not have access to secrets) where builds are run.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/tailscale/tb/tb/tbtype"
	"tailscale.com/tsnet"
	"tailscale.com/util/cmpx"
	"tailscale.com/util/rands"
)

var (
	stateDir  = flag.String("state", "/persist/tsnet", "state directory")
	cacheDir  = flag.String("cache", "/persist/cache", "cache directory")
	workerApp = flag.String("worker-app", cmpx.Or(os.Getenv("TB_WORKER_APP"), "tb-worker-no-secrets"), "the untrusted, secret-less Fly app in which to create machines for CI builds")
)

type Controller struct {
	cacheRoot string
	gitDir    string // under cache

	ts *tsnet.Server
}

func main() {
	flag.Parse()
	if strings.HasPrefix(*stateDir, *cacheDir) {
		log.Fatalf("state and cache directories must be different")
	}

	c := &Controller{
		cacheRoot: *cacheDir,
		gitDir:    filepath.Join(*cacheDir, "git"),
	}

	if err := os.MkdirAll(c.gitDir, 0755); err != nil {
		log.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(c.gitDir, ".git")); err != nil {
		cmd := exec.Command("git", "init")
		cmd.Dir = c.gitDir
		out, err := cmd.CombinedOutput()
		if err != nil {
			log.Fatalf("git init: %v\n%s", err, out)
		}
	}

	c.ts = &tsnet.Server{
		Dir:      *stateDir,
		Hostname: "tb",
	}
	ctx := context.Background()
	if _, err := c.ts.Up(ctx); err != nil {
		log.Fatal(err)
	}

	ln, err := c.ts.Listen("tcp", ":80")
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("Listening on %v", ln.Addr())

	errc := make(chan error)
	go func() {
		errc <- fmt.Errorf("tsnet.Serve: %w", http.Serve(ln, http.HandlerFunc(c.ServeTSNet)))
	}()
	go func() {
		errc <- fmt.Errorf("http.Serve: %w", http.ListenAndServe(":8080", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {

			fmt.Fprintf(w, "Hello, world (from 8080)")
		})))
	}()

	log.Fatal(<-errc)
}

func (c *Controller) ServeTSNet(w http.ResponseWriter, r *http.Request) {
	if r.Method == "POST" {
		switch r.RequestURI {
		case "/fetch":
			c.serveFetch(w, r)
			return
		default:
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
	}
	switch r.RequestURI {
	default:
		http.Error(w, "not found", http.StatusNotFound)
		return
	case "/stats":
		c.serveStats(w, r)
	case "/":
		fmt.Fprintf(w, `<html><body><h1>Tailscale Build</h1><a href='/stats'>stats</a>
		<form method=POST action=/fetch>Ref: <input name=ref><input type=submit name="fetch"></form>
		`)
	}
}

func (c *Controller) serveStats(w http.ResponseWriter, r *http.Request) {
	cmd := exec.Command("du", "-h")
	cmd.Dir = c.cacheRoot
	out, err := cmd.CombinedOutput()
	if err != nil {
		http.Error(w, fmt.Sprintf("du -h: %v\n%s", err, out), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Write(out)
}

func (c *Controller) serveJSON(w http.ResponseWriter, statusCode int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(statusCode)
	e := json.NewEncoder(w)
	e.SetIndent("", "\t")
	e.Encode(v)
}

func (c *Controller) serveFetch(w http.ResponseWriter, r *http.Request) {
	ref := r.FormValue("ref")
	res := &tbtype.FetchResponse{
		RemoteRef: ref,
	}
	if strings.ContainsAny(ref, " \t\n\r\"'|") {
		res.Error = "bad ref"
		c.serveJSON(w, http.StatusBadRequest, res)
		return
	}
	localRef := fmt.Sprintf("fetch-ref-%v-%v", time.Now().UnixNano(), rands.HexString(10))
	cmd := exec.Command("git", "fetch", "-f", "https://github.com/tailscale/tailscale", ref+":"+localRef)
	cmd.Dir = c.gitDir
	out, err := cmd.CombinedOutput()
	if err != nil {
		res.Error = fmt.Sprintf("git fetch: %v\n%s", err, out)
		c.serveJSON(w, http.StatusInternalServerError, res)
		return
	}
	res.LocalRef = localRef

	cmd = exec.Command("git", "rev-parse", localRef)
	cmd.Dir = c.gitDir
	out, err = cmd.CombinedOutput()
	if err != nil {
		res.Error = fmt.Sprintf("git rev-parse: %v\n%s", err, out)
		c.serveJSON(w, http.StatusInternalServerError, res)
		return
	}
	res.Hash = strings.TrimSpace(string(out))

	c.serveJSON(w, http.StatusOK, res)
}
