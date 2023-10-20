// The tb command is the Tailscale Build server controller, cache, and frontend.
//
// It runs in a Fly app that has access to secrets (Fly API token, GitHub Apps
// private key, Tailscale auth key) and creates Fly Machines in another app
// (that does not have access to secrets) where builds are run.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/tailscale/tb/fly"
	"github.com/tailscale/tb/tb/tbtype"
	"tailscale.com/tsnet"
	"tailscale.com/types/logger"
	"tailscale.com/util/cmpx"
	"tailscale.com/util/rands"
)

var (
	stateDir  = flag.String("state", "/persist/tsnet", "state directory")
	cacheDir  = flag.String("cache", "/persist/cache", "cache directory")
	workerApp = flag.String("worker-app", cmpx.Or(os.Getenv("TB_WORKER_APP"), "tb-no-secrets"), "the untrusted, secret-less Fly app in which to create machines for CI builds")
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
		errc <- fmt.Errorf("http.Serve: %w", http.ListenAndServe(":8080", http.HandlerFunc(c.Serve6PN)))
	}()

	log.Fatal(<-errc)
}

// Serve6PN serves 6PN clients (over port 8080 on the Fly private IPv6 network
// within the org, from the untrusted work app). These requests should be
// assumed to be suspect.
func (c *Controller) Serve6PN(w http.ResponseWriter, r *http.Request) {
	if r.Method == "GET" && r.URL.Path == "/archive" {
		c.serveArchive(w, r)
		return
	}
}

// ServeTSNet serves tsnet clients (over Tailscale).
func (c *Controller) ServeTSNet(w http.ResponseWriter, r *http.Request) {
	if r.Method == "POST" {
		switch r.URL.Path {
		case "/fetch":
			c.serveFetch(w, r)
			return
		case "/start-build":
			c.serveStartBuild(w, r)
			return
		default:
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
	}
	switch r.URL.Path {
	default:
		http.Error(w, "not found", http.StatusNotFound)
		return
	case "/archive":
		c.serveArchive(w, r)
	case "/stats":
		c.serveStats(w, r)
	case "/":
		fmt.Fprintf(w, `<html><body><h1>Tailscale Build</h1><a href='/stats'>stats</a>
		<form method=POST action=/fetch>Ref: <input name=ref><input type=submit value="fetch"></form>
		<form method=POST action=/start-build>Ref: <input name=ref><input type=submit value="start"></form>
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

func (c *Controller) fetch(ref string) (*tbtype.FetchResponse, error) {
	if ref == "" {
		return nil, fmt.Errorf("missing ref")
	}
	if strings.ContainsAny(ref, " \t\n\r\"'|") {
		return nil, fmt.Errorf("bad ref")
	}
	localRef := fmt.Sprintf("fetch-ref-%v-%v", time.Now().UnixNano(), rands.HexString(10))
	cmd := exec.Command("git", "fetch", "-f", "https://github.com/tailscale/tailscale", ref+":"+localRef)
	cmd.Dir = c.gitDir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("git fetch: %v\n%s", err, out)
	}
	cmd = exec.Command("git", "rev-parse", localRef)
	cmd.Dir = c.gitDir
	out, err = cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("git rev-parse: %v\n%s", err, out)
	}
	return &tbtype.FetchResponse{
		RemoteRef: ref,
		LocalRef:  localRef,
		Hash:      strings.TrimSpace(string(out)),
	}, nil
}

func (c *Controller) serveFetch(w http.ResponseWriter, r *http.Request) {
	ref := r.FormValue("ref")
	res, err := c.fetch(ref)
	if err != nil {
		c.serveJSON(w, http.StatusInternalServerError, &tbtype.FetchResponse{
			RemoteRef: ref,
			Error:     err.Error(),
		})
		return
	}
	c.serveJSON(w, http.StatusOK, res)
}

var hashRx = regexp.MustCompile(`^[0-9a-f]{40}$`)

func (c *Controller) serveArchive(w http.ResponseWriter, r *http.Request) {
	hash := r.FormValue("hash")
	if !hashRx.MatchString(hash) {
		http.Error(w, "bad 'hash'; want 40 lowercase hex", http.StatusBadRequest)
		return
	}
	// TODO(bradfitz): also require a local-ref and make sure its rev-parse
	// matches the hash. The randomness in the local-ref (and that it times out)
	// then also serves as a time-limited capability token that we give out,
	// preventing untrusted workers from getting any hash they know about.
	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s.tar.gz"`, hash))
	cmd := exec.Command("git", "archive", "--format=tar.gz", hash)
	cmd.Dir = c.gitDir
	cmd.Stdout = w
	var errBuf bytes.Buffer
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		http.Error(w, fmt.Sprintf("git archive: %v\n%s", err, errBuf.Bytes()), http.StatusInternalServerError)
		return
	}
}

func (c *Controller) serveStartBuild(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	ref := r.FormValue("ref")
	fetchRes, err := c.fetch(ref)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	fc := &fly.Client{
		App:   *workerApp,
		Token: os.Getenv("FLY_TOKEN"),
	}
	mm, err := fc.ListMachines(ctx)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	var base *fly.Machine
	for _, m := range mm {
		if strings.HasPrefix(m.Name, "base-") && (base == nil || m.CreatedAt > base.CreatedAt) {
			base = m
		}
	}
	if base == nil {
		http.Error(w, "no base machine", http.StatusInternalServerError)
		return
	}
	if base.Config == nil || base.Config.Image == "" {
		http.Error(w, "no base config image machine", http.StatusInternalServerError)
		return
	}
	fmt.Fprintf(w, "<html><body>base machine: <pre>%v</pre>", logger.AsJSON(base))
	w.(http.Flusher).Flush()

	m, err := fc.CreateMachine(ctx, &fly.CreateMachineRequest{
		Region: "sea",
		Config: &fly.MachineConfig{
			AutoDestroy: true,
			Env: map[string]string{
				"VM_MAX_DURATION": "5m",
			},
			Guest: &fly.MachineGuest{
				MemoryMB: 8192, // required 2048 per core?
				CPUs:     4,
				CPUKind:  "performance",
			},
			Image: base.Config.Image,
			Restart: &fly.MachineRestart{
				Policy: "no",
			},
		},
	})
	if err != nil {
		http.Error(w, "CreateMachine: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer func() {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			err := fc.StopMachine(ctx, m.ID, &fly.StopParam{Signal: "kill", Timeout: time.Nanosecond})
			log.Printf("stop machine = %v", err)
			err = fc.DeleteMachine(ctx, m.ID)
			log.Printf("delete machine = %v", err)
		}()
	}()

	wc := &WorkerClient{m: m, c: c, hash: fetchRes.Hash}
	if err := wc.WaitUp(15 * time.Second); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	io.WriteString(w, "made "+string(m.ID)+"\n")
	w.(http.Flusher).Flush()

	tgzURL := fmt.Sprintf("http://[%s]:8080/archive?hash=%s&ref=%s",
		os.Getenv("FLY_PRIVATE_IP"), fetchRes.Hash, url.QueryEscape(fetchRes.LocalRef))

	if err := wc.PushTreeFromURL(ctx, "", tgzURL); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if err := wc.Test(ctx, w, "tailscale.com/util/lru"); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
}

type WorkerClient struct {
	m    *fly.Machine
	c    *Controller
	hash string
}

func (c *WorkerClient) WaitUp(d time.Duration) error {
	// TODO(bradfitz): this a little trashy. we could also use the
	// https://docs.machines.dev/swagger/index.html#/Machines/Machines_wait call
	// first before probing the /gen204.
	deadline := time.Now().Add(d)
	hc := &http.Client{Timeout: 2 * time.Second}
	for time.Now().Before(deadline) {
		res, _ := hc.Get("http://[" + c.m.PrivateIP.String() + "]:8080/gen204")
		if res != nil && res.StatusCode == http.StatusNoContent {
			return nil
		}
		if res != nil {
			res.Body.Close()
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("machine didn't come up in %v", d)
}

func (c *WorkerClient) PushTreeFromURL(ctx context.Context, dir, tgzURL string) error {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "POST", "http://["+c.m.PrivateIP.String()+"]:8080/put", strings.NewReader((url.Values{
		"url": {tgzURL},
		"dir": {dir},
	}).Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return fmt.Errorf("pushing tarball: %v", res.Status)
	}
	return nil
}

func (c *WorkerClient) Test(ctx context.Context, w io.Writer, pkg string) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", "http://["+c.m.PrivateIP.String()+"]:8080/test/"+pkg, nil)
	if err != nil {
		return err
	}

	cmd := exec.Command("git", "show", c.hash+":go.toolchain.rev")
	cmd.Dir = c.c.gitDir
	toolChain, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("looking up go.toolchain.rev for %v: %v", c.hash, err)
	}

	// TODO(bradfitz): remove these. This was a dead end experiment
	// for a throway branch. What we really need to do is send over
	// a tarball of a .git directory shallow clone checked out.
	// e.g. git clone file:///Users/bradfitz/src/tailscale.com --depth=1 foo
	// in foo along with its .foo/git. That ~doubles the size of the tarball
	// but that's kinda tolerable for now.
	req.Header.Add("Test-Env", "GOCROSS_WANTVER="+c.hash)
	req.Header.Add("Test-Env", "GO_TOOLCHAIN_REV="+strings.TrimSpace(string(toolChain)))

	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return fmt.Errorf("pushing tarball: %v", res.Status)
	}
	io.Copy(w, res.Body)
	return nil
}
