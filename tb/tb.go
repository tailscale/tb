// The tb command is the Tailscale Build server controller, cache, and frontend.
//
// It runs in a Fly app that has access to secrets (Fly API token, GitHub Apps
// private key, Tailscale auth key) and creates Fly Machines in another app
// (that does not have access to secrets) where builds are run.
package main

/*

TODO:
- set CI:true env var
- record time traces (in Go format? can we exclude built-in trace info?)

*/

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"html"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/tailscale/tb/fly"
	"github.com/tailscale/tb/tb/tbtype"
	"tailscale.com/syncs"
	"tailscale.com/tsnet"
	"tailscale.com/types/logger"
	"tailscale.com/util/cmpx"
	"tailscale.com/util/mak"
	"tailscale.com/util/rands"
)

var (
	stateDir    = flag.String("state", "/persist/tsnet", "state directory")
	cacheDir    = flag.String("cache", "/persist/cache", "cache directory")
	workerApp   = flag.String("worker-app", cmpx.Or(os.Getenv("TB_WORKER_APP"), "tb-no-secrets"), "the untrusted, secret-less Fly app in which to create machines for CI builds")
	maxMachines = flag.Int("max-machines", 500, "maximum number of machines to have running at once")
)

type Controller struct {
	cacheRoot string
	gitDir    string // under cache

	ts *tsnet.Server
	fc *fly.Client

	machineSem syncs.Semaphore

	mu   sync.Mutex
	runs map[string]*Run
}

func main() {
	flag.Parse()
	if strings.HasPrefix(*stateDir, *cacheDir) {
		log.Fatalf("state and cache directories must be different")
	}

	c := &Controller{
		cacheRoot:  *cacheDir,
		gitDir:     filepath.Join(*cacheDir, "git"),
		machineSem: syncs.NewSemaphore(*maxMachines),
		fc: &fly.Client{
			App:   *workerApp,
			Token: os.Getenv("FLY_TOKEN"),
		},
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
	case "/run":
		c.serveRun(w, r)
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

func (c *Controller) registerRun(r *Run) {
	c.mu.Lock()
	defer c.mu.Unlock()
	mak.Set(&c.runs, r.id, r)
}

func (c *Controller) unregisterRun(r *Run) {
	// TODO: demote to some other set that serveRun also looks up, but only keep
	// the most recent N hours or M items in that other set?
}

func (c *Controller) serveRun(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	run, ok := c.runs[r.FormValue("id")]
	c.mu.Unlock()
	if !ok {
		http.Error(w, "no such run, or long expired", http.StatusNotFound)
		return
	}

	run.mu.Lock()
	defer run.mu.Unlock()

	fmt.Fprintf(w, "<html><body><h1>run %v</h1>", run.id)

	if run.doneAt.IsZero() {
		fmt.Fprintf(w, "<p>running for %v</p>", time.Since(run.createdAt))
	} else {
		var errMsg string
		if run.err != nil {
			errMsg = run.err.Error()
		}
		fmt.Fprintf(w, "<p>done in %v: %v</p>", run.doneAt.Sub(run.createdAt), html.EscapeString(errMsg))
	}

	fmt.Fprintf(w, "<hr><pre>\n%s\n</pre>", html.EscapeString(run.buf.String()))
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
	localRef := fmt.Sprintf("tempref-handle-%v-%v", time.Now().UnixNano(), rands.HexString(16))
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

var (
	hashRx    = regexp.MustCompile(`^[0-9a-f]{40}$`)
	tempRefRx = regexp.MustCompile(`^tempref-handle-\d+-[0-9a-f]+$`)
)

func (c *Controller) serveArchive(w http.ResponseWriter, r *http.Request) {
	hash := r.FormValue("hash")
	if !hashRx.MatchString(hash) {
		http.Error(w, "bad 'hash'; want 40 lowercase hex", http.StatusBadRequest)
		return
	}
	// We also require a local-ref and make sure its rev-parse matches the hash.
	// The randomness in the local-ref (and that it times out) then also serves
	// as a time-limited capability token that we give out, preventing untrusted
	// workers from getting any hash they know about.
	ref := r.FormValue("ref")
	if !tempRefRx.MatchString(ref) {
		http.Error(w, "bad 'ref'; want a tempref-handle from fetch", http.StatusBadRequest)
		return
	}

	cmd := exec.Command("git", "rev-parse", ref)
	cmd.Dir = c.gitDir
	out, err := cmd.CombinedOutput()
	if err != nil {
		http.Error(w, fmt.Sprintf("git rev-parse: %v\n%s", err, out), http.StatusInternalServerError)
		return
	}
	if strings.TrimSpace(string(out)) != hash {
		http.Error(w, "ref handle doesn't match provided hash", http.StatusBadRequest)
		return
	}

	// TODO(bradfitz): at this point, check a cache (memory is probably
	// sufficient, LRU of a dozen tarballs) and see if we've already made this
	// tarball.

	td, err := os.MkdirTemp("", "tbarchive-*")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer os.RemoveAll(td)

	shallowClone := filepath.Join(td, "shallow")
	// git clone --depth=1 --single-branch --branch=foo file:///Users/bradfitz/src/tailscale.com /tmp/git/lil2
	cmd = exec.Command("git", "clone",
		"--depth=1",
		"--single-branch",
		"--branch="+ref,
		"file://"+c.gitDir,
		shallowClone)
	cmd.Dir = c.gitDir
	if out, err := cmd.CombinedOutput(); err != nil {
		http.Error(w, fmt.Sprintf("git clone: %v\n%s", err, out), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s.tar.gz"`, hash))
	cmd = exec.Command("tar", "-zcf", "-", ".")
	cmd.Dir = shallowClone
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

	mm, err := c.fc.ListMachines(ctx)
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

	run := &Run{
		c:         c,
		id:        rands.HexString(32),
		fetch:     fetchRes,
		base:      base,
		createdAt: time.Now(),
	}
	run.ctx, run.cancel = context.WithTimeout(context.Background(), 30*time.Minute)
	c.registerRun(run)
	go run.Run()

	http.Redirect(w, r, "/run?id="+run.id, http.StatusFound)
	fmt.Fprintf(w, "<html><body>base machine: <pre>%v</pre>", logger.AsJSON(base))

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

func (c *WorkerClient) CheckGo(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", "http://["+c.m.PrivateIP.String()+"]:8080/check/go-version", nil)
	if err != nil {
		return "", err
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return "", fmt.Errorf("pushing tarball: %v", res.Status)
	}
	all, err := io.ReadAll(res.Body)
	return strings.TrimSpace(string(all)), err
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

type Run struct {
	ctx    context.Context
	cancel context.CancelFunc

	c         *Controller
	id        string // rand hex
	fetch     *tbtype.FetchResponse
	base      *fly.Machine
	createdAt time.Time

	mu     sync.Mutex
	doneAt time.Time
	err    error
	buf    bytes.Buffer
}

func (r *Run) Run() {
	err := r.run()
	defer r.c.unregisterRun(r)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.doneAt = time.Now()
	r.err = err
}

func (r *Run) Write(p []byte) (n int, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.buf.Write(p)
}

func (r *Run) run() error {
	fc := r.c.fc
	m, err := fc.CreateMachine(r.ctx, &fly.CreateMachineRequest{
		Region: "sea",
		Config: &fly.MachineConfig{
			AutoDestroy: true,
			Env: map[string]string{
				"VM_MAX_DURATION": "5m",
			},
			Guest: &fly.MachineGuest{
				MemoryMB: 8192, // required 2048 per core for performance
				CPUs:     4,
				CPUKind:  "performance",
			},
			Image: r.base.Config.Image,
			Restart: &fly.MachineRestart{
				Policy: "no",
			},
		},
	})
	if err != nil {
		return fmt.Errorf("CreateMachine: %w", err)
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

	wc := &WorkerClient{m: m, c: r.c, hash: r.fetch.Hash}
	if err := wc.WaitUp(15 * time.Second); err != nil {
		return fmt.Errorf("WorkerClient.WaitUp = %w", err)
	}

	t0 := time.Now()
	fmt.Fprintf(r, "Pushing tree...\n")
	tgzURL := fmt.Sprintf("http://[%s]:8080/archive?hash=%s&ref=%s",
		os.Getenv("FLY_PRIVATE_IP"), r.fetch.Hash, url.QueryEscape(r.fetch.LocalRef))

	if err := wc.PushTreeFromURL(r.ctx, "", tgzURL); err != nil {
		return fmt.Errorf("PushTreeFromURL = %w", err)
	}
	fmt.Fprintf(r, "Pushed tree in %v\n", time.Since(t0))

	t0 = time.Now()
	v, err := wc.CheckGo(r.ctx)
	fmt.Fprintf(r, "CheckGo = %q, %v in %v\n", v, err, time.Since(t0))

	for _, pkg := range []string{"tailscale.com/util/lru", "tailscale.com/util/cmpx"} {
		t0 := time.Now()
		err := wc.Test(r.ctx, r, pkg)
		fmt.Fprintf(r, "test %v = %v, in %v\n", pkg, err, time.Since(t0))
	}

	return nil
}
