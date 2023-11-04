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

% git grep -l -e '^func Test' $(git rev-parse HEAD)
bb93ec5d320917178637529a97f9a27610ca6446:appc/appc_test.go
bb93ec5d320917178637529a97f9a27610ca6446:appc/handlers_test.go
bb93ec5d320917178637529a97f9a27610ca6446:atomicfile/atomicfile_test.go
...

*/

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"expvar"
	"flag"
	"fmt"
	"html"
	"io"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bradfitz/go-tool-cache/cachers"
	"github.com/goproxy/goproxy"
	"github.com/tailscale/tb/fly"
	"github.com/tailscale/tb/tb/tbtype"
	"go4.org/mem"
	"golang.org/x/sync/errgroup"
	"tailscale.com/syncs"
	"tailscale.com/tsnet"
	"tailscale.com/tsweb"
	"tailscale.com/util/cmpx"
	"tailscale.com/util/mak"
	"tailscale.com/util/rands"
	"tailscale.com/util/set"
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

	ts       *tsnet.Server
	fc       *fly.Client
	tsnetMux *http.ServeMux

	machineSem syncs.Semaphore

	mu         sync.Mutex
	runs       map[string]*Run
	aliveChans map[string]chan bool
}

func main() {
	flag.Parse()
	if strings.HasPrefix(*stateDir, *cacheDir) {
		log.Fatalf("state and cache directories must be different")
	}

	goCacheDir := filepath.Join(*cacheDir, "gocache")
	if err := os.MkdirAll(goCacheDir, 0755); err != nil {
		log.Fatal(err)
	}
	goCache := &goCacheServer{
		cache: &cachers.DiskCache{Dir: goCacheDir},
	}

	goProxyCacheDir := filepath.Join(*cacheDir, "goproxy")
	if err := os.MkdirAll(goProxyCacheDir, 0755); err != nil {
		log.Fatal(err)
	}
	goProxyHandler := &goproxy.Goproxy{
		GoBinName: "/usr/bin/false", // should never be used
		GoBinEnv: []string{
			"GOPROXY=https://proxy.golang.org",
		},
		Cacher:              goProxyMetricTrackingCacher{goproxy.DirCacher(goProxyCacheDir)},
		CacherMaxCacheBytes: 20 << 30,
		Transport:           goProxyMetricTrackingRoundTripper{http.DefaultTransport},
		ErrorLogger:         log.Default(),
	}

	c := &Controller{
		cacheRoot:  *cacheDir,
		gitDir:     filepath.Join(*cacheDir, "git"),
		machineSem: syncs.NewSemaphore(*maxMachines),
		tsnetMux:   http.NewServeMux(),
		fc: &fly.Client{
			App:   *workerApp,
			Token: os.Getenv("FLY_TOKEN"),
			Base:  "http://_api.internal:4280",
		},
	}
	debugger := tsweb.Debugger(c.tsnetMux)
	_ = debugger
	c.tsnetMux.Handle("/", http.HandlerFunc(c.ServeTSNet))

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
		errc <- fmt.Errorf("tsnet.Serve: %w", http.Serve(ln, c.tsnetMux))
	}()
	go func() {
		errc <- fmt.Errorf("http.Serve: %w", http.ListenAndServe(":8080", http.HandlerFunc(c.Serve6PN)))
	}()
	go func() {
		errc <- fmt.Errorf("http.Serve(gocache 8081): %w", http.ListenAndServe(":8081", goCache))
	}()
	go func() {
		errc <- fmt.Errorf("goproxy: %w", http.ListenAndServe(":8082", goProxyHandler))
	}()

	log.Fatal(<-errc)
}

var (
	metricWorkAliveOK      = expvar.NewInt("counter_worker_alive_ok")
	metricWorkAliveMiss    = expvar.NewInt("counter_worker_alive_miss")
	metricWorkerWaitChanOK = expvar.NewInt("counter_worker_wait_chan_ok")
	metricWorkerWaitPollOK = expvar.NewInt("counter_worker_wait_poll_ok")
	metricCounterArchives  = expvar.NewInt("counter_archives")
	metricDeleteMachineErr = expvar.NewInt("counter_delete_machine_err")
	metricDeleteMachineOK  = expvar.NewInt("counter_delete_machine_ok")
)

// Serve6PN serves 6PN clients (over port 8080 on the Fly private IPv6 network
// within the org, from the untrusted work app). These requests should be
// assumed to be suspect.
func (c *Controller) Serve6PN(w http.ResponseWriter, r *http.Request) {
	if r.Method == "GET" {
		if machineRand, ok := strings.CutPrefix(r.URL.Path, "/worker-alive/"); ok {
			c := c.machineAliveChan(machineRand)
			if c != nil {
				select {
				case c <- true:
					metricWorkAliveOK.Add(1)
					return
				default:
				}
			}
			metricWorkAliveMiss.Add(1)
			w.WriteHeader(http.StatusNoContent)
			return
		}
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
	case "/du":
		c.serveDU(w, r)
	case "/run":
		c.serveRun(w, r)
	case "/":
		fmt.Fprintf(w, `<html><body><h1>Tailscale Build</h1>
		[<a href='/du'>du</a>] [<a href="/debug">debug</a>]
		<form method=POST action=/start-build>Test ref: <input name=ref>, <code>-j</code><input name=machines value=1 size=2>, package(s)/all: <input name=pkg> <input type=submit value="start"></form>
		<form method=POST action=/fetch>[debug] Fetch ref: <input name=ref><input type=submit value="fetch"></form>
		`)

	}
}

func (c *Controller) serveDU(w http.ResponseWriter, r *http.Request) {
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

// getMachine returns a future to an newly created machine.
// It doesn't take a context because if the caller goes away, we still want to
// wait for it to be created so we can either shut it down cleanly or give it out
// to somebody else who does want it.
func (c *Controller) getMachine(image string) *Lazy[*fly.Machine] {
	machineRand := rands.HexString(16)
	aliveCh := make(chan bool, 1)
	c.registerMachineAliveChan(machineRand, aliveCh)

	const minMemoryMBPerPerformanceCore = 2048
	const numCPU = 1

	return GetLazy(func() (*fly.Machine, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		return c.fc.CreateMachine(ctx, &fly.CreateMachineRequest{
			Region: "phx",
			Config: &fly.MachineConfig{
				AutoDestroy: true,
				Env: map[string]string{
					"VM_MAX_DURATION":    "20m",
					"REGISTER_ALIVE_URL": "http://[" + os.Getenv("FLY_PRIVATE_IP") + "]:8080/worker-alive/" + machineRand,
				},
				Guest: &fly.MachineGuest{
					MemoryMB: numCPU * minMemoryMBPerPerformanceCore,
					CPUs:     numCPU,
					CPUKind:  "performance",
				},
				Image: image,
				Restart: &fly.MachineRestart{
					Policy: "no",
				},
			},
		})
	})
}

func (c *Controller) returnMachine(m *fly.Machine) {
	// TOOD: pool, reuse? For now, just nuke them.
	go c.bestEffortDeleteMachine(m.ID)
}

func (c *Controller) registerMachineAliveChan(id string, ch chan bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	mak.Set(&c.aliveChans, id, ch)
}

func (c *Controller) machineAliveChan(id string) chan bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.aliveChans[id]
}

func (c *Controller) unregisterRun(r *Run) {
	// TODO: demote to some other set that serveRun also looks up, but only keep
	// the most recent N hours or M items in that other set?
}

func (c *Controller) findFlyWorkerImage(ctx context.Context) (image string, err error) {
	// TODO(bradfitz): cache. use last answer if in past minute or something.

	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	mm, err := c.fc.ListMachines(ctx)
	if err != nil {
		return "", err
	}
	var base *fly.Machine
	var allBase = set.Set[*fly.Machine]{}
	for _, m := range mm {
		if strings.HasPrefix(m.Name, "base-") {
			allBase.Add(m)
			if base == nil || m.CreatedAt > base.CreatedAt {
				base = m
			}
		}
	}
	if base == nil {
		return "", errors.New("no base machine images found")
	}
	if base.Config == nil || base.Config.Image == "" {
		return "", errors.New("base machine found has no config image")
	}
	// Clean up old base machines that aren't the latest.
	for m := range allBase {
		if m != base {
			log.Printf("deleting old base machine %v", m.ID)
			go c.bestEffortDeleteMachine(m.ID)
		}
	}
	return base.Config.Image, nil
}

func (c *Controller) serveRun(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	run, ok := c.runs[r.FormValue("id")]
	c.mu.Unlock()
	if !ok {
		http.Error(w, "no such run, or long expired", http.StatusNotFound)
		return
	}

	wantTask := -1 // means overview page; >=0 means view a task
	if v, err := strconv.Atoi(r.FormValue("task")); err == nil {
		wantTask = v
	}
	viewAll := wantTask == -1

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

	if viewAll {
		fmt.Fprintf(w, "<hr><pre>\n%s\n</pre><hr><h3>tasks</h3>", html.EscapeString(run.buf.String()))
	}

	for i, t := range run.req.Tasks {
		if !viewAll && i != wantTask {
			continue
		}
		fmt.Fprintf(w, "<p><b><a href='/run?id=%s&task=%d'>Task %s</a></b>", run.id, i, html.EscapeString(t.Name))
		if ts, ok := run.tasks[t]; ok {
			if !ts.endedAt.IsZero() {
				d := ts.endedAt.Sub(ts.startedAt)
				fmt.Fprintf(w, ", after %v: %v", d, html.EscapeString(fmt.Sprint(ts.finalErr)))
			} else {
				fmt.Fprintf(w, ", running for %v", time.Since(ts.startedAt))
			}
			if i == wantTask {
				fmt.Fprintf(w, "<pre>\n%s\n</pre>", html.EscapeString(ts.buf.String()))
			}
		} else {
			fmt.Fprintf(w, ", not started")
		}
		fmt.Fprintf(w, "</p>")
	}

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

func (c *Controller) bestEffortDeleteMachine(id fly.MachineID) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	err := c.fc.DeleteMachine(ctx, id)
	log.Printf("delete of machine %v: err=%v", id, err)
	if err != nil {
		metricDeleteMachineErr.Add(1)
		return
	}
	metricDeleteMachineOK.Add(1)
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

func (c *Controller) getArchive(hash, ref string) (mem.RO, error) {
	var zero mem.RO
	metricCounterArchives.Add(1)
	if !hashRx.MatchString(hash) {
		return zero, errors.New("bad 'hash'; want 40 lowercase hex")
	}
	// TODO: add more spans; move getArchive to method on Run instead?
	td, err := os.MkdirTemp("", "tbarchive-*")
	if err != nil {
		return zero, err
	}
	defer os.RemoveAll(td)

	shallowClone := filepath.Join(td, "shallow")
	cmd := exec.Command("git", "clone",
		"--depth=1",
		"--single-branch",
		"--branch="+ref,
		"file://"+c.gitDir,
		shallowClone)
	cmd.Dir = c.gitDir
	if out, err := cmd.CombinedOutput(); err != nil {
		return zero, fmt.Errorf("git clone: %w\n%s", err, out)
	}

	var buf bytes.Buffer
	cmd = exec.Command("tar", "-zcf", "-", ".")
	cmd.Dir = shallowClone
	cmd.Stdout = &buf
	var errBuf bytes.Buffer
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		return zero, fmt.Errorf("git archive: %v\n%s", err, errBuf.Bytes())
	}
	return mem.B(buf.Bytes()), nil
}

func (c *Controller) serveStartBuild(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	if r.Method != "POST" {
		http.Error(w, "bad method", http.StatusMethodNotAllowed)
		return
	}

	var req tbtype.BuildRequest
	if r.Header.Get("Content-Type") == "application/x-www-form-urlencoded" {
		req.Machines, _ = strconv.Atoi(r.FormValue("machines"))
		if req.Machines == 0 {
			req.Machines = 1
		}
		req.Ref = r.FormValue("ref")
		pkg := r.FormValue("pkg")
		switch pkg {
		case "":
			req.Tasks = []*tbtype.Task{
				{Name: "lru", GOOS: "linux", GOARCH: "amd64", Action: "test", Packages: []string{"tailscale.com/util/lru"}},
				{Name: "lru", GOOS: "linux", GOARCH: "amd64", Action: "test", Packages: []string{"tailscale.com/util/cmpx"}},
			}
		case "!netmesh":
			// Make a set of 1 Task that matches what the doingNetMeshTest method looks for.
			req.Tasks = []*tbtype.Task{
				{Name: "netmesh", Action: "netmesh"},
			}
		default:
			pkgs := strings.Fields(pkg)
			for _, pkg := range pkgs {
				if !strings.Contains(pkg, ".") {
					pkg = "tailscale.com/" + pkg
				}
				req.Tasks = append(req.Tasks, &tbtype.Task{
					Name: pkg, GOOS: "linux", GOARCH: "amd64", Action: "test", Packages: []string{pkg},
				})
			}
		}
	} else {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	}
	const maxMachines = 300
	if req.Machines == 0 {
		req.Machines = 1
	} else if req.Machines > maxMachines {
		req.Machines = maxMachines
	}

	// TODO(bradfitz): finish plumbing/using this goCache stuff
	goCache := r.FormValue("gocache") // "rw", "ro", or "" for none
	if req.Ref == "main" {
		goCache = "rw"
	}

	run := &Run{
		c:         c,
		req:       &req,
		id:        rands.HexString(32),
		createdAt: time.Now(),
		goCache:   goCache,
	}

	var workerImageCallDone = make(chan error, 1)
	go func() {
		s := run.startSpan("find-fly-worker-image")
		var err error
		run.workerImage, err = c.findFlyWorkerImage(ctx)
		s.end(err)
		workerImageCallDone <- err
	}()

	s := run.startSpan("fetch-ref")
	fetchRes, err := c.fetch(req.Ref)
	s.end(err)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	run.fetch = fetchRes

	run.filesTarball = GetLazy(func() (mem.RO, error) {
		s := run.startSpan("generate-files-tar")
		v, err := c.getArchive(fetchRes.Hash, fetchRes.LocalRef)
		s.end(err)
		return v, err
	})

	// Wait for the base image fetch to finish.
	select {
	case err := <-workerImageCallDone:
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	case <-ctx.Done():
		return
	}

	run.ctx, run.cancel = context.WithTimeout(context.Background(), 30*time.Minute)
	c.registerRun(run)
	go run.Run()

	http.Redirect(w, r, "/run?id="+run.id, http.StatusFound)
}

type WorkerClient struct {
	m    *fly.Machine
	c    *Controller
	hash string
}

func (c *WorkerClient) WaitUp(d time.Duration) error {
	_, machineRand, ok := strings.Cut(c.m.Config.Env["REGISTER_ALIVE_URL"], "/worker-alive/")
	if !ok || machineRand == "" {
		return errors.New("failed to find machinerand in machine env")
	}

	donec := make(chan struct{})
	defer close(donec)
	alive := c.c.machineAliveChan(machineRand)
	timer := time.NewTimer(d)
	defer timer.Stop()

	upByPoll := make(chan struct{})
	go func() {
		hc := &http.Client{Timeout: 2 * time.Second}
		for {
			res, _ := hc.Get("http://[" + c.m.PrivateIP.String() + "]:8080/gen204")
			if res != nil {
				res.Body.Close()
				if res.StatusCode == http.StatusNoContent {
					close(upByPoll)
					return
				}
			}
			select {
			case <-donec:
				return
			case <-time.After(time.Second):
			}
		}
	}()

	select {
	case <-alive:
		metricWorkerWaitChanOK.Add(1)
		return nil
	case <-upByPoll:
		metricWorkerWaitPollOK.Add(1)
		return nil
	case <-timer.C:
		return errors.New("timeout waiting for machine to come up")
	}
}

func (c *WorkerClient) PushTreeFromReader(ctx context.Context, dir string, r io.Reader) error {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "PUT",
		"http://["+c.m.PrivateIP.String()+"]:8080/put?dir="+url.QueryEscape(dir),
		r)
	if err != nil {
		return err
	}
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

func cacheServerAddr() string {
	return "http://" + os.Getenv("FLY_REGION") + "." + os.Getenv("FLY_APP_NAME") + ".internal:8081"
}

type ExecOpt struct {
	GoCache string // "rw", "ro", or ""
}

var zeroOpt = new(ExecOpt)

func (c *WorkerClient) CheckGo(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	var stdout, stderr bytes.Buffer
	err := c.Exec(ctx, &stdout, &stderr, &tbtype.ExecRequest{
		Cmd:  "./tool/go",
		Args: []string{"version"},
	})
	if err != nil {
		return "", fmt.Errorf("exec: %v, %s", err, stderr.Bytes())
	}
	return strings.TrimSpace(stdout.String()), err
}

// Exec executes req remotely.
//
// req.TimeoutSeconds doesn't need to be set. The context is also used.
//
// If stderr or stdout is nil, that output stream is ignored.
func (c *WorkerClient) Exec(ctx context.Context, stdout, stderr io.Writer, param *tbtype.ExecRequest) error {
	er := *param
	if stdout == nil {
		er.IgnoreStdout = true
	}
	if stderr == nil {
		er.IgnoreStderr = true
	}
	j, err := json.Marshal(&er)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", "http://["+c.m.PrivateIP.String()+"]:8080/exec", bytes.NewReader(j))
	if err != nil {
		return err
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return fmt.Errorf("exec %v %v: %v", er.Cmd, er.Args, res.Status)
	}
	d := json.NewDecoder(res.Body)
	for {
		var es tbtype.ExecStream
		if err := d.Decode(&es); err != nil {
			return err
		}
		if es.Exit != nil {
			if *es.Exit == 0 {
				return nil
			}
			return fmt.Errorf("exit status: %v, %v", *es.Exit, es.Err)
		}
		if es.O != "" {
			es.OB = []byte(es.O)
		}
		if es.E != "" {
			es.EB = []byte(es.E)
		}
		if es.OB != nil && stdout != nil {
			stdout.Write(es.OB)
		}
		if es.EB != nil && stderr != nil {
			stderr.Write(es.EB)
		}
	}

}

func (c *WorkerClient) Test(ctx context.Context, w io.Writer, pkg string) error {
	pkg, run, _ := strings.Cut(pkg, "!") // TODO(bradfitz): temporary hack to smuggle run arg
	const timeout = 15 * time.Minute
	ctx, cancel := context.WithTimeout(ctx, timeout+5*time.Second)
	defer cancel()

	stdoutRead, stdoutWrite := io.Pipe()
	var eg errgroup.Group
	eg.Go(func() error {
		d := json.NewDecoder(stdoutRead)
		for {
			var testEvent struct {
				Time    time.Time // encodes as an RFC3339-format string
				Action  string
				Package string
				Test    string
				Elapsed float64 // seconds
				Output  string
			}
			if err := d.Decode(&testEvent); err != nil {
				if errors.Is(err, io.EOF) {
					return nil
				}
				return err
			}
			if testEvent.Output != "" {
				io.WriteString(w, testEvent.Output)
				continue
			}
		}
	})

	defer func() {
		if err := eg.Wait(); err != nil {
			fmt.Fprintf(w, "stdout JSON reading error: %v", err)
		}
	}()
	defer stdoutWrite.Close()

	return c.Exec(ctx, stdoutWrite, w, &tbtype.ExecRequest{
		Dir:  "",
		Cmd:  "./tool/go",
		Args: []string{"test", "-json", "-v", "-run=" + run, pkg},
		Env: []string{
			"GOPROXY=http://[" + os.Getenv("FLY_PRIVATE_IP") + "]:8082",
			fmt.Sprintf("GOCACHEPROG=/usr/local/bin/go-cacher --verbose=%v --cache-server=%s --cache-dir=/home/workdir/.cache/go-cacher", false, cacheServerAddr()),
		},
		TimeoutSeconds: timeout.Seconds(),
	})
}

type Run struct {
	ctx    context.Context
	cancel context.CancelFunc

	req     *tbtype.BuildRequest
	goCache string // "rw", "ro", or "

	c         *Controller
	id        string // rand hex
	fetch     *tbtype.FetchResponse
	base      *fly.Machine
	createdAt time.Time

	// populated during run, owned by run's goroutine
	workerImage      string
	toolchain        string
	toolChainTarball *Lazy[mem.RO]
	filesTarball     *Lazy[mem.RO]

	mu              sync.Mutex
	machinesStarted set.Set[*Lazy[*fly.Machine]]
	tasks           map[*tbtype.Task]*TaskStatus
	clients         set.Set[*WorkerClient]
	doneAt          time.Time
	err             error
	buf             bytes.Buffer
	spans           []*span
	spansOpen       int
}

type TaskStatus struct {
	r    *Run
	task *tbtype.Task

	// Mutable fields, guarded by r.mu:
	startedAt time.Time
	endedAt   time.Time
	finalErr  error
	buf       bytes.Buffer
}

func (ts *TaskStatus) Write(p []byte) (n int, err error) {
	ts.r.mu.Lock()
	defer ts.r.mu.Unlock()
	return ts.buf.Write(p)
}

func (r *Run) Run() {
	err := r.run()
	log.Printf("Run error: %v", err)
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

type span struct {
	name    string
	r       *Run
	startAt time.Time

	// Mutable fields, guarded by r.mu:
	endAt time.Time // or zero if still ru nning
	err   error
}

func (r *Run) runSpan(name string, f func() error) error {
	return r.startSpan(name).end(f())
}

func (r *Run) startSpan(name string) *span {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.startSpanLocked(name)
}

func (r *Run) startSpanLocked(name string) *span {
	s := &span{
		name:    name,
		r:       r,
		startAt: time.Now(),
	}
	r.spansOpen++
	r.spans = append(r.spans, s)
	fmt.Fprintf(&r.buf, "[+%10s] span %q started\n", s.startAt.Sub(r.createdAt).Round(time.Millisecond).String(), name)
	return s
}

func (s *span) end(err error) error {
	r := s.r
	r.mu.Lock()
	defer r.mu.Unlock()
	s.endAt = time.Now()
	s.err = err
	res := "ok"
	if err != nil {
		res = err.Error()
	}
	fmt.Fprintf(&r.buf, "[+%10s] span %q ended after %v: %v\n",
		s.endAt.Sub(r.createdAt).Round(time.Millisecond).String(),
		s.name,
		s.endAt.Sub(s.startAt).Round(time.Millisecond),
		res)
	r.spansOpen--
	if r.spansOpen == 0 {
		fmt.Fprintf(&r.buf, "--\n")
	}
	return err
}

func (r *Run) doingNetMeshTest() bool {
	return len(r.req.Tasks) == 1 && r.req.Tasks[0].Action == "netmesh"
}

func (r *Run) getUsableMachine() (_ *WorkerClient, retErr error) {
	r.mu.Lock()
	n := len(r.machinesStarted) + 1
	spanUsable := r.startSpanLocked(fmt.Sprintf("get-usable-machine-%d", n))
	defer func() { spanUsable.end(retErr) }()

	spanMachine := r.startSpanLocked(fmt.Sprintf("create-machine-%d", n))
	lm := r.c.getMachine(r.workerImage)
	if r.machinesStarted == nil {
		r.machinesStarted = set.Set[*Lazy[*fly.Machine]]{}
	}
	r.machinesStarted.Add(lm)
	r.mu.Unlock()

	m, err := lm.Get(r.ctx)
	spanMachine.end(err)
	if err != nil {
		return nil, err
	}

	wc := &WorkerClient{m: m, c: r.c, hash: r.fetch.Hash}
	spanReach := r.startSpan(fmt.Sprintf("wait-reachable-%d", n))
	if err := spanReach.end(wc.WaitUp(30 * time.Second)); err != nil {
		return nil, fmt.Errorf("WorkerClient.WaitUp = %w", err)
	}

	if r.doingNetMeshTest() {
		return wc, nil
	}

	var eg errgroup.Group
	eg.Go(func() error {
		if err := r.runSpan("push-files", func() error {
			ro, err := r.filesTarball.Get(r.ctx)
			if err != nil {
				return err
			}
			return wc.PushTreeFromReader(r.ctx, "code", mem.NewReader(ro))
		}); err != nil {
			return fmt.Errorf("PushTreeFromURL = %w", err)
		}
		return nil
	})
	eg.Go(func() error {
		if err := r.runSpan("push-go-toolchain", func() error {
			tgz, err := r.toolChainTarball.Get(r.ctx)
			if err != nil {
				return err
			}
			return wc.PushTreeFromReader(r.ctx, "tailscale-go/"+r.toolchain, mem.NewReader(tgz))
		}); err != nil {
			return fmt.Errorf("PushTreeFromReader = %w", err)
		}
		return nil
	})
	if err := eg.Wait(); err != nil {
		return nil, err
	}

	return wc, nil
}

func (r *Run) cleanup() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for lm := range r.machinesStarted {
		lm := lm
		go func() {
			if m, err := lm.Get(context.Background()); err == nil {
				r.c.returnMachine(m)
			}
		}()
	}
}

func (r *Run) run() error {
	defer r.cleanup()

	if r.doingNetMeshTest() {
		return r.runNetMeshTest()
	}

	cmd := exec.Command("git", "show", r.fetch.Hash+":go.toolchain.rev")
	cmd.Dir = r.c.gitDir
	toolChainOut, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("looking up go.toolchain.rev for %v: %v", r.fetch.Hash, err)
	}
	r.toolchain = strings.TrimSpace(string(toolChainOut))
	r.toolChainTarball = GetLazy(func() (mem.RO, error) {
		s := r.startSpan("get-toolchain-tgz")
		v, err := r.c.getToolchainTarball(r.ctx, r.toolchain)
		s.end(err)
		return v, err
	})

	freeMachines := make(chan *WorkerClient, 1000)
	machineErr := make(chan error, 1)
	for i := 0; i < r.req.Machines; i++ {
		go func() {
			wc, err := r.getUsableMachine()
			if err != nil {
				select {
				case machineErr <- err:
				default:
				}
				return
			}
			select {
			case freeMachines <- wc:
			case <-r.ctx.Done():
			}
		}()
	}

	doneTask := make(chan *tbtype.Task, 16)
	remain := r.req.Tasks
	done := 0
	gotAnyMachine := false
	for {
		select {
		case <-r.ctx.Done():
			return r.ctx.Err()
		case <-doneTask:
			done++
			if done == len(r.req.Tasks) {
				return nil
			}
		case err := <-machineErr:
			if !gotAnyMachine {
				return err
			}
		case wc := <-freeMachines:
			gotAnyMachine = true
			if len(remain) == 0 {
				// TODO: early return it so different run can use it?
				break
			}
			task := remain[0]
			remain = remain[1:]
			go func() {
				if err := r.runTask(wc, task); err != nil {
					log.Printf("task %q: %v", task.Name, err)
				}
				select {
				case doneTask <- task:
					select {
					case freeMachines <- wc:
					case <-r.ctx.Done():
					}
				case <-r.ctx.Done():
					return
				}
			}()
		}
	}
}

func (r *Run) runNetMeshTest() error {
	s := r.startSpan("get-all-machines")
	mm := make([]*WorkerClient, r.req.Machines)
	var eg errgroup.Group
	for i := 0; i < r.req.Machines; i++ {
		i := i
		eg.Go(func() (err error) {
			mm[i], err = r.getUsableMachine()
			return
		})
	}
	if err := s.end(eg.Wait()); err != nil {
		return err
	}

	if r.req.Machines <= 1 {
		return nil
	}

	s = r.startSpan("netmesh-all")
	var eg2 errgroup.Group
	style := "pairs-sort" // "mesh"
	switch style {
	case "pairs-sort":
		sort.Slice(mm, func(i, j int) bool {
			return mm[i].m.PrivateIP.Compare(*mm[j].m.PrivateIP) < 0
		})
		fallthrough
	case "pairs":
		size := (1 << 30) * len(mm) / (len(mm) / 2)
		for i, wc := range mm {
			if i%2 == 0 {
				continue
			}
			var args []string
			args = append(args, fmt.Sprintf("http://[%s]:8080/rand?n=%v", mm[i-1].m.PrivateIP, size))
			wc := wc
			s := r.startSpan(fmt.Sprintf("run-netmesh-%v", i))
			eg2.Go(func() error {
				return s.end(wc.Exec(r.ctx, r, r, &tbtype.ExecRequest{
					Cmd:                   tbtype.CmdNetMesh,
					Args:                  args,
					MergeStderrIntoStdout: true,
					TimeoutSeconds:        60 * 10,
				}))
			})
		}

	case "mesh":
		size := (300 << 20) / (r.req.Machines - 1)
		for i, wc := range mm {
			args := make([]string, 0, len(mm)-1)
			for _, wc2 := range mm {
				if wc == wc2 {
					continue
				}
				args = append(args, fmt.Sprintf("http://[%s]:8080/rand?n=%v", wc2.m.PrivateIP, size))
			}
			wc := wc
			s := r.startSpan(fmt.Sprintf("run-netmesh-%v", i))
			eg2.Go(func() error {
				return s.end(wc.Exec(r.ctx, r, r, &tbtype.ExecRequest{
					Cmd:                   tbtype.CmdNetMesh,
					Args:                  args,
					MergeStderrIntoStdout: true,
					TimeoutSeconds:        60 * 10,
				}))
			})
		}
	}
	return s.end(eg2.Wait())
}

func (r *Run) runTask(wc *WorkerClient, task *tbtype.Task) (retErr error) {
	ts := &TaskStatus{r: r, task: task, startedAt: time.Now()}
	r.mu.Lock()
	mak.Set(&r.tasks, task, ts)
	r.mu.Unlock()

	defer func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		ts.endedAt = time.Now()
		ts.finalErr = retErr
	}()

	switch task.Action {
	case "check-go":
		s := r.startSpan("check-go")
		v, err := wc.CheckGo(r.ctx)
		s.end(err)
		fmt.Fprintf(ts, "CheckGo = %q, %v\n", v, err)
		return err
	case "test":
		// TODO(bradfitz): support more than one package per invocation;
		// overhaul the Test method into a generic Exec method like Go's
		// buildlet.
		return r.runSpan("task-"+task.Name, func() error { return wc.Test(r.ctx, ts, task.Packages[0]) })
	}
	return fmt.Errorf("unknown action %q", task.Action)
}

var (
	metricToolChainHit    = expvar.NewInt("counter_toolchain_hit")
	metricToolChainMiss   = expvar.NewInt("counter_toolchain_miss")
	metricToolChainFilled = expvar.NewInt("counter_toolchain_filled")
	metricToolChainErr    = expvar.NewInt("counter_toolchain_err")
)

func (c *Controller) getToolchainTarball(ctx context.Context, goHash string) (tgz mem.RO, retErr error) {
	var zero mem.RO
	defer func() {
		if retErr != nil {
			metricToolChainErr.Add(1)
		}
	}()
	if !hashRx.MatchString(goHash) {
		return zero, fmt.Errorf("bad hash")
	}

	cacheDir := filepath.Join(c.cacheRoot, "toolchain")
	if err := os.MkdirAll(cacheDir, 0755); err != nil {
		return zero, err
	}
	cacheFile := filepath.Join(cacheDir, goHash+".tar.gz")
	if all, err := os.ReadFile(cacheFile); err == nil {
		metricToolChainHit.Add(1)
		return mem.B(all), nil
	}
	metricToolChainMiss.Add(1)
	req, err := http.NewRequestWithContext(ctx, "GET", "https://github.com/tailscale/go/releases/download/build-"+goHash+"/linux-amd64.tar.gz", nil)
	if err != nil {
		return zero, err
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return zero, err
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return zero, fmt.Errorf("bad status: %v", res.Status)
	}
	all, err := io.ReadAll(res.Body)
	if err != nil {
		return zero, err
	}
	tmpFile := cacheFile + ".tmp"
	if err := os.WriteFile(tmpFile, all, 0644); err != nil {
		return zero, err
	}
	if err := os.Rename(tmpFile, cacheFile); err != nil {
		return zero, err
	}
	metricToolChainFilled.Add(1)
	return mem.B(all), nil
}

type Lazy[T any] struct {
	ready chan struct{} // closed on done
	v     T
	err   error
}

func GetLazy[T any](f func() (T, error)) *Lazy[T] {
	lv := &Lazy[T]{ready: make(chan struct{})}
	go func() {
		lv.v, lv.err = f()
		close(lv.ready)
	}()
	return lv
}

func (lv *Lazy[T]) Get(ctx context.Context) (T, error) {
	select {
	case <-lv.ready:
		return lv.v, lv.err
	case <-ctx.Done():
		var zero T
		return zero, ctx.Err()
	}
}

var (
	metricGoProxyCacheHit      = expvar.NewInt("counter_goproxy_cache_hit")
	metricGoProxyCacheHitBytes = expvar.NewInt("counter_goproxy_cache_hit_bytes")
	metricGoProxyCacheMiss     = expvar.NewInt("counter_goproxy_cache_miss")
	metricGoProxyCacheErr      = expvar.NewInt("counter_goproxy_cache_err")

	metricGoProxyFetchCount = expvar.NewInt("counter_goproxy_fetch_count")
	metricGoProxyFetchBytes = expvar.NewInt("counter_goproxy_fetch_bytes")
	metricGoProxyFetchErr   = expvar.NewInt("counter_goproxy_fetch_err")

	metricGoProxyPutSuccess = expvar.NewInt("counter_goproxy_put_success")
	metricGoProxyPutErr     = expvar.NewInt("counter_goproxy_put_err")
)

var _ goproxy.Cacher = goProxyMetricTrackingCacher{}

type goProxyMetricTrackingCacher struct {
	c goproxy.Cacher
}

func (c goProxyMetricTrackingCacher) Get(ctx context.Context, name string) (io.ReadCloser, error) {
	v, err := c.c.Get(ctx, name)
	switch {
	case err == nil:
		metricGoProxyCacheHit.Add(1)
		if fi, ok := v.(interface{ Size() int64 }); ok { // DirCache embeds an os.FileInfo in its ReadCloser type
			metricGoProxyCacheHitBytes.Add(fi.Size())
		}
	case errors.Is(err, fs.ErrNotExist):
		metricGoProxyCacheMiss.Add(1)
	default:
		metricGoProxyCacheErr.Add(1)
	}
	return v, err
}

// Put puts a cache for the name with the content.
func (c goProxyMetricTrackingCacher) Put(ctx context.Context, name string, content io.ReadSeeker) error {
	err := c.c.Put(ctx, name, content)
	if err != nil {
		metricGoProxyPutErr.Add(1)
		return err
	}
	metricGoProxyPutSuccess.Add(1)
	return nil
}

type goProxyMetricTrackingRoundTripper struct {
	rt http.RoundTripper
}

func (rt goProxyMetricTrackingRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	res, err := rt.rt.RoundTrip(req)
	if err != nil {
		metricGoProxyFetchErr.Add(1)
		return nil, err
	}
	if res.StatusCode == 200 {
		metricGoProxyFetchCount.Add(1)
		if res.ContentLength > 0 {
			metricGoProxyFetchBytes.Add(res.ContentLength)
		}
	} else {
		metricGoProxyFetchErr.Add(1)
	}
	return res, nil
}
