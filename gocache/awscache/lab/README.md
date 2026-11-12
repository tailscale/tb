# awscache lab

This Terraform module creates a small lab environment in which to test and
benchmark `gocache/awscache` against real AWS. Use a sandbox account. It
creates:

- a DynamoDB table for action metadata;
- an S3 Standard bucket for blobs;
- an S3 Express One Zone directory bucket, in `var.az_id`;
- an EC2 VM in the same AZ, with Go and checkouts of this repo and
  [tailscale/tailscale], to run the tests from;
- a VPC with gateway endpoints for S3, S3 Express, and DynamoDB, as in the
  corp `ci` VPC;
- a gocached VM with instance-store NVMe in the same subnet (unless
  `var.enable_gocached` is false), for the `gocached` and `ddbgocached`
  configs (see "gocached");
- security groups that allow SSH to the VMs from `var.my_ip` only, and
  gocached only from the benchmark VM;
- IAM for the VM: the cache's permissions, plus SSM Session Manager.

The design is in `ci/doc/aws-native-gocached.md` in the corp repo, and the
benchmark results in `ci/doc/aws-native-gocached-benchmarks.md`. RUNS.md
lists the benchmark runs behind them, and how to reproduce each one.

[tailscale/tailscale]: https://github.com/tailscale/tailscale

## End to end

You need Terraform 1.6 or later, the AWS CLI v2, and an aws-vault profile
for the sandbox account. The commands call it `sandbox`. Each step is
explained in more detail in the sections that follow.

1. Push this branch, so the VM can clone it:

   ```sh
   git push origin tomhjp/mid-object-cache
   ```

2. Create the lab, from this directory:

   ```sh
   aws-vault exec sandbox --no-session -- terraform init
   aws-vault exec sandbox --no-session -- terraform apply -var my_ip=$(curl -s https://checkip.amazonaws.com)
   ```

3. Wait two or three minutes for the VM to set itself up, then connect:

   ```sh
   aws-vault exec sandbox -- $(terraform output -raw ssh_command)
   ```

   Check that setup has finished: `~/tb` and `~/tailscale` exist, and
   `tail /var/log/cloud-init-output.log` doesn't show errors.

4. On the VM, check that it can use the table and both buckets:

   ```sh
   cd ~/tb
   git pull
   go test ./gocache/awscache -run '^TestRealAWS$' -v
   ```

5. Run the benchmark in tmux, so that it continues if your connection
   drops (reattach with `tmux attach`):

   ```sh
   tmux new -s bench
   go test ./gocache/awscache -run '^TestBuildBench$' -v -timeout 4h \
       -buildbench -buildbench.out=$HOME/results.jsonl 2>&1 | tee ~/bench.log
   ```

   The defaults do 5 builds of the `tstest` case: no cache, warm local
   cache, cold remote cache, warm remote cache, and warm GOCACHEPROG disk
   cache. At the end, the log has tables of the results. See "Running the
   tests" for more builds and configs.

6. Copy the results to your laptop through the S3 Standard bucket. On the
   VM:

   ```sh
   aws s3 cp ~/results.jsonl s3://$AWSCACHE_TEST_BUCKET/results/
   aws s3 cp ~/bench.log s3://$AWSCACHE_TEST_BUCKET/results/
   ```

   Then on your laptop (the bucket's lifecycle rule deletes them after
   `var.blob_expiry_days`):

   ```sh
   aws-vault exec sandbox -- aws s3 cp --region $(terraform output -raw region) --recursive s3://$(terraform output -raw bucket)/results/ .
   ```

7. Stop the VM until the next run, or destroy the lab (see "Stopping and
   destroying").

## Cost

The VM costs the most: a `c7i.4xlarge` is about $0.71/hour on demand, plus
about $50/month for its gp3 volume (100GB with 8000 IOPS and 500MB/s). Stop
the VM when you don't use it (see below). The gocached VM, an
`i7i.xlarge`, is about $0.40/hour more. DynamoDB is on demand, and a
benchmark run stores a few GB at most, so the rest costs cents.

## Deploying

From your laptop, with credentials for the sandbox account:

```sh
cd gocache/awscache/lab
aws-vault exec sandbox -- terraform init
aws-vault exec sandbox -- terraform apply -var my_ip=$(curl -s https://checkip.amazonaws.com)
```

`my_ip` is required: the VM accepts SSH from that IPv4 address only. If your
address changes, apply again with the new one. To avoid typing it every
time, put `my_ip = "<address>"` in `terraform.tfvars`, which Git ignores.

IAM API calls fail with session credentials from `GetSessionToken`. If you
get `InvalidClientTokenId` errors for IAM resources, use
`aws-vault exec --no-session`, or a profile that assumes a role.

The Terraform state is local, in this directory. Don't commit it, and
don't lose it: anything that deletes ignored files (like `git clean -X`)
deletes it too. If you lose it, see "Recovering lost state".

The VM takes a few minutes after `apply` to install Go and clone the repos
(see `user-data.sh.tftpl`; the log is in `/var/log/cloud-init-output.log`).
It checks out `var.repo_branch` of this repo, if it has been pushed, and
`var.tailscale_ref` of tailscale/tailscale. The benchmark logs the commit
of each repo it builds.

## Connecting

With SSH, from `my_ip`. EC2 Instance Connect sends a temporary key for each
connection, so there's no key pair to manage:

```sh
aws-vault exec sandbox -- $(terraform output -raw ssh_command)
cd tb
git pull
```

Or with SSM Session Manager, from anywhere. You need the
[Session Manager plugin] for the AWS CLI:

```sh
aws-vault exec sandbox -- $(terraform output -raw ssm_command)
sudo -iu ec2-user
```

The login shell has `go` on its `PATH` and the environment the tests need
(`terraform output test_env`). `TMPDIR` is on the root volume, because `/tmp`
is a tmpfs.

[Session Manager plugin]: https://docs.aws.amazon.com/systems-manager/latest/userguide/session-manager-working-with-install-plugin.html

## Running the tests

First, check that the VM can use the table and both buckets:

```sh
go test ./gocache/awscache -run '^TestRealAWS$' -v
```

Then run the build benchmark. With the default flags, it builds the
`tstest` case (compile and link every test binary in tailscale/tailscale)
5 times, with the `express` config (blobs in the S3 Express One Zone
bucket):

```sh
go test ./gocache/awscache -run '^TestBuildBench$' -v -timeout 4h \
    -buildbench -buildbench.out=$HOME/results.jsonl
```

For each build case, the benchmark does these builds:

| Run | Remote cache | Local cache | Shows |
|---|---|---|---|
| `nocache` | none | empty | the build without any cache |
| `localwarm` | none | GOCACHE filled by `nocache` | the usual warm build on a developer's machine |
| `cold` | empty | empty | the cost of filling the remote cache |
| `warm1`, ... | filled by `cold` | empty | a CI job in a new VM |
| `diskwarm` | filled by `cold` | the GOCACHEPROG's disk cache filled by `cold` | the GOCACHEPROG with every hit from local disk |

`cold` and `warmN` happen for each storage config, and `diskwarm` once,
after the first config. So `warm1` minus `diskwarm` is the cost of having the
cache remote, and `diskwarm` minus `localwarm` is the cost of the GOCACHEPROG
protocol. `-buildbench.local=false` skips `nocache`, `localwarm`, and
`diskwarm`.

Every remote build uses a new `Cache`, like a CI job in a new VM. The
GOCACHEPROG works like cigocacher, but at exit it waits for all background
PUTs, so the warm builds find everything. The "drain" column is that wait,
which is part of the wall time.

To compare with S3 Standard, add the `s3` config. Each config adds a cold
build and `-buildbench.warm` warm builds of each case:

```sh
go test ./gocache/awscache -run '^TestBuildBench$' -v -timeout 4h \
    -buildbench -buildbench.out=$HOME/results.jsonl \
    -buildbench.configs=express,s3 -buildbench.warm=2
```

At the end, it logs these tables:

- an overview of each build: wall time, hits by tier (inline from DynamoDB,
  or s3 for blobs), misses, uploads, and the p50 lookup, time to first
  byte, and transfer time of blobs;
- the total time that each build's gets spent in each phase, by tier:
  DynamoDB lookup, blob first byte, and transfer (streaming the blob from
  S3 to the GOCACHEPROG);
- the uncompressed sizes of the objects that each build's gets hit;
- the total time that each build's puts spent in each phase: the DynamoDB
  existence check, receiving the body, checking for the blob, uploading it,
  and writing the action item.

The totals are sums over concurrent requests, so they can be more than the
wall time. They show where the time goes, not how much of the wall time
each phase costs.

With `-buildbench.out`, it also appends each result as a JSON line, with the
handler's whole `/session/stats` (see `Stats`), the mean, p50, and p95 of
every phase for every tier, and the object size histograms.

| Flag | Default | Meaning |
|---|---|---|
| `-buildbench` | `false` | Run the benchmark. |
| `-buildbench.cases` | `tstest` | Build cases; see below. |
| `-buildbench.tailscale` | `$AWSCACHE_BENCH_TAILSCALE_DIR` | Checkout of tailscale/tailscale for the Tailscale cases. The VM sets it to `~/tailscale`. |
| `-buildbench.configs` | `express` | Storage configs: `express` (blobs in the S3 Express One Zone bucket, `$AWSCACHE_TEST_EXPRESS_BUCKET`), `s3` (blobs in the S3 Standard bucket, `$AWSCACHE_TEST_BUCKET`), `gocached`, and `ddbgocached` (see "gocached"), and `mid512k`, `mid4m`, and `midall` (see "Mid-size tier"). |
| `-buildbench.gocached` | `$AWSCACHE_BENCH_GOCACHED_URL` | gocached's URL, for the `gocached` and `ddbgocached` configs: `terraform output -raw gocached_url`. |
| `-buildbench.warm` | `1` | Warm builds per case and config. |
| `-buildbench.local` | `true` | Do the `nocache`, `localwarm`, and `diskwarm` builds. |
| `-buildbench.gen` | new | Base generation. See below. |
| `-buildbench.cold` | `true` | Do the cold builds. If `false`, `-buildbench.gen` is required, and there's no `diskwarm` build. |
| `-buildbench.out` | none | File to append JSON results to. |

The build cases:

| Case | Command | Where | Cache gets (cold / warm) |
|---|---|---|---|
| `tstest` | `go test -vet=off -count=1 -exec=/bin/true ./...` | tailscale/tailscale | about 7k / 10k (estimated) |
| `tsall` | `go build -o <empty dir>/ ./cmd/tailscale ./cmd/tailscaled ./cmd/tailscale/cli` | tailscale/tailscale | 1.5k / 2.3k |
| `tailscale` | `go build -o /dev/null ./cmd/tailscale` | tailscale/tailscale | |
| `tailscaled` | `go build -o /dev/null ./cmd/tailscaled` | tailscale/tailscale | |
| `tailscalecli` | `go build ./cmd/tailscale/cli` (a library, so compile only) | tailscale/tailscale | |
| `std` | `go build std` | anywhere | |
| `cmd` | `go build cmd` | anywhere | |
| `tb` | `go build ./...` | this repo | |
| `tbvet` | `go vet ./...` | this repo | |
| `tbtest` | `go test -count=1 -run=^$ ./...` | this repo | |

`tstest` is like corp CI's `go test -vet=off tailscale.io/...`, for the OSS
repo: it compiles every package and its tests, and links every test binary.
`-exec=/bin/true` runs `/bin/true` instead of each test binary, so no tests
run. `go build -o` links the binaries, and then discards them. Linking
takes time that the cache can't save; the `localwarm` build shows how much.

## gocached

Two configs use the lab's gocached VM, whose cache is on instance-store
NVMe:

- `gocached`: the GOCACHEPROG talks to gocached directly, with no `Cache`
  and no DynamoDB. This is the design of today's ci-gocached-aws-1, though
  without S3 Files behind it, nor ciguestlet's TLS proxy in front of it.
- `ddbgocached`: a simulation of the NVMe cache pod design. Metadata is in
  DynamoDB, as usual, and blobs are in gocached (playing a pod), in front of
  the S3 Standard bucket. PUTs write each blob to S3 and then to gocached;
  GETs read from gocached and fall back to S3. There's one pod, not a
  sharded pool.

gocached has no auth here, so it has one namespace. For each run, the
`gocached` config salts its action IDs, and `ddbgocached` uses blob keys
with the run's prefix, so cold builds start empty all the same. gocached
still deduplicates blob content across runs, so `gocached` cold builds
upload less than the first one did.

Set gocached's URL (`terraform output gocached_url`) on the benchmark VM.
It's a fixed address, so it doesn't change if the gocached VM is replaced:

```sh
echo 'export AWSCACHE_BENCH_GOCACHED_URL=http://10.88.0.250:31364' >> ~/.bashrc
source ~/.bashrc
```

The gocached VM builds gocached from `var.repo_branch` and runs it with
systemd. Its NVMe is empty after every stop and start; the service formats
and mounts it again before it starts. To look at it:
`$(terraform output -raw gocached_ssh_command)`, then
`systemctl status gocached`, `journalctl -u gocached`, and
`curl localhost:31365/metrics`.

## Mid-size tier

The `mid512k`, `mid4m`, and `midall` configs use the library's mid-size
tier (`Config.MidBucket` and `Config.MidMax`): blobs of objects up to 512KiB,
4MiB, or any size (uncompressed) go to the S3 Express One Zone bucket, and
larger ones to the S3 Standard bucket. Each action item records which
bucket holds its blob. The tables report the S3 Express hits as the `mid`
tier, and the S3 Standard ones as `s3`.

`midall` stores the same blobs in the same place as `express`, through the
mid-size tier's code rather than with the S3 Express bucket as the only
bucket.

## Wiping the cache

You don't need to. Each case and config of each run uses its own global
generation (the run's base generation times 1000, plus 10 times the case's
index, plus the config's index) and its own blob prefix
(`buildbench/<generation>/`), so every cold build starts with an empty
cache. The base generation is the Unix time at the start of the run, and the
test logs it.

To repeat only the warm builds against an earlier run's cache, give that
run's base generation:

```sh
go test ./gocache/awscache -run '^TestBuildBench$' -v -timeout 4h \
    -buildbench -buildbench.gen=<base generation> -buildbench.cold=false
```

Old data deletes itself: the benchmark's DynamoDB items expire through TTL
after 2 days, and the S3 Standard bucket's lifecycle rule deletes blobs after
`var.blob_expiry_days`. The directory bucket has no lifecycle rule; its
blobs stay until you destroy the lab, or until you delete them with
`aws s3 rm --recursive s3://<bucket>/buildbench/`.

## Recovering lost state

If the lab exists but its Terraform state is gone, `terraform apply` would
try to create everything again, and fail on the names that already exist.
`import-existing.sh` finds the lab's resources with the AWS CLI and writes
Terraform `import` blocks for them, so that the next apply adopts them
instead:

```sh
aws-vault exec sandbox -- ./import-existing.sh <region> > imports.tf
aws-vault exec sandbox --no-session -- terraform init
aws-vault exec sandbox --no-session -- terraform plan -var my_ip=$(curl -s https://checkip.amazonaws.com)
```

Use the region the lab is in (and add `-var region=...` and `-var
az_id=...` to the plan, unless they're in `terraform.tfvars`). If you
didn't use the default `var.name`, pass it as a second argument. Check
`imports.tf`: a resource the script couldn't find gets a comment, and
Terraform creates it.

The plan should show each resource as "will be imported", with at most
small in-place updates (for example `force_destroy`, which isn't stored in
AWS), and new resources only for what didn't exist before (such as the
gocached VM). If it wants to replace anything, find out why before you
apply. Then apply with the same arguments, and delete `imports.tf`.

The VMs ignore changes to their setup scripts, so an import doesn't replace
them. To rebuild one with the current script, apply with
`-replace=aws_instance.vm` or `-replace='aws_instance.gocached[0]'`.

## Stopping and destroying

Stop the VMs when you don't need them, and start them again later:

```sh
aws-vault exec sandbox -- aws ec2 stop-instances --region $(terraform output -raw region) --instance-ids $(terraform output -raw instance_id) $(terraform output -raw gocached_instance_id)
aws-vault exec sandbox -- aws ec2 start-instances --region $(terraform output -raw region) --instance-ids $(terraform output -raw instance_id) $(terraform output -raw gocached_instance_id)
```

When you're done, destroy everything, including the buckets' contents:

```sh
aws-vault exec sandbox -- terraform destroy
```

## Caveats

- CI runners are VMs on metal hosts, and ciguestlet on the host serves
  their cache requests. Here, the test process serves them on the same VM
  as the build. Network paths and CPU differ from CI.
- One VM gives one client's view. It doesn't measure S3 or DynamoDB under
  the fleet's load, nor throttling.
- The `cmd` and `std` cases build Go's own source, so their results depend
  on the Go version.
