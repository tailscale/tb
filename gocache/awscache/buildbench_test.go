// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package awscache

import (
	"cmp"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"maps"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"text/tabwriter"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/tailscale/tb/gocache/cacheproc"
	"github.com/tailscale/tb/gocache/cachers"
)

// TestBuildBench measures how long Go builds take with the cache in real
// AWS. For each build case, it does these builds:
//
//   - "nocache": no remote cache and an empty local cache.
//   - "localwarm": no remote cache, and the local cache (GOCACHE) that
//     nocache filled: the usual warm build on a developer's machine.
//   - "cold", for each storage config: an empty remote cache and an empty
//     local cache.
//   - "warm1", "warm2", ..., for each storage config: the remote cache that
//     cold filled, and an empty local cache, like a CI job in a fresh VM.
//   - "diskwarm", once, after the first config: the GOCACHEPROG with the
//     local disk cache that cold filled, so it answers every get from local
//     disk. Compared with warm, it shows the cost of a remote cache; compared
//     with localwarm, the cost of the GOCACHEPROG protocol.
//
// -buildbench.local=false skips nocache, localwarm, and diskwarm.
//
// It uses the same environment variables as TestRealAWS, and only runs
// with -buildbench. The express config needs
// $AWSCACHE_TEST_EXPRESS_BUCKET, and the cases that build Tailscale need a
// checkout of github.com/tailscale/tailscale (-buildbench.tailscale). See
// lab/README.md for how to run it.
//
// Each case and config of each run uses its own global generation (see
// [benchGeneration]) and its own blob prefix, so every cold build starts
// with an empty cache, and nothing needs to be wiped.
// To repeat only the warm builds against an earlier run's cache, give
// its base generation with -buildbench.gen and set -buildbench.cold=false.
func TestBuildBench(t *testing.T) {
	if !*buildBench {
		t.Skip("-buildbench not set")
	}
	table, bucket := os.Getenv("AWSCACHE_TEST_TABLE"), os.Getenv("AWSCACHE_TEST_BUCKET")
	if table == "" || bucket == "" {
		t.Fatal("AWSCACHE_TEST_TABLE and AWSCACHE_TEST_BUCKET must be set")
	}
	expressBucket := os.Getenv("AWSCACHE_TEST_EXPRESS_BUCKET")

	cases, err := selectBenchItems(benchCases, *buildBenchCases)
	if err != nil {
		t.Fatal(err)
	}
	configs, err := selectBenchItems(benchConfigs, *buildBenchConfigs)
	if err != nil {
		t.Fatal(err)
	}
	baseGen := *buildBenchGen
	if baseGen == 0 {
		if !*buildBenchCold {
			t.Fatal("-buildbench.cold=false needs -buildbench.gen")
		}
		baseGen = int(time.Now().Unix())
	}

	region := os.Getenv("AWS_REGION")
	b := newBuildBencher(t, func(ctx context.Context, cfg benchConfig, gen int) (*Cache, error) {
		// Default SpoolDir (os.TempDir), so builds and spooling use the
		// same disk.
		bkt := bucket
		if cfg.express {
			bkt = expressBucket
		}
		ccfg := Config{
			Region:           region,
			Table:            table,
			Bucket:           bkt,
			BlobPrefix:       fmt.Sprintf("buildbench/%d/", gen),
			GlobalGeneration: gen,
			Retention:        2 * 24 * time.Hour, // let bench items expire soon
			Logf:             t.Logf,
		}
		if cfg.pod {
			// The stores Config would make, but with blobs in gocached.
			awsCfg, err := loadAWSConfig(ctx, region)
			if err != nil {
				return nil, err
			}
			m := newMetrics() // the stores' own; not reported
			ccfg.meta = &dynamoStore{c: dynamodb.NewFromConfig(awsCfg), table: table, m: m}
			ccfg.blobs = newPodBlobStore(*buildBenchGocached, &s3Store{c: s3.NewFromConfig(awsCfg), bucket: bucket, service: "s3", m: m})
		}
		c, err := New(ctx, ccfg)
		if err != nil {
			return nil, err
		}
		if err := c.Check(ctx); err != nil {
			c.Close(ctx)
			return nil, err
		}
		return c, nil
	})
	if *buildBenchOut != "" {
		f, err := os.OpenFile(*buildBenchOut, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0644)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		b.out = json.NewEncoder(f)
	}

	goVersion, err := exec.Command("go", "version").CombinedOutput()
	if err != nil {
		t.Fatalf("go version: %v, %s", err, goVersion)
	}
	t.Logf("%s; base generation %d", strings.TrimSpace(string(goVersion)), baseGen)

	// Find the repos the cases build in, and download their modules first,
	// so builds don't include that.
	repoDirs := map[string]string{"": b.modRoot}
	for _, bc := range cases {
		if bc.repo != "tailscale" || repoDirs[bc.repo] != "" {
			continue
		}
		if *buildBenchTailscale == "" {
			t.Fatalf("case %s needs -buildbench.tailscale or $AWSCACHE_BENCH_TAILSCALE_DIR", bc.name)
		}
		dir, err := filepath.Abs(*buildBenchTailscale)
		if err != nil {
			t.Fatal(err)
		}
		repoDirs[bc.repo] = dir
	}
	for repo, dir := range repoDirs {
		if out, err := b.goCmd(dir, "mod", "download").CombinedOutput(); err != nil {
			t.Fatalf("go mod download in %s: %v, %s", dir, err, out)
		}
		if commit, err := exec.Command("git", "-C", dir, "rev-parse", "HEAD").Output(); err == nil {
			t.Logf("repo %q: %s at %s", repo, dir, strings.TrimSpace(string(commit)))
		}
	}
	for i, bc := range cases {
		if bc.dir == "" {
			cases[i].dir = repoDirs[bc.repo]
		}
	}

	b.gocachedURL = *buildBenchGocached
	configs = slices.DeleteFunc(configs, func(cfg benchConfig) bool {
		switch {
		case cfg.express && expressBucket == "":
			t.Logf("skipping config %s: AWSCACHE_TEST_EXPRESS_BUCKET not set", cfg.name)
			return true
		case (cfg.gocached || cfg.pod) && b.gocachedURL == "":
			t.Logf("skipping config %s: -buildbench.gocached not set", cfg.name)
			return true
		}
		return false
	})
	for _, bc := range cases {
		b.runCase(bc, configs, baseGen)
	}
	b.printSummary()
}

// runCase does all the builds of case bc. See TestBuildBench.
func (b *buildBencher) runCase(bc benchCase, configs []benchConfig, baseGen int) {
	if *buildBenchLocal {
		d := b.run(bc, noRemote, 0, "nocache", runDirs{})
		b.run(bc, noRemote, 0, "localwarm", runDirs{gocache: d.gocache}).remove()
		d.remove()
	}
	diskWarmDone := !*buildBenchLocal
	for _, cfg := range configs {
		gen := benchGeneration(baseGen, bc, cfg)
		var cold runDirs
		if *buildBenchCold {
			cold = b.run(bc, cfg, gen, "cold", runDirs{})
		}
		for i := range *buildBenchWarm {
			b.run(bc, cfg, gen, fmt.Sprintf("warm%d", i+1), runDirs{}).remove()
		}
		if !diskWarmDone && cold.progDisk != "" {
			b.run(bc, cfg, gen, "diskwarm", runDirs{progDisk: cold.progDisk}).remove()
			diskWarmDone = true
		}
		cold.remove()
	}
}

var (
	buildBench          = flag.Bool("buildbench", false, "run TestBuildBench against real AWS")
	buildBenchCases     = flag.String("buildbench.cases", "tstest", "comma-separated build cases for TestBuildBench: "+benchItemNames(benchCases))
	buildBenchConfigs   = flag.String("buildbench.configs", "express", "comma-separated storage configs for TestBuildBench: "+benchItemNames(benchConfigs))
	buildBenchWarm      = flag.Int("buildbench.warm", 1, "warm builds per case and config")
	buildBenchCold      = flag.Bool("buildbench.cold", true, "do a cold build of each case and config before the warm builds; if false, -buildbench.gen is required")
	buildBenchLocal     = flag.Bool("buildbench.local", true, "also do the builds without a remote cache (nocache, localwarm) and with a warm local disk (diskwarm)")
	buildBenchGen       = flag.Int("buildbench.gen", 0, "base generation; 0 means a new one from the current time")
	buildBenchOut       = flag.String("buildbench.out", "", "if non-empty, a file to append results to as JSON lines")
	buildBenchTailscale = flag.String("buildbench.tailscale", os.Getenv("AWSCACHE_BENCH_TAILSCALE_DIR"),
		"checkout of github.com/tailscale/tailscale, for the cases that build Tailscale; default $AWSCACHE_BENCH_TAILSCALE_DIR")
	buildBenchGocached = flag.String("buildbench.gocached", os.Getenv("AWSCACHE_BENCH_GOCACHED_URL"),
		"base URL of a gocached server without auth, for the gocached and ddbgocached configs; default $AWSCACHE_BENCH_GOCACHED_URL")
)

// benchCase is a go command whose build time TestBuildBench measures.
type benchCase struct {
	name string
	repo string // where to run it: "" for this module, or "tailscale"
	dir  string // if non-empty, where to run it instead of repo
	args []string
}

func (c benchCase) itemName() string { return c.name }

// outDirArg, in a benchCase's args, is replaced by an empty directory
// (with a trailing slash), for go build -o.
const outDirArg = "{outdir}"

var benchCases = []benchCase{
	// Tailscale's OSS repo. go build -o links the binaries, then discards
	// them. cmd/tailscale/cli is a library, which go build only compiles.
	{name: "tailscale", repo: "tailscale", args: []string{"build", "-o", os.DevNull, "./cmd/tailscale"}},
	{name: "tailscaled", repo: "tailscale", args: []string{"build", "-o", os.DevNull, "./cmd/tailscaled"}},
	{name: "tailscalecli", repo: "tailscale", args: []string{"build", "./cmd/tailscale/cli"}},
	{name: "tsall", repo: "tailscale", args: []string{"build", "-o", outDirArg, "./cmd/tailscale", "./cmd/tailscaled", "./cmd/tailscale/cli"}},
	// Like corp CI's "go test -vet=off tailscale.io/...", but for the OSS
	// repo: it compiles and links every test binary, but -exec=/bin/true
	// runs /bin/true instead of each test binary.
	{name: "tstest", repo: "tailscale", args: []string{"test", "-vet=off", "-count=1", "-exec=/bin/true", "./..."}},

	// Go itself, and this module.
	{name: "std", args: []string{"build", "std"}},
	{name: "cmd", args: []string{"build", "cmd"}},
	{name: "tb", args: []string{"build", "./..."}},
	{name: "tbvet", args: []string{"vet", "./..."}},
	{name: "tbtest", args: []string{"test", "-count=1", "-run=^$", "./..."}},
}

// benchGeneration returns the global generation of case bc with config cfg
// in the run with base generation baseGen: baseGen*1000, plus 10 times the
// case's index in benchCases, plus the config's index.
func benchGeneration(baseGen int, bc benchCase, cfg benchConfig) int {
	ci := slices.IndexFunc(benchCases, func(c benchCase) bool { return c.name == bc.name })
	return baseGen*1000 + max(ci, 0)*10 + cfg.index
}

// benchConfig is a storage config for TestBuildBench.
type benchConfig struct {
	name    string
	index   int  // added to the generation, so each config has its own cache
	express bool // blobs in $AWSCACHE_TEST_EXPRESS_BUCKET, not $AWSCACHE_TEST_BUCKET

	// gocached is whether the GOCACHEPROG talks to the gocached server at
	// -buildbench.gocached directly, with no Cache: the design of today's
	// cigocached.
	gocached bool

	// pod is whether the Cache keeps blobs in the gocached server at
	// -buildbench.gocached, in front of $AWSCACHE_TEST_BUCKET, with
	// metadata in DynamoDB as usual: a simulation of the NVMe cache pod
	// design, with one pod. See [podBlobStore].
	pod bool
}

func (c benchConfig) itemName() string { return c.name }

var benchConfigs = []benchConfig{
	{name: "s3", index: 0},                     // S3 Standard
	{name: "express", index: 1, express: true}, // S3 Express One Zone
	{name: "gocached", index: 2, gocached: true},
	{name: "ddbgocached", index: 3, pod: true},
}

type benchItem interface{ itemName() string }

func benchItemNames[T benchItem](items []T) string {
	var names []string
	for _, it := range items {
		names = append(names, it.itemName())
	}
	return strings.Join(names, ", ")
}

// selectBenchItems returns the items named in the comma-separated list.
func selectBenchItems[T benchItem](items []T, list string) ([]T, error) {
	var out []T
	for name := range strings.SplitSeq(list, ",") {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		i := slices.IndexFunc(items, func(it T) bool { return it.itemName() == name })
		if i < 0 {
			return nil, fmt.Errorf("unknown name %q; want one of %s", name, benchItemNames(items))
		}
		out = append(out, items[i])
	}
	return out, nil
}

type buildBencher struct {
	t           *testing.T
	newCache    func(_ context.Context, _ benchConfig, gen int) (*Cache, error)
	gocachedURL string // for configs that use gocached
	exe         string // this test binary, which is also the GOCACHEPROG
	modRoot     string
	out         *json.Encoder // or nil

	results []benchResult
}

func newBuildBencher(t *testing.T, newCache func(context.Context, benchConfig, int) (*Cache, error)) *buildBencher {
	b := &buildBencher{t: t, newCache: newCache}
	var err error
	b.exe, err = os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	b.modRoot, err = filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// benchResult is the result of one build.
type benchResult struct {
	Case       string
	Config     string
	Run        string // "baseline", "cold", or "warmN"
	Generation int    `json:",omitempty"`
	Time       time.Time

	Err string `json:",omitempty"` // if the go command failed

	WallSecs  float64 // the whole go command
	DrainSecs float64 // of WallSecs, waiting for background PUTs at exit

	// From the GOCACHEPROG.
	Gets, LocalHits, RemoteHits, RemoteErrs int64
	Puts, PutsTimedOut, PutsCanceled        int64

	// From the HTTP handler: counts, and the total time in each phase, for
	// each tier.
	Stats *Stats `json:",omitempty"`

	// From the Cache's metrics: the latency of each phase of gets and puts,
	// by tier and then phase. See gocache_aws_get_phase_duration_seconds
	// and gocache_aws_put_phase_duration_seconds.
	GetPhases map[string]map[string]latencyStats `json:",omitempty"`
	PutPhases map[string]map[string]latencyStats `json:",omitempty"`

	// Sizes are histograms of uncompressed object sizes, by op: "get" for
	// hits, and "put" for objects stored. See gocache_aws_object_bytes.
	Sizes map[string]sizeHist `json:",omitempty"`
}

// sizeHist is a histogram of object sizes.
type sizeHist []sizeBucket

// sizeBucket is the number of objects larger than the previous bucket's
// UpTo, and at most UpTo bytes.
type sizeBucket struct {
	UpTo  int64 // noLimit for the last bucket (JSON can't encode +Inf)
	Count int64
}

// noLimit is the UpTo of the last sizeBucket.
const noLimit = math.MaxInt64

// count returns the number of objects in h larger than lo bytes, and at
// most hi. lo and hi must be bucket bounds, or -1 and noLimit.
func (h sizeHist) count(lo, hi int64) int64 {
	var n int64
	for _, bk := range h {
		if bk.UpTo > lo && bk.UpTo <= hi {
			n += bk.Count
		}
	}
	return n
}

// misses returns how many of r's gets missed in both the local and the
// remote cache.
func (r *benchResult) misses() int64 {
	return r.Gets - r.LocalHits - r.RemoteHits - r.RemoteErrs
}

// getTier returns r's stats of gets from tier.
func (r *benchResult) getTier(tier string) GetTierStats {
	if r.Stats == nil {
		return GetTierStats{}
	}
	return r.Stats.GetTiers[tier]
}

// getPhase returns r's latency stats of phase of gets from tier.
func (r *benchResult) getPhase(tier, phase string) latencyStats {
	return r.GetPhases[tier][phase]
}

type latencyStats struct {
	N        int64
	Mean     float64
	P50, P95 float64 // estimated from histogram buckets
}

// goCmd returns a go command in the module root, with any GOCACHE and
// GOCACHEPROG from the environment removed.
func (b *buildBencher) goCmd(dir string, args ...string) *exec.Cmd {
	cmd := exec.Command("go", args...)
	cmd.Dir = b.modRoot
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Env = slices.DeleteFunc(os.Environ(), func(kv string) bool {
		return strings.HasPrefix(kv, "GOCACHE=") || strings.HasPrefix(kv, "GOCACHEPROG=")
	})
	return cmd
}

// noRemote is the config of builds without a remote cache.
var noRemote = benchConfig{name: "none"}

// runDirs are the local cache directories of a build.
type runDirs struct {
	root     string // the build's own directory, which holds the others unless reused
	gocache  string // GOCACHE
	progDisk string // the GOCACHEPROG's local disk cache, if it has one
}

// remove removes the build's own directory, but not directories reused from
// another build.
func (d runDirs) remove() {
	if d.root != "" {
		os.RemoveAll(d.root)
	}
}

// run does one build of bc with storage config cfg in generation gen, and
// records the result. The noRemote config means no remote cache and no
// GOCACHEPROG. It uses the GOCACHE and GOCACHEPROG disk cache in reuse,
// if set, or else new empty ones. It returns the directories it used; the
// caller must remove them.
func (b *buildBencher) run(bc benchCase, cfg benchConfig, gen int, runName string, reuse runDirs) runDirs {
	t := b.t
	ctx := context.Background()
	// Not t.TempDir, which would keep every build's caches until the test
	// ends.
	dir, err := os.MkdirTemp("", "buildbench-"+runName+"-")
	if err != nil {
		t.Fatal(err)
	}
	dirs := runDirs{
		root:     dir,
		gocache:  cmp.Or(reuse.gocache, filepath.Join(dir, "gocache")),
		progDisk: cmp.Or(reuse.progDisk, filepath.Join(dir, "cacheprog")),
	}
	res := benchResult{
		Case:       bc.name,
		Config:     cfg.name,
		Run:        runName,
		Generation: gen,
		Time:       time.Now().UTC(),
	}

	args := slices.Clone(bc.args)
	for i, a := range args {
		if a == outDirArg {
			args[i] = filepath.Join(dir, "out") + string(filepath.Separator)
		}
	}
	cmd := b.goCmd(bc.dir, args...)
	cmd.Env = append(cmd.Env, "GOCACHE="+dirs.gocache)

	var c *Cache
	var h *Handler
	statsFile := filepath.Join(dir, "cacheprog-stats.json")
	if cfg != noRemote {
		var url, salt string
		if cfg.gocached {
			// No Cache: the GOCACHEPROG talks to gocached directly. Salting
			// its action IDs with the generation gives each config of each
			// run an empty cache in gocached's one global namespace.
			if b.gocachedURL == "" {
				t.Fatalf("config %s needs -buildbench.gocached", cfg.name)
			}
			url, salt = b.gocachedURL, strconv.Itoa(gen)
		} else {
			c, err = b.newCache(ctx, cfg, gen)
			if err != nil {
				t.Fatal(err)
			}
			h = c.NewHandler(func(*http.Request) Grant { return Grant{CanWrite: true} })
			ts := httptest.NewServer(h)
			defer func() {
				ts.Close()
				c.Close(ctx)
			}()
			url = ts.URL
		}
		args := []string{b.exe, cacheprogArg, url, dirs.progDisk, statsFile}
		if salt != "" {
			args = append(args, salt)
		}
		for _, a := range args {
			if strings.ContainsAny(a, " \t'\"") {
				t.Fatalf("GOCACHEPROG argument %q has spaces or quotes", a)
			}
		}
		cmd.Env = append(cmd.Env, "GOCACHEPROG="+strings.Join(args, " "))
	}

	start := time.Now()
	out, err := cmd.CombinedOutput()
	res.WallSecs = time.Since(start).Seconds()
	if err != nil {
		// Keep going: a build with a few broken packages still measures
		// the cache.
		t.Errorf("%s/%s/%s: go %s: %v\n%s", bc.name, cfg.name, runName, strings.Join(bc.args, " "), err, tail(out, 4096))
		res.Err = err.Error()
	}

	if cfg != noRemote {
		var ps cacheprogStats
		if j, err := os.ReadFile(statsFile); err != nil {
			t.Errorf("reading GOCACHEPROG stats: %v", err)
		} else if err := json.Unmarshal(j, &ps); err != nil {
			t.Errorf("parsing GOCACHEPROG stats: %v", err)
		}
		res.DrainSecs = ps.DrainSecs
		res.Gets, res.LocalHits, res.RemoteHits, res.RemoteErrs = ps.Gets, ps.LocalHits, ps.RemoteHits, ps.RemoteErrs
		res.Puts, res.PutsTimedOut, res.PutsCanceled = ps.Puts, ps.PutsTimedOut, ps.PutsCanceled
		if !ps.Drained {
			t.Errorf("%s/%s/%s: background PUTs didn't drain", bc.name, cfg.name, runName)
		}
	}
	if c != nil {
		st := h.Stats()
		res.Stats = &st
		b.addCacheMetrics(&res, c)
	}

	b.results = append(b.results, res)
	t.Logf("%s/%s/%s: %.1fs (drain %.1fs); gets %d, local hits %d, remote hits %d (inline %d, s3 %d), misses %d; puts %d",
		bc.name, cfg.name, runName, res.WallSecs, res.DrainSecs, res.Gets, res.LocalHits, res.RemoteHits,
		res.getTier("inline").Gets, res.getTier("s3").Gets, res.misses(), res.Puts)
	if b.out != nil {
		if err := b.out.Encode(res); err != nil {
			t.Error(err)
		}
	}
	return dirs
}

// addCacheMetrics fills in res from c's Prometheus metrics.
func (b *buildBencher) addCacheMetrics(res *benchResult, c *Cache) {
	reg := prometheus.NewRegistry()
	reg.MustRegister(c.Collectors()...)
	mfs, err := reg.Gather()
	if err != nil {
		b.t.Fatal(err)
	}
	res.GetPhases = map[string]map[string]latencyStats{}
	res.PutPhases = map[string]map[string]latencyStats{}
	res.Sizes = map[string]sizeHist{}
	for _, mf := range mfs {
		var phases map[string]map[string]latencyStats
		switch mf.GetName() {
		case "gocache_aws_object_bytes":
			for _, m := range mf.GetMetric() {
				var op string
				for _, lp := range m.GetLabel() {
					if lp.GetName() == "op" {
						op = lp.GetValue()
					}
				}
				var hist sizeHist
				var prev uint64
				h := m.GetHistogram()
				for _, bk := range h.GetBucket() {
					hist = append(hist, sizeBucket{UpTo: int64(bk.GetUpperBound()), Count: int64(bk.GetCumulativeCount() - prev)})
					prev = bk.GetCumulativeCount()
				}
				hist = append(hist, sizeBucket{UpTo: noLimit, Count: int64(h.GetSampleCount() - prev)})
				res.Sizes[op] = hist
			}
			continue
		case "gocache_aws_get_phase_duration_seconds":
			phases = res.GetPhases
		case "gocache_aws_put_phase_duration_seconds":
			phases = res.PutPhases
		default:
			continue
		}
		for _, m := range mf.GetMetric() {
			labels := map[string]string{}
			for _, lp := range m.GetLabel() {
				labels[lp.GetName()] = lp.GetValue()
			}
			tier := labels["tier"]
			if phases[tier] == nil {
				phases[tier] = map[string]latencyStats{}
			}
			phases[tier][labels["phase"]] = histStats(m.GetHistogram())
		}
	}
}

// histStats returns the count, mean, and estimated quantiles of h.
func histStats(h *dto.Histogram) latencyStats {
	n := int64(h.GetSampleCount())
	if n == 0 {
		return latencyStats{}
	}
	return latencyStats{
		N:    n,
		Mean: h.GetSampleSum() / float64(n),
		P50:  histQuantile(h, 0.5),
		P95:  histQuantile(h, 0.95),
	}
}

// histQuantile estimates quantile q of h by linear interpolation within
// its buckets, as Prometheus's histogram_quantile does.
func histQuantile(h *dto.Histogram, q float64) float64 {
	total := float64(h.GetSampleCount())
	rank := q * total
	lower, prevCount := 0.0, 0.0
	for _, bk := range h.GetBucket() {
		upper, count := bk.GetUpperBound(), float64(bk.GetCumulativeCount())
		if count >= rank {
			if math.IsInf(upper, 1) || count == prevCount {
				return lower
			}
			return lower + (upper-lower)*(rank-prevCount)/(count-prevCount)
		}
		lower, prevCount = upper, count
	}
	return lower // in the implicit +Inf bucket
}

// printSummary logs tables of all results: an overview of each build, and
// the total time that each build's gets and puts spent in each phase, by
// tier. The totals are sums over concurrent requests, so they can be more
// than the wall time.
func (b *buildBencher) printSummary() {
	var sb strings.Builder
	tw := tabwriter.NewWriter(&sb, 0, 0, 2, ' ', tabwriter.AlignRight)
	ms := func(ls latencyStats) string {
		if ls.N == 0 {
			return "-"
		}
		return fmt.Sprintf("%.1f", ls.P50*1000)
	}
	fmt.Fprintln(tw, "case\tconfig\trun\twall s\tdrain s\tgets\tlocal hits\tinline\ts3\tmisses\tuploads\t"+
		"lookup p50 ms\tfirst byte p50 ms\ttransfer p50 ms\t")
	for _, r := range b.results {
		var uploads int64
		if r.Stats != nil {
			for _, ts := range r.Stats.PutTiers {
				uploads += ts.BlobUploads
			}
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%.1f\t%.1f\t%d\t%d\t%d\t%d\t%d\t%d\t%s\t%s\t%s\t\n",
			r.Case, r.Config, r.Run, r.WallSecs, r.DrainSecs, r.Gets, r.LocalHits,
			r.getTier("inline").Gets, r.getTier("s3").Gets, r.misses(), uploads,
			ms(r.getPhase("s3", "lookup")), ms(r.getPhase("s3", "first_byte")), ms(r.getPhase("s3", "transfer")))
	}
	tw.Flush()

	msTotal := func(nanos int64) string { return fmt.Sprintf("%.0f", float64(nanos)/1e6) }
	mib := func(n int64) string { return fmt.Sprintf("%.1f", float64(n)/(1<<20)) }
	sb.WriteString("\nget time by tier (total ms):\n")
	tw = tabwriter.NewWriter(&sb, 0, 0, 2, ' ', tabwriter.AlignRight)
	fmt.Fprintln(tw, "case\tconfig\trun\ttier\tgets\tall\tlookup\tfirst byte\ttransfer\tblob MiB\t")
	for _, r := range b.results {
		if r.Stats == nil {
			continue
		}
		for _, tier := range slices.Sorted(maps.Keys(r.Stats.GetTiers)) {
			ts := r.Stats.GetTiers[tier]
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d\t%s\t%s\t%s\t%s\t%s\t\n",
				r.Case, r.Config, r.Run, tier, ts.Gets, msTotal(ts.Nanos),
				msTotal(ts.LookupNanos), msTotal(ts.FirstByteNanos), msTotal(ts.TransferNanos), mib(ts.BlobBytes))
		}
	}
	tw.Flush()

	sb.WriteString("\nuncompressed object sizes of get hits (count):\n")
	tw = tabwriter.NewWriter(&sb, 0, 0, 2, ' ', tabwriter.AlignRight)
	fmt.Fprintln(tw, "case\tconfig\trun\thits\t<= 1KiB\t1-64KiB\t64-512KiB\t512KiB-4MiB\t> 4MiB\t")
	for _, r := range b.results {
		h := r.Sizes["get"]
		if len(h) == 0 {
			continue
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%d\t%d\t%d\t%d\t%d\t\n", r.Case, r.Config, r.Run,
			h.count(-1, noLimit), h.count(-1, 1<<10), h.count(1<<10, 64<<10),
			h.count(64<<10, 512<<10), h.count(512<<10, 4<<20), h.count(4<<20, noLimit))
	}
	tw.Flush()

	sb.WriteString("\nput time by tier (total ms):\n")
	tw = tabwriter.NewWriter(&sb, 0, 0, 2, ' ', tabwriter.AlignRight)
	fmt.Fprintln(tw, "case\tconfig\trun\ttier\tputs\tall\texists\tspool wait\treceive\tblob check\tupload\tmeta\tuploads\tblob MiB\t")
	for _, r := range b.results {
		if r.Stats == nil {
			continue
		}
		for _, tier := range slices.Sorted(maps.Keys(r.Stats.PutTiers)) {
			ts := r.Stats.PutTiers[tier]
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%d\t%s\t\n",
				r.Case, r.Config, r.Run, tier, ts.Puts, msTotal(ts.Nanos),
				msTotal(ts.ExistsNanos), msTotal(ts.SpoolWaitNanos), msTotal(ts.ReceiveNanos),
				msTotal(ts.BlobCheckNanos), msTotal(ts.UploadNanos), msTotal(ts.MetaNanos), ts.BlobUploads, mib(ts.BlobBytes))
		}
	}
	tw.Flush()
	b.t.Logf("results:\n%s", sb.String())
}

func tail(b []byte, n int) []byte {
	if len(b) > n {
		return b[len(b)-n:]
	}
	return b
}

// TestBuildBenchFake runs TestBuildBench's machinery against in-memory
// stores, building a tiny program, to check that warm builds hit.
func TestBuildBenchFake(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping builds in short mode")
	}
	dir := t.TempDir()
	for name, data := range map[string]string{
		"go.mod":  "module example.com/hello\n",
		"main.go": "package main\n\nfunc main() { println(\"hello\") }\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0644); err != nil {
			t.Fatal(err)
		}
	}
	env := newTestEnv(t)
	b := newBuildBencher(t, func(ctx context.Context, cfg benchConfig, gen int) (*Cache, error) {
		return New(ctx, Config{
			GlobalGeneration: gen,
			SpoolDir:         t.TempDir(),
			Logf:             t.Logf,
			meta:             env.meta,
			blobs:            env.blobs,
		})
	})
	bc := benchCase{name: "hello", dir: dir, args: []string{"build", "-o", os.DevNull, "."}}
	b.runCase(bc, []benchConfig{benchConfigs[1]}, 1) // with the default flags
	b.printSummary()
	var runs []string
	for _, r := range b.results {
		runs = append(runs, r.Run)
	}
	if want := []string{"nocache", "localwarm", "cold", "warm1", "diskwarm"}; !slices.Equal(runs, want) {
		t.Fatalf("runs = %q; want %q", runs, want)
	}
	for _, r := range b.results {
		if _, err := json.Marshal(r); err != nil {
			t.Errorf("%s: %v", r.Run, err)
		}
	}
	nocache, localwarm, cold, warm, diskwarm := b.results[0], b.results[1], b.results[2], b.results[3], b.results[4]
	if nocache.Stats != nil || localwarm.Stats != nil || nocache.Gets != 0 {
		t.Errorf("nocache and localwarm used a remote cache")
	}
	if diskwarm.LocalHits < warm.RemoteHits || diskwarm.RemoteHits != 0 {
		t.Errorf("diskwarm: %d local hits, %d remote hits; want at least warm's %d hits, all local", diskwarm.LocalHits, diskwarm.RemoteHits, warm.RemoteHits)
	}
	if n := warm.Sizes["get"].count(-1, noLimit); n != warm.RemoteHits {
		t.Errorf("warm: object sizes of %d get hits; want all %d", n, warm.RemoteHits)
	}
	coldMisses, warmMisses := cold.getTier("miss").Gets, warm.getTier("miss").Gets
	if coldMisses == 0 || cold.RemoteHits != 0 || cold.Puts == 0 {
		t.Errorf("cold build: %d misses, %d remote hits, %d puts; want misses and puts, no hits", coldMisses, cold.RemoteHits, cold.Puts)
	}
	// A few actions (such as the link) miss every time.
	if warmMisses*10 > coldMisses || warm.getTier("s3").Gets == 0 || warm.getTier("inline").Gets == 0 {
		t.Errorf("warm build: %d misses (cold: %d), tiers %v; want few misses, and inline and s3 hits", warmMisses, coldMisses, warm.Stats.GetTiers)
	}
	if warm.getPhase("s3", "first_byte").N == 0 || warm.getPhase("s3", "transfer").N == 0 {
		t.Errorf("warm build: s3 phases %v; want first_byte and transfer", warm.GetPhases["s3"])
	}
}

// cacheprogArg, as the first argument to this test binary, makes it run as
// TestBuildBench's GOCACHEPROG instead of running tests. The go command
// doesn't pass its GOCACHEPROG arguments on to the test binaries it runs,
// so nested test runs (as in the tbtest case) still run tests.
const cacheprogArg = "-awscache-buildbench-cacheprog"

func TestMain(m *testing.M) {
	if (len(os.Args) == 5 || len(os.Args) == 6) && os.Args[1] == cacheprogArg {
		var salt string
		if len(os.Args) == 6 {
			salt = os.Args[5]
		}
		os.Exit(runBenchCacheprog(os.Args[2], os.Args[3], os.Args[4], salt))
	}
	os.Exit(m.Run())
}

// cacheprogStats are the stats TestBuildBench's GOCACHEPROG writes to a
// file when the go command closes it.
type cacheprogStats struct {
	Gets, LocalHits, RemoteHits, RemoteErrs int64
	Puts, PutsTimedOut, PutsCanceled        int64
	Drained                                 bool
	DrainSecs                               float64
}

// runBenchCacheprog runs a GOCACHEPROG that works like Tailscale's
// cigocacher: a local disk cache in dir, then the HTTP cache at url, with
// PUTs to the HTTP cache in the background. Unlike cigocacher, it waits for
// all background PUTs at exit, so the next build finds them. It writes
// [cacheprogStats] to statsFile at exit. If salt is non-empty, it salts the
// action IDs it sends to the HTTP cache (see [saltTransport]).
func runBenchCacheprog(url, dir, statsFile, salt string) int {
	disk := &cachers.DiskCache{Dir: dir}
	remote := &cachers.HTTPClient{
		BaseURL:               url,
		Disk:                  disk,
		BestEffortHTTP:        true,
		AsyncPutTimeout:       benchAsyncPutTimeout,
		AsyncPutMaxConcurrent: 10, // as in cigocacher
	}
	if salt != "" {
		// A remote server across the network, as ciguestlet's proxy to
		// cigocached is: keep connections to it open, as that proxy does.
		tr := http.DefaultTransport.(*http.Transport).Clone()
		tr.MaxIdleConnsPerHost = 256
		remote.HTTPClient = &http.Client{Transport: saltTransport{salt: salt, rt: tr}}
	}
	var gets, localHits, remoteHits, remoteErrs, puts atomic.Int64
	p := &cacheproc.Process{
		Get: func(ctx context.Context, actionID string) (outputID, diskPath string, err error) {
			gets.Add(1)
			outputID, diskPath, err = disk.Get(ctx, actionID)
			if err == nil && outputID != "" {
				localHits.Add(1)
				return outputID, diskPath, nil
			}
			outputID, diskPath, err = remote.Get(ctx, actionID)
			if err != nil {
				remoteErrs.Add(1)
				return "", "", nil
			}
			if outputID != "" {
				remoteHits.Add(1)
			}
			return outputID, diskPath, nil
		},
		Put: func(ctx context.Context, actionID, outputID string, size int64, r io.Reader) (string, error) {
			puts.Add(1)
			return remote.Put(ctx, actionID, outputID, size, r)
		},
		Close: func() error {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
			defer cancel()
			start := time.Now()
			drained := remote.Shutdown(ctx)
			st := cacheprogStats{
				Gets:         gets.Load(),
				LocalHits:    localHits.Load(),
				RemoteHits:   remoteHits.Load(),
				RemoteErrs:   remoteErrs.Load(),
				Puts:         puts.Load(),
				PutsTimedOut: remote.PutsTimedOut.Load(),
				PutsCanceled: remote.PutsCanceled.Load(),
				Drained:      drained,
				DrainSecs:    time.Since(start).Seconds(),
			}
			j, err := json.Marshal(st)
			if err != nil {
				return err
			}
			return os.WriteFile(statsFile, j, 0644)
		},
	}
	if err := p.Run(); err != nil {
		log.Printf("buildbench cacheprog: %v", err)
		return 1
	}
	return 0
}

// benchAsyncPutTimeout is the same as cigocacher's timeout for background
// PUTs: the time to send size bytes at 2MiB/s, clamped to [5s, 30s].
func benchAsyncPutTimeout(size int64) time.Duration {
	const (
		minAverageBandwidth = 2 * 1 << 20 / float64(time.Second) // bytes per nanosecond
		minPutTimeout       = 5 * time.Second
		maxPutTimeout       = 30 * time.Second
	)
	timeout := time.Duration(float64(size) / minAverageBandwidth)
	return min(max(minPutTimeout, timeout), maxPutTimeout)
}

func init() {
	// benchGeneration puts the case index (two digits) and the config
	// index (one digit) in the last three decimal digits.
	if len(benchCases) > 100 || len(benchConfigs) > 10 {
		panic("too many bench cases or configs for benchGeneration")
	}
}
