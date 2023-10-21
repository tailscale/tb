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
	"errors"
	"expvar"
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
	"tailscale.com/tsweb"
	"tailscale.com/types/logger"
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

	c := &Controller{
		cacheRoot:  *cacheDir,
		gitDir:     filepath.Join(*cacheDir, "git"),
		machineSem: syncs.NewSemaphore(*maxMachines),
		tsnetMux:   http.NewServeMux(),
		fc: &fly.Client{
			App:   *workerApp,
			Token: os.Getenv("FLY_TOKEN"),
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
		if r.URL.Path == "/archive" {
			c.serveArchive(w, r)
			return
		}
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
	case "/archive":
		c.serveArchive(w, r)
	case "/du":
		c.serveDU(w, r)
	case "/run":
		c.serveRun(w, r)
	case "/":
		fmt.Fprintf(w, `<html><body><h1>Tailscale Build</h1>
		[<a href='/du'>du</a>] [<a href="/debug">debug</a>]
		<form method=POST action=/fetch>Fetch ref: <input name=ref><input type=submit value="fetch"></form>
		<form method=POST action=/start-build>Test ref: <input name=ref><input type=submit value="start"></form>
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

func (c *Controller) runByLocalRef(ref string) *Run {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, r := range c.runs {
		if r.fetch.LocalRef == ref {
			return r
		}
	}
	return nil
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

func (c *Controller) bestEffortDeleteMachine(id fly.MachineID) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	err := c.fc.StopMachine(ctx, id, &fly.StopParam{Signal: "kill", Timeout: time.Nanosecond})
	log.Printf("stop machine = %v", err)

	for ctx.Err() == nil {
		err = c.fc.DeleteMachine(ctx, id)
		log.Printf("delete of machine %v: err=%v", id, err)
		if err == nil {
			metricDeleteMachineOK.Add(1)
			return
		}
		time.Sleep(time.Second)
	}
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
	metricCounterArchives.Add(1)
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

	run := c.runByLocalRef(ref)
	if run == nil {
		log.Printf("can't find run for local ref %q", ref)
		http.Error(w, "can't find run by local ref", http.StatusBadRequest)
		return
	}
	s := run.startSpan("archive-clone-shallow")

	td, err := os.MkdirTemp("", "tbarchive-*")
	if err != nil {
		s.end(err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer os.RemoveAll(td)

	shallowClone := filepath.Join(td, "shallow")
	cmd = exec.Command("git", "clone",
		"--depth=1",
		"--single-branch",
		"--branch="+ref,
		"file://"+c.gitDir,
		shallowClone)
	cmd.Dir = c.gitDir
	if out, err := cmd.CombinedOutput(); err != nil {
		s.end(err)
		http.Error(w, fmt.Sprintf("git clone: %v\n%s", err, out), http.StatusInternalServerError)
		return
	}
	s.end(nil)

	s = run.startSpan("archive-tar-send")

	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s.tar.gz"`, hash))
	cmd = exec.Command("tar", "-zcf", "-", ".")
	cmd.Dir = shallowClone
	cmd.Stdout = w
	var errBuf bytes.Buffer
	cmd.Stderr = &errBuf
	if err := s.end(cmd.Run()); err != nil {
		http.Error(w, fmt.Sprintf("git archive: %v\n%s", err, errBuf.Bytes()), http.StatusInternalServerError)
		return
	}
}

func (c *Controller) serveStartBuild(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	ref := r.FormValue("ref")

	run := &Run{
		c:         c,
		id:        rands.HexString(32),
		createdAt: time.Now(),
	}
	s := run.startSpan("fetch-ref")
	fetchRes, err := c.fetch(ref)
	s.end(err)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	run.fetch = fetchRes
	// TODO(bradfitz): start building the tarball concurrently

	s = run.startSpan("find-fly-worker-template")
	mm, err := c.fc.ListMachines(ctx)
	s.end(err)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
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
		http.Error(w, "no base machine", http.StatusInternalServerError)
		return
	}
	if base.Config == nil || base.Config.Image == "" {
		http.Error(w, "no base config image machine", http.StatusInternalServerError)
		return
	}
	// Clean up old base machines that aren't the latest.
	for m := range allBase {
		if m != base {
			log.Printf("deleting old base machine %v", m.ID)
			go c.bestEffortDeleteMachine(m.ID)
		}
	}
	run.base = base

	run.ctx, run.cancel = context.WithTimeout(context.Background(), 30*time.Minute)
	c.registerRun(run)
	go run.Run()

	http.Redirect(w, r, "/run?id="+run.id, http.StatusFound)
	fmt.Fprintf(w, "<html><body>base machine: <pre>%v</pre>", logger.AsJSON(base))

}

type WorkerClient struct {
	m           *fly.Machine
	c           *Controller
	hash        string
	machineRand string
}

func (c *WorkerClient) WaitUp(d time.Duration) error {
	donec := make(chan struct{})
	defer close(donec)
	alive := c.c.machineAliveChan(c.machineRand)
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
	spans  []*span
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
	s := &span{
		name:    name,
		r:       r,
		startAt: time.Now(),
	}
	r.mu.Lock()
	defer r.mu.Unlock()
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
	return err
}

func (r *Run) run() error {
	fc := r.c.fc

	s := r.startSpan("create-machine1")
	machineRand := rands.HexString(16)
	aliveCh := make(chan bool, 1)
	r.c.registerMachineAliveChan(machineRand, aliveCh)
	m, err := fc.CreateMachine(r.ctx, &fly.CreateMachineRequest{
		Region: "sea",
		Config: &fly.MachineConfig{
			AutoDestroy: true,
			Env: map[string]string{
				"VM_MAX_DURATION":    "5m",
				"REGISTER_ALIVE_URL": "http://[" + os.Getenv("FLY_PRIVATE_IP") + "]:8080/worker-alive/" + machineRand,
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
	s.end(err)
	if err != nil {
		return fmt.Errorf("CreateMachine: %w", err)
	}
	defer func() { go r.c.bestEffortDeleteMachine(m.ID) }()

	wc := &WorkerClient{m: m, c: r.c, hash: r.fetch.Hash, machineRand: machineRand}
	s = r.startSpan("wait-up1")
	if err := s.end(wc.WaitUp(15 * time.Second)); err != nil {
		return fmt.Errorf("WorkerClient.WaitUp = %w", err)
	}

	tgzURL := fmt.Sprintf("http://[%s]:8080/archive?hash=%s&ref=%s",
		os.Getenv("FLY_PRIVATE_IP"), r.fetch.Hash, url.QueryEscape(r.fetch.LocalRef))

	if err := r.runSpan("push-work-tree", func() error { return wc.PushTreeFromURL(r.ctx, "", tgzURL) }); err != nil {
		return fmt.Errorf("PushTreeFromURL = %w", err)
	}

	s = r.startSpan("check-go")
	v, err := wc.CheckGo(r.ctx)
	s.end(err)
	fmt.Fprintf(r, "CheckGo = %q, %v\n", v, err)

	for _, pkg := range []string{"tailscale.com/util/lru", "tailscale.com/util/cmpx"} {
		r.runSpan("test-"+pkg, func() error { return wc.Test(r.ctx, r, pkg) })
	}

	return nil
}
