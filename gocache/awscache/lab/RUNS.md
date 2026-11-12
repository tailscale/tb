# Benchmark runs

The runs of `TestBuildBench` that informed the design of the AWS-native Go
build cache, and how to reproduce them. The results and their analysis are
in `ci/doc/aws-native-gocached-benchmarks.md` in Tailscale's corp repo;
this file is about the code and commands behind them. See README.md for
setting up the lab and running the benchmark.

## Common setup

Every run used the lab in eu-west-2 (one AZ), with the default
`c7i.4xlarge` benchmark VM, Go 1.27.4, and tailscale/tailscale at
`e3ecbb9894cdcb3805e4ffc1c63c08aecb454316` for the Tailscale build cases.
Each run uses new generations and blob prefixes, so its cold builds start
empty. Compare configs within a run: between runs, every build (local ones
too) moved by about a second.

## Which commit has what

On this branch, each config needs the commit that added it, or a later
one:

| Configs, flags | Commit |
|---|---|
| `s3`, `express`; the `tstest` and `tsall` cases; `nocache`, `localwarm`, `diskwarm` builds | gocache/awscache: add a build benchmark and a lab to run it in |
| `gocached`, `ddbgocached` (and the lab's gocached VM) | gocache/awscache: benchmark gocached and a simulated NVMe cache pod |
| `mid512k`, `mid4m`, `midall` | gocache/awscache: add a mid-size tier, for benchmarking |

The branch's commits before the benchmark changed what a GET does: the
first streams blobs instead of buffering them, and the second checks
their hashes while streaming. Runs 1 and 2 predate both.

## The runs

The exact commit of each run is tagged `bench/run<N>-...`. The tagged
commits are work in progress from before this branch was split into
commits, so their code differs from this branch's. Their lab used
`$AWSCACHE_TEST_MID_BUCKET` where this branch uses
`$AWSCACHE_TEST_EXPRESS_BUCKET`; export both to run them.

### Run 1: a first look

- Question: does the cache work end to end, and what does a warm build
  cost?
- Exact code: `bench/run1-tsall`. GETs buffered each blob and verified its
  hash before sending it.
- Command (that commit's defaults: the `tsall` case, the `mid4m` config,
  one warm build, and a build with no cache):

  ```sh
  go test ./gocache/awscache -run '^TestBuildBench$' -v -timeout 4h \
      -buildbench -buildbench.out=$HOME/results.jsonl
  ```

- On this branch (GETs stream; adds `localwarm` and `diskwarm` builds):

  ```sh
  go test ./gocache/awscache -run '^TestBuildBench$' -v -timeout 4h \
      -buildbench -buildbench.cases=tsall -buildbench.configs=mid4m
  ```

- Result: 2.3k warm gets; no cache 29.1 s, cold 31.3 s, warm 3.7 s.

### Run 2: a corp-sized build, S3 Express against S3 Standard

- Question: with a build closer to corp CI's size, how much does S3
  Express save over S3 Standard, and what does a remote cache cost over a
  local one?
- Exact code: `bench/run2-tstest-buffered`. GETs buffered and verified
  blobs. (Its JSON output lost the remote builds' results; the log has
  them.)
- Command (that commit's defaults: the `tstest` case, one warm build, and
  the `nocache`, `localwarm`, and `diskwarm` builds):

  ```sh
  go test ./gocache/awscache -run '^TestBuildBench$' -v -timeout 4h \
      -buildbench -buildbench.configs=mid4m,s3 \
      -buildbench.out=$HOME/results2.jsonl
  ```

- On this branch (GETs stream):

  ```sh
  go test ./gocache/awscache -run '^TestBuildBench$' -v -timeout 4h \
      -buildbench -buildbench.configs=mid4m,s3
  ```

- Result: 8.1k warm gets; no cache 112.6 s, local warm 11.7 s, disk warm
  13.2 s, `mid4m` warm 20.6 s, `s3` warm 32.8 s.

### Run 3: size caps, with streaming GETs

- Question: should S3 Express hold blobs up to 512KiB, up to 4MiB, or all
  of them, and does streaming GETs help?
- Exact code: `bench/run3-tstest-sizecaps`. GETs stream blobs and check
  their hashes on the way, as on this branch.
- Command, there and on this branch:

  ```sh
  go test ./gocache/awscache -run '^TestBuildBench$' -v -timeout 4h \
      -buildbench -buildbench.configs=mid4m,midall,mid512k,s3 -buildbench.warm=2 \
      -buildbench.out=$HOME/results3.jsonl
  ```

- Result: disk warm 12.0 s; warm `midall` 18.6 s, `mid4m` 19.4 s,
  `mid512k` 21.0 s, `s3` 30.6 s (means of two). Streaming didn't change the
  remote cost measurably.

### Run 4: gocached, and a simulated NVMe cache pod

- Question: how does the AWS-native design compare with today's gocached,
  and with the NVMe cache pod design, measured rather than estimated?
- Exact code: `bench/run4-tstest-gocached`, with the lab's gocached VM.
- Command, there and on this branch (with `$AWSCACHE_BENCH_GOCACHED_URL`
  set; see README.md):

  ```sh
  go test ./gocache/awscache -run '^TestBuildBench$' -v -timeout 4h \
      -buildbench -buildbench.configs=express,gocached,ddbgocached,s3 -buildbench.warm=2 \
      -buildbench.out=$HOME/results4.jsonl
  ```

- Result: no cache 125.1 s (13 s more than in runs 2 and 3), local warm
  12.3 s, disk warm 12.3 s; warm `gocached` 14.0 s, `ddbgocached` 17.25 s,
  `express` 18.85 s, `s3` 30.8 s (means of two).
