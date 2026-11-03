# mantsana: headless macOS VMs for CI, replacing Tart

Status: design + prototype verified on hardware. This document is the
source of truth for requirements and decisions; update it as they change.

The name: it has "ts" in it, sounds like Spanish *manzana* (apple), means
"apple" in Western Huasteca Nahuatl, and in some Malagasy dialects describes
something quick and nimble.

## 1. Goal

`mantsana` runs ephemeral macOS guest VMs on Apple Silicon Macs for
Tailscale's CI system. It replaces [Tart](https://tart.run) and the `pf`
anchors in the corp macOS stack, and nothing else: corp's `cihostlet` and its
per-slot `ciguestlet` processes stay, and `ciguestlet` gains a mantsana VM
backend next to its Tart one (section 4.8). cimgr, the cihostlet API, the
guest state machine and the GitHub runner recipe are unchanged.

mantsana is Go packages (and development tools) built on Apple's
Virtualization.framework via
[`github.com/Code-Hex/vz/v3`](https://github.com/Code-Hex/vz), which
`ciguestlet` links in-process. No Swift, no AppKit window, no App Store
signing or notarization: we `go build` on the host Mac and ad-hoc
`codesign` with the virtualization entitlement.

It does exactly what our CI needs and not more:

* Boot a macOS guest from a base image in seconds, normally by restoring a
  saved memory snapshot rather than cold booting.
* Throw the guest away afterwards. All guest disk writes are discarded.
* Give each guest its own isolated network with a metadata service (IMDS),
  intercepted DNS, and logged egress flows.
* Provide screenshots of the guest display for debugging, with no GUI on the
  host (mechanism open; section 4.5).
* Let `ciguestlet` keep running the guest-side CI recipe (GitHub runner JIT
  config, job-start hook, gomodfs NFS, gocache and git proxies) with its
  services plugged into the guest's network instead of `pf` rules.

Non-goals for now: OCI registries, GUI, running on Intel Macs, Linux/Windows
guests (planned later, see section 12), nested virtualization, hosting
anything but CI workloads.

## 2. Requirements log

Collected from the initial brief. Add new ones here with dates.

2026-09-29, initial:

* Go, using `github.com/Code-Hex/vz/v3`. Fix problems in vz upstream (or in a
  fork) rather than working around them.
* Headless Go server. No macOS GUI.
* No signing/notarization. Compile on the host Mac, ad-hoc sign for the
  `com.apple.security.virtualization` entitlement.
* Replaces the macOS path in corp `tailscale.io` CI (`ciguestlet`'s
  `tart.go` and `cihostlet`'s `tart.go`).
* All VMs ephemeral. Boot fast (seconds to usable), typically from a restore
  snapshot, then only fix up DHCP/ARP state. Guest writes are discarded via an
  NBD block device backed by the discard-writes NBD server we already use on
  Linux (`github.com/tailscale/tb/guestbd`). No ephemeral disk files.
* Disk images come from the network and will be lazily synced later. For now
  the base image is plain files already on local disk. Content-addressed
  chunks and incremental sync come later.
* Each VM in its own isolated network, so guests cannot talk to each other.
  Two macOS guests max per host (framework limit); more slots later for Linux.
* An IMDS metadata IP providing services to the guest.
* Later: intercept and log all DNS, log all outgoing TCP/HTTP flows per guest
  for accounting, intercept TCP to the IMDS IP for services. (The design does
  these from day one because the network architecture makes them free.)
* Maybe share code with `tailscale/tailvisor`'s Go `netd`, but not its Swift.
* Internal VNC to render screenshots of each guest slot.
* macOS guests first; Linux guests later.

2026-09-29, later:

* Name is `mantsana`, not `vzd`. At least the Apple-specific parts are in
  `./cmd/mantsana`.
* It may take over both the `cihostlet` and `ciguestlet` roles in one
  process (decided: it does), but then it must act like the existing
  `cihostlet` API so corp `cimgr` can create VMs on it unchanged.
* Move the cimgr/hostlet API types from corp into this repo. The corp
  `ciguestlet` and `cihostlet` code will move here later too.
* `setec` is open source: depend on it directly and support it first class
  for secrets (the GitHub App key for runner JIT configs).

2026-09-30:

* mantsana uses the Mac's own tailscaled (its tailnet address for the
  cihostlet API listener, LocalAPI WhoIs for callers), as cihostlet does;
  it is not a tsnet node.
* Framework facts come from Apple's documentation and our own experiments,
  not from reading other hypervisors' source.
* `cmd/oci-img-dl` downloads registry images straight into mantsana images,
  stored under `~/Library/Application Support/mantsana`.
* Keep corp's cihostlet and ciguestlet; replace only their Tart part. They
  run as separate processes so a cihostlet deploy doesn't interrupt builds
  running in guests; a one-process design would regress that, because a VM
  (and its in-process network and disk servers) can't outlive the process
  that created it. ciguestlet gets a mantsana backend and has 1:1 shared
  fate with its VM.
* The mantsana backend coexists with the Tart one for a while, so Macs can
  opt in one at a time: a cihostlet flag selects the backend and is passed
  down to ciguestlet.

2026-11-03:

* Don't move the cimgr/hostlet API types from corp yet; corp shouldn't be
  half migrated to this repo.

## 3. What exists today (research summary)

### 3.1 Corp CI (`tailscale.io`, `cmd/cihostlet` + `cmd/ciguestlet`)

`cihostlet` runs one per Mac (launchd daemon `com.ci.hostlet`) and owns
slots. Per slot it launches one `ciguestlet` process (launchd
`com.ci.<host>-<slot>`, `KeepAlive=false`) which supervises exactly one VM.
`ciguestlet` is host-side; nothing of ours runs inside the guest except a
baked-in `on-job-started.sh` hook. macOS defaults: `--guestlets=1
--max-guestlets=2` (hard cap 2 on darwin).

The macOS path in `ciguestlet/tart.go` today:

```
tart list --format json            # stop+delete stale VMs with the slot prefix
tart clone <image> <name>
tart set --cpu N <name>
tart set --memory MiB <name>
tart run --no-audio --no-clipboard --root-disk-opts=sync=none,caching=cached
         [--no-graphics] [--vnc-experimental] <name>     (env CI=... so VNC URL is printed)
tart ip <name>                     # polled every 1s, forever
ssh admin@<ip> (password admin)    # polled every 250ms, forever
   sudo route -n add -host 169.254.169.254 <gw>
   sudo route -n add -host 169.254.169.253 <gw>
   sudo networksetup -setdnsservers Ethernet 169.254.169.253
   mount -t nfs ... <gw>:/ /Users/admin/go/pkg/mod      # gomodfs from the host
   tar -xvf - (on-job-started.sh, marker files)
   ACTIONS_RUNNER_HOOK_JOB_STARTED=... run.sh --jitconfig ...
tart stop <name>; tart delete <name>
```

Networking on macOS is vmnet NAT shared by all Tart VMs on the host, which
forces: ephemeral ports for the per-VM guest server and DNS proxy, a
`guestOnlyListener` peer filter, per-slot `pf` anchors
(`rdr ... to 169.254.169.254 port 80 -> host:port`, DNS rdr, blocks for the
CGNAT range), and a leaked-anchor sweeper (tailscale/corp#45834). The
`route -n add` calls exist because 169.254/16 is on-link (APIPA) in the guest
and would never be routed to the gateway.

IMDS surface the guest uses (must be preserved):

| Guest-visible address | Service |
|---|---|
| `169.254.169.254:80` | guest server: `POST /<token>` job-start hook (body: `env -0`) |
| `169.254.169.254:31364` | Go build cache proxy (`GOCACHEPROG`); disabled on darwin today |
| `169.254.169.253:53` | DNS (also every port 53 flow is redirected here) |
| `<gateway>:2049` | gomodfs NFSv3, read-only |
| `<gateway>:9418` | git proxy (opt-in) |

Fast boot on Linux/QEMU today (Windows, FreeBSD guests) restores a migration
state file, then re-DHCPs the guest (Windows: `guest-exec ipconfig /renew`;
FreeBSD: hot-plug the NIC after `cont` so `devd` runs `dhclient`). Firecracker
picks a new guest MAC per boot so the host flushes its ARP entry. None of this
exists for macOS today: every macOS job is a full cold boot.

The Linux NBD path: `guestbd.NewServer(guestbd.FileSource(img),
guestbd.WithSharedSnapshot(), guestbd.WithMaxMem(0))` served over TCP to
`/dev/nbdN` via a custom ioctl-based client. Writes go to an unlinked temp file
and vanish on disconnect.

The `ciguestlet` states cihostlet watches (`statustype.GuestState`) must keep
being emitted by whatever replaces `tart.go`'s `guest.Run`; nothing above that
seam needs to change.

The cimgr <-> cihostlet contract (plain HTTP over the tailnet, no
request-level auth beyond binding to the tailnet address):

* cimgr to hostlet, on `<tailnet-name>:8692`:
  `POST /api/vms` (`guestlet.Opts`, strict decode; 201 + `guestlet.Guestlet`;
  503 when draining or out of capacity, which makes cimgr try the next host),
  `DELETE /api/vms/{name}`, `POST /api/vms/{name}/ttl`
  (`hostlet.VMTTLRequest`/`Response`),
  `GET /api/vms/{name}/boot-log?stream=true` (NDJSON `bootlog.Event`),
  `POST /api/drain` (`hostlet.DrainRequest`). Also served: `GET /api/vms`,
  `GET /api/vms/{name}`, `GET /api/drain`, and `POST /api/vms/{name}/ssh`, an
  `Upgrade: ssh` hijack returning `Ssh-Username`/`Ssh-Password` headers,
  authorized by `Guestlet.SSHBearerToken` or the creator's WhoIs login.
* Hostlet to cimgr: a websocket to `ws://ci-mgr/hostinfo-ws` carrying
  `worktype.HostWSMessage{hello|ping|guest-update}` with a
  `worktype.HostInfo` (name, OS, request endpoint, runner labels, max and
  baseline guestlets, `Guestlets map[ciid.GuestletName]*guestlet.Guestlet`,
  draining).
* cimgr treats a VM as ready from its `GuestState` (`github-runner-waiting`,
  `github-runner-running-job`, `ready`, ...), and as having started its job
  from `StartedJob`. Slots `1..--guestlets` are baseline runners the hostlet
  recreates forever; the rest are allocated for API requests. Names are
  `<host>-<slot>-<unixtime>`.
* The shared types (`ci/ciid`, `labels`, `cinet`, `guestlettype`,
  `statustype`, `hostlet`, `guestlet`, `bootlog`, `worktype`,
  `cimgr/cimgrapi`) are a closed, stdlib-only import graph. `vmimage` mixes
  types with filesystem and exec code and needs splitting first.
* The GitHub runner JIT config is generated by ciguestlet itself
  (`Actions.GenerateOrgJITConfig` with a GitHub App key from setec); cimgr
  never sees it.

### 3.2 Virtualization.framework (Apple's documentation)

Framework behavior comes from Apple's
[Virtualization documentation](https://developer.apple.com/documentation/virtualization)
and from our own measurements on hardware (section 9). The facts we rely
on, as documented:

* `VZFileHandleNetworkDeviceAttachment` (macOS 11) maps a network interface
  to a *connected datagram socket* and carries data at the data link layer;
  it needs no `com.apple.vm.networking` entitlement. Its
  `maximumTransmissionUnit` (macOS 13) defaults to 1500 and must be within
  1500..65535, or the configuration is invalid. The socket's `SO_RCVBUF`
  must be at least twice its `SO_SNDBUF`, and four times is recommended.
* `VZNetworkBlockDeviceStorageDeviceAttachment` (macOS 14) is an NBD client
  for a server outside the framework, addressed by an NBD URI (per the NBD
  project's URI spec, which includes `nbd+unix:///<export>?socket=<path>`).
  It connects at `start()`, not when the attachment is created, reconnects
  on recoverable errors throughout the VM's life, and requires the
  `com.apple.security.network.client` entitlement.
* `saveMachineStateTo(url:)` (macOS 14) requires a paused VM and leaves it
  paused. `restoreMachineStateFrom(url:)` requires a stopped VM whose
  configuration is compatible with the saved state, and leaves it paused,
  to be resumed. `validateSaveRestoreSupport()` reports whether a
  configuration can be saved; Apple does not list which devices can't.
  Apple does not document the state file's encryption or portability.
* `VZErrorVirtualMachineLimitExceeded`: starting a VM would exceed the
  system's limit on simultaneously running VMs. For macOS guests the limit
  is two, which corp's cihostlet also caps.
* `VZMacTrackpadConfiguration` (macOS 13) is used by guests running macOS 13
  and later; `VZMacKeyboardConfiguration` (macOS 14) carries Mac-specific
  keys. Apple documents both in terms of `VZVirtualMachineView` and says
  nothing about save/restore.
* The only guest display path Apple documents is `VZVirtualMachineView`, an
  AppKit view. There is no documented headless framebuffer or screenshot
  API.

### 3.3 tailvisor `netd` (Go)

A single-guest userspace L2-to-L4 gateway: raw Ethernet frames over a unix
datagram socket, gVisor netstack (`ipv4`, `tcp`, `icmp4`), promiscuous and
spoofing NIC, `tcp.NewForwarder` terminating every TCP flow and re-dialing it
through tsnet, hand-rolled ARP (gateway only), DHCP (fixed lease), DNS, ICMP.
Clean and reusable in structure, but: single-instance globals (UDP session
table, stats), tsnet-only egress, no IMDS, no IPv6, no tests, nested module in
a Swift-first repo. Decision: copy the architecture and selected code, don't
import it (section 4.3).

### 3.4 `guestbd` (Go)

Library NBD server: fixed newstyle handshake, `NBD_OPT_GO`, READ/WRITE/FLUSH
(no-op)/TRIM/DISC, base image via `BaseImage` interface (`io.ReaderAt` +
`Size`), per-connection or shared in-memory/temp-file COW snapshot with
SHA-256 page dedup, `Serve(net.Listener)` so it works over a unix socket.
Well tested including against the Linux kernel client. Decision: import it.
Its `BaseImage` seam is where lazy network fetching plugs in later.

### 3.5 `Code-Hex/vz` (Go)

Everything we need is bound: macOS platform/bootloader/installer,
save/restore (`SaveMachineStateToPath`, `RestoreMachineStateFromURL`,
`ValidateSaveRestoreSupport`, macOS 14+), `FileHandleNetworkDeviceAttachment`
(must be `SOCK_DGRAM`; raw Ethernet frames one per datagram; MTU settable
1500..65535 on macOS 13+), `NetworkBlockDeviceStorageDeviceAttachment`
(macOS 14+, NBD URI, reconnect timeout, `Connected()` / `DidEncounterError()`
channels), vsock, serial console over file handles, graphics device config,
Mac keyboard/trackpad. No framebuffer access. Per-VM dispatch queue,
so no main-thread requirement unless `StartGraphicApplication` is called
(we never call it).

## 4. Architecture

```
  cimgr ── cihostlet (root, LaunchDaemon; unchanged API, slots, TTLs, drain, HostInfo)
  (tailnet)   │  launchctl bootstrap user/<uid> com.ci.<host>-<slot>  (one job per slot)
              ▼
   ┌──────────────────── ciguestlet --vm-backend=mantsana (one process per VM, as the CI user) ───────┐
   │  existing recipe: runner JIT (setec), job-start hook, gomodfs NFS, gocache + git proxies,        │
   │  GuestState stream and logs on its loopback API; plus a dial endpoint for cihostlet's SSH proxy  │
   │                                                                                                  │
   │   ┌────────────┐  socketpair   ┌──────────────┐  dial/listen      host network                   │
   │   │ VZ VM      │◄──SOCK_DGRAM─►│ guestnet     │─────────────────► (default route)                │
   │   │ (vz)       │  raw ethernet │ gVisor stack │──► IMDS / service table: ciguestlet's guest      │
   │   │            │               │ DHCP ARP DNS │    server, DNS, gocache, git, NFS handlers       │
   │   │ virtio-blk │◄─nbd+unix────►│ NTP  flows   │──► DNS and flow logs                             │
   │   │ virtio-net │               └──────────────┘                                                  │
   │   │ graphics   │◄───────────── screenshots (mechanism open, section 4.5)                         │
   │   └────────────┘   guestbd NBD server (writes discarded) serving the image's (warm) disk.img     │
   └──────────────────────────────────────────────────────────────────────────────────────────────────┘
```

### 4.1 Process model

corp's split stays: `cihostlet` (one per Mac, root) owns slots, the
cimgr-facing API, HostInfo push, TTLs and drain; one `ciguestlet` process
per slot owns one VM's lifecycle, its network services, the GitHub runner
recipe and `GuestState` transitions. They are separate processes so that
deploying cihostlet doesn't interrupt builds running in guests.

With the mantsana backend, ciguestlet creates the VM in-process
(`mantsana/guest` with the `mantsana/vzvm` backend) instead of shelling out
to `tart`. The VM, its guestnet netstack, its guestbd NBD server and its
IMDS all live in the ciguestlet process, so the VM has exactly the
ciguestlet's lifetime: a VM can't outlive the process that created it
(observed: killing the creating process ends the VM), and its network and
disk die with the process anyway. This is also crash containment: a
Virtualization.framework crash (corp has seen VM-side crashes, #43482) takes
down one slot.

ciguestlet must run as the CI user in that user's launchd domain, not as a
LaunchDaemon, because saving and restoring machine state use per-user
security services (section 9, item 4). Measured on the test Mac:

| How the per-VM process is launched | Save | Restore |
|---|---|---|
| root LaunchDaemon (system domain) | no (no per-user `secd`) | no |
| LaunchDaemon, system domain, `UserName=<CI user>` (Tart-mode ciguestlet today) | no (no per-user `secd`) | no |
| root runs `launchctl bootstrap user/<uid>`, plist with `LimitLoadToSessionType=Background` | yes | yes, 4.2 s to sshd |
| root runs `launchctl bootstrap gui/<uid>` | yes | yes, 3.9 s to sshd |

(Without `LimitLoadToSessionType=Background`, bootstrapping into `user/<uid>`
fails with "5: Input/output error", as root or as the user; agents
evidently default to the Aqua session type.)

Per `launchctl(1)`, `user/<uid>` is the user domain, which "may exist
independently of a logged-in user" (its session type is Background), and
`gui/<uid>` is the user-login domain created when the user logs in at the
console (session type Aqua), existing only while that login does. The two
share a Mach bootstrap namespace but not services, which is presumably why
both can reach the user's security services while the system domain can't.

So in mantsana mode cihostlet writes the guestlet plist without `UserName`
and with `LimitLoadToSessionType=Background`, and bootstraps it into
`user/<uid>` of the CI user. That keeps guestlet lifetimes independent of
the GUI login's, like today's LaunchDaemons, and the job is still separate
from cihostlet's own, so cihostlet restarts don't touch it. (`gui/<uid>`
also works if we ever need the window server, for example for screenshots,
section 4.5.) Cold boots work in any of these contexts, but restores also
need the user logged in and the session unlocked; the screen lock is
typically MDM policy, so CI Macs must be excluded from any idle-lock
policy.

Host setup for a mantsana CI Mac:

1. Boot straight into the CI user's session: automatic login
   (`/etc/kcpassword`), which macOS only allows with FileVault off;
   otherwise every reboot goes through `fdesetup authrestart`.
2. The session never locks, by policy: no MDM idle-lock profile, screen
   saver password off. Display sleep is
   then the only remaining trigger, and mantsana prevents it.
3. The ciguestlets are jobs in that session (above). On any Mac whose MDM
   still enforces an idle lock, also install the keep-awake agent
   `cmd/mantsana/launchd/com.tailscale.mantsana.keepawake.plist`. It keeps
   `caffeinate -dimsu` assertions held continuously (a new 600-second one
   every 300 seconds, overlapping), so the session never sleeps and never
   goes idle, which is what defeats an MDM idle lock (sleep assertions alone
   don't). There must be no gap: the first version let launchd restart a
   single `caffeinate -t 300`, which left 20 seconds without assertions
   every 5 minutes, and the managed screen saver started (and locked the
   screen 60 seconds later) in one of those gaps. As the CI user:

   ```
   # install
   cp cmd/mantsana/launchd/com.tailscale.mantsana.keepawake.plist ~/Library/LaunchAgents/
   launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/com.tailscale.mantsana.keepawake.plist

   # remove
   launchctl bootout gui/$(id -u)/com.tailscale.mantsana.keepawake
   rm ~/Library/LaunchAgents/com.tailscale.mantsana.keepawake.plist
   ```
4. If the session is locked anyway, a restore fails fast (about 100 ms,
   surfaced as "permission denied"); `vzvm` reports that as a transient
   `RestoreError`, and `mantsana/guest` cold boots without marking the warm
   snapshot stale.

### 4.2 Guest VM configuration (`mantsana/vzvm`, darwin && arm64 only)

Built once per boot, identically for cold boot and restore, since the
restored state must match the configuration:

| Device | Choice | Why |
|---|---|---|
| Boot loader | `MacOSBootLoader` | macOS guest |
| Platform | `MacPlatformConfiguration` with hardware model, machine identifier and aux storage from the image | required |
| CPU/memory | from the request, defaulting to the image's; a restored VM must match the warm snapshot's size | |
| Disk | one `VirtioBlockDeviceConfiguration`; attachment is `NetworkBlockDeviceStorageDeviceAttachment` to the in-process guestbd server over a unix socket, `nbd+unix:///?socket=<path>`, sync mode none | discard writes, no per-VM disk files, lazy fetch later |
| Network | one `VirtioNetworkDeviceConfiguration` with fixed MAC from the image, `FileHandleNetworkDeviceAttachment` over one end of a `socketpair(AF_UNIX, SOCK_DGRAM)`; `SO_SNDBUF` 512 KiB and `SO_RCVBUF` 2 MiB on both ends (Apple requires `SO_RCVBUF` of at least twice `SO_SNDBUF` and recommends four times; absolute sizes to be tuned, section 9 item 7) | per-VM isolation, all packets visible to Go |
| Graphics | `MacGraphicsDeviceConfiguration` with one display, fixed pixel geometry from the image manifest (default 1280x800 @ 72 ppi) | for screenshots; part of the saved state's configuration |
| Input | `MacKeyboardConfiguration`, `MacTrackpadConfiguration` | Mac-specific input for macOS 13+ guests; in case screenshots grow into UI automation |
| vsock | `VirtioSocketDeviceConfiguration` | future guest agent; free |
| Serial | `VirtioConsoleDeviceSerialPortConfiguration` to a file handle pair, logged as the console log | debugging; macOS guests print little but it costs nothing |
| Entropy, sound, balloon, USB, shared dirs, clipboard | omitted | not needed; fewer devices to keep save/restore compatible; virtiofs on macOS is unreliable (corp uses NFS) |

This exact device set passes `Validate()` and `ValidateSaveRestoreSupport()`
on hardware, with the disk over NBD and with or without the serial port, and
a state saved with it restores (section 9). Both validations run at every
boot and are logged; if save/restore validation fails the VM still boots
cold but can't be snapshotted.

Lifecycle per slot:

1. Allocate slot, socketpair, unix socket dir (short path, `sun_path` limit).
2. Start guestbd serving `disk.img` with `WithSharedSnapshot()` (the framework
   NBD client reconnects on timeout; per-connection snapshots would lose the
   guest's writes across a reconnect and corrupt the filesystem).
3. Clone the image's `nvram.img` (NVRAM, ~tens of MB) with `clonefile` into the
   slot dir; the guest writes NVRAM variables.
4. Start guestnet on the other socketpair end.
5. Build config, validate, `NewVirtualMachine`. Keep the Go pointer reachable
   for the VM's lifetime (vz finalizers release the ObjC object).
6. If the image has a warm state: `RestoreMachineStateFromURL` then
   `Resume`. On restore error (host OS updated, mismatch): log, mark the warm
   image stale, fall back to cold `Start`.
7. Watch `StateChangedNotify()` in one goroutine; publish events.
8. Stop: `RequestStop()` (graceful, guest shutdown) with a deadline, then
   `Stop()`. Close everything; delete the slot dir.

### 4.3 Guest networking (`guestnet`, portable Go)

The macOS equivalent of "each VM in its own network namespace" is a private
userspace network stack per VM. There is no shared bridge, no vmnet, no `pf`.
A guest's frames only ever reach Go code owned by its slot.

Design (adapted from tailvisor `netd` and containers' `gvisor-tap-vsock`):

* One gVisor `stack.Stack` per VM: `ipv4`, `arp`, `tcp`, `udp`, `icmp`. NIC is
  an `ethernet` link over a `channel` endpoint pumped by a `FrameLink`
  (`ReadFrame`/`WriteFrame`), implemented over the unix datagram socket. For
  tests, an in-memory link pair.
* The NIC is promiscuous and spoofing, so every IPv4 packet the guest sends,
  to any destination, is delivered to our forwarders.
* Addresses on our NIC, so gVisor answers ARP and ICMP echo for them
  natively: gateway `192.168.64.1/24`, IMDS `169.254.169.254`, DNS
  `169.254.169.253`. Because the two link-local addresses are on the NIC, the
  guest's on-link ARP for 169.254.x.x is answered directly and the
  `route -n add` hack in the guest goes away.
* Guest gets a fixed lease: IP `192.168.64.2`, router `.1`, DNS
  `169.254.169.253`, MTU option matching the link, long lease. Every slot uses
  the same addresses; the L2 domains are separate so there is no conflict,
  and it means a memory snapshot is valid in any slot without re-DHCP.
  The gateway MAC is a constant, so the guest's ARP cache stays valid after
  restore. DHCP renewals from a restored guest are ACKed for the same
  address. That is the whole "twiddle DHCP/ARP" step.
* NTP: intercept every UDP/123 flow and answer with host time. A restored
  guest's clock is stale by the snapshot age and `timed` fixes it on first
  sync. The API also exposes a "guest ready" hook where ciguestlet can force
  `sntp` over SSH if needed.
* TCP: `tcp.NewForwarder`. Each SYN is dispatched by (dst IP, dst port):
  1. service table hit (see below): hand the `gonet` conn to the handler.
  2. blocked destination (tailnet CGNAT `100.64.0.0/10`, other slots, the
     host's own addresses unless allow-listed): RST.
  3. otherwise egress: dial from the host's network stack, splice, and record
     a flow log entry (start, end, guest src port, dst, bytes in/out, SNI or
     Host if cheaply visible). Flow logs are what "log all outgoing TCP/HTTP
     flows" means; they are per slot and streamable from the API.
* UDP: forwarder with a per-slot session table keyed by 4-tuple, idle
  timeout, same dispatch and logging rules. Port 53 to any destination goes
  to the DNS service; port 123 to NTP.
* DNS: parse with `golang.org/x/net/dns/dnsmessage` for logging (qname, qtype,
  rcode, upstream, latency), forward the raw query to configured upstreams
  (default: the host's resolvers) over UDP with TCP fallback, and answer
  configurable overrides (for example a MagicDNS suffix to the host's
  `100.100.100.100` while still blocking the CGNAT range for everything else).
  Every query is logged; this replaces `--debug-dns`.
* IPv6: dropped for now (corp blocks it today).
* Host to guest: `gonet.DialTCP` into the guest from the slot's stack. The API
  exposes it as a connection upgrade (`POST /v0/vms/{name}/dial`), which is
  how ciguestlet SSHes in. There is deliberately no host interface with a
  route to the guest.

Service table: the guest recipe registers guest-visible endpoints that mantsana
forwards to host-side targets, replacing the `pf rdr` rules:

```json
"services": [
  {"guest": "169.254.169.254:80",   "target": "unix:/var/run/ci/slot1-guest.sock"},
  {"guest": "169.254.169.254:31364","target": "tcp:127.0.0.1:31364"},
  {"guest": "192.168.64.1:2049",    "target": "tcp:127.0.0.1:2049"},
  {"guest": "192.168.64.1:9418",    "target": "tcp:127.0.0.1:9418"}
]
```

mantsana's built-in IMDS (section 4.4) serves anything on `169.254.169.254:80` that
the caller has not claimed. Because each slot has its own stack, the same
guest port maps to different host targets per slot with no ephemeral-port
games and no peer-IP filtering.

Isolation properties: a guest can only send frames into its own stack; it can
reach (a) service-table targets, (b) egress destinations not denied by policy,
via connections mantsana originates. It cannot see another guest's frames, ARP
spoof anything real, or reach host listeners except those explicitly
published. The daemon's egress dial can additionally be constrained
(deny RFC1918, deny host addresses) by policy.

Performance: one frame per `read`/`write` syscall on the datagram socket, no
batching on macOS. gVisor plus splice handles the CI workload (git, module
downloads over NFS from the host, HTTPS). Raise the link MTU (up to 65535)
if host-to-guest NFS throughput matters; it is baked into the snapshot via
DHCP option 26 so it is an image property.

### 4.4 IMDS (`imds`)

Plain `http.Handler` served to guest connections to `169.254.169.254:80`
(and reachable to the host process for tests). Built-in endpoints, all per
slot so the identity of the caller is implicit:

* `GET /` index.
* `GET /v0/meta` JSON: VM name, slot, image, cpus, memory, boot mode
  (cold/restored), boot time, host name.
* `GET /v0/user-data/<key>`: opaque key/values supplied at create time (job
  tokens, runner config, etc.), so nothing has to be pushed over SSH.
* `GET /v0/time`: host time, for guests that want it without NTP.
* `POST /v0/ready`: guest signals it is usable; surfaces as an event.
* `POST /v0/log`: append a line to the slot's event log.
* Everything else: 404, unless the caller claimed the port in the service
  table, in which case mantsana never sees it.

### 4.5 Screenshots

Requirement: screenshots of each guest's display (the brief suggests an
internal VNC server), with no GUI on the host. The mechanism is open. The
only display path Apple documents is `VZVirtualMachineView`, an AppKit view,
and there is no documented headless framebuffer API. Candidates, to be
evaluated from Apple's documentation and experiment when we get to it
(milestone 5):

* Host the documented `VZVirtualMachineView` in an offscreen window inside
  ciguestlet (which runs in the user's Aqua session, section 4.1) and
  capture it with documented AppKit/ScreenCaptureKit APIs.
* A VNC server in the guest (macOS Screen Sharing), reached through the
  netstack like sshd, with a Go RFB client that grabs one frame.

Either way the graphics device stays configured, since its geometry is part
of the saved state's configuration. ciguestlet's existing `/screenshot`
endpoint serves the result.

### 4.6 Threading

vz wraps every framework call in `dispatch_sync` on the VM's private queue.
No `runtime.LockOSThread`, no `NSApplication`, no main thread. The only
single-consumer resource is `StateChangedNotify()`; one goroutine per VM
drains it and fans out to the event log. KVO callbacks arrive on the dispatch
queue thread; they only touch a mutex and a channel.

### 4.7 Images (`mantsana/image`)

An image is a directory of plain files, in the image store directory,
by default `~/Library/Application Support/mantsana/images` (not
`~/Library/Caches`: the system may purge caches, and warm snapshots are
costly to rebuild):

```
<images-dir>/<name>/
  manifest.json      # see below
  disk.img           # raw, sparse; immutable once published
  nvram.img          # VZMacAuxiliaryStorage (NVRAM), pristine
```

`manifest.json`:

```json
{
  "version": 1,
  "os": "macos",
  "arch": "arm64",
  "hardware_model": "<base64 of VZMacHardwareModel.dataRepresentation>",
  "machine_id": "<base64 of VZMacMachineIdentifier.dataRepresentation>",
  "mac": "02:76:7a:00:00:02",
  "cpus": 4,
  "memory_bytes": 8589934592,
  "display": {"width": 1280, "height": 800, "ppi": 72},
  "mtu": 1500,
  "disk_bytes": 85899345920,
  "created": "2026-09-29T00:00:00Z",
  "description": "macOS 26.x + Xcode 26.5, admin/admin"
}
```

The MAC, machine identifier, display geometry and MTU are image properties
because a memory snapshot bakes them in. Two slots running the same image
share them; that is fine because their networks are separate and nothing
depends on machine identifier uniqueness. (Not yet verified: running two
VMs with the same machine identifier at once; section 9 item 10.)

Warm snapshots are host-local derived data. Apple doesn't document the
state file's portability, but on hardware it is encrypted to a Secure
Enclave key of the logged-in user (section 9), so treat it as bound to the
host and user:

```
<images-dir>/<name>/warm/
  disk.img           # clonefile of ../disk.img, then mutated by the warm boot
  nvram.img          # clonefile of ../nvram.img, then mutated
  state.vzvmsave     # saved memory state, consistent with the two files above
  warm.json          # host OS build, mantsana version, image manifest hash, cpus/mem
```

Warm-up flow (`POST /v0/images/{name}/warm`, also automatic on first use):
clone aux; boot cold with the *same* device set as production, disk over NBD
included; wait until the guest is
usable (TCP 22 reachable through the netstack, or `POST /v0/ready`); optionally
settle for a few seconds; `Pause`; `SaveMachineStateToPath`; `Stop`. Production
runs then serve `warm/disk.img` over NBD with discarded writes and restore
`warm/state.vzvmsave`. A warm image is invalidated when the host OS build or
the manifest hash changes, or when a restore fails; mantsana then cold boots and
re-warms in the background when a slot is free.

Persisting the warm-up's disk: the framework only restores a state into a
configuration with the same kind of disk attachment it was saved with
(section 9), so the warm-up boot uses NBD exactly like production. After
`Stop`, mantsana clones `disk.img` to `warm/disk.img` and writes the guestbd
shared snapshot's dirty pages onto it (`Snapshot.WriteDirtyTo`). On the
first hardware run this was 81,317 pages (318 MiB) in 2.5 s.

Cold-boot-only images (no warm dir) work too, just slower.

The image store API is intentionally tiny (`list`, `get`, `warm`,
`delete-warm`); acquiring images is a separate concern. Later this becomes a
content-addressed chunk store with lazy fetch, plugged in at guestbd's
`BaseImage` seam and served to the framework's NBD client exactly as today.

### 4.8 Corp integration: a mantsana backend in cihostlet and ciguestlet

The mantsana backend coexists with the Tart one so Macs can opt in one at a
time. cihostlet gets a flag selecting the macOS VM backend (for example
`--vm-backend=tart|mantsana`, default `tart`), which it passes down to each
ciguestlet. Everything above that seam (cimgr, the cihostlet API and its
types, `GuestState`s, boot logs, TTLs, drain, baseline and API slots) is
unchanged in both modes.

ciguestlet, in mantsana mode (replacing what `tart.go` does):

1. Boot with `mantsana/guest` + `mantsana/vzvm` in-process: restore the
   image's warm snapshot if usable, else cold boot; the disk is served over
   NBD with writes discarded. No `tart clone`/`set`/`run`/`ip`/`stop`.
2. Wait for sshd with `guestnet.DialGuest(22)`, and SSH over that
   connection. There is no `tart ip`: the guest address is fixed. The
   in-guest `route -n add` and `networksetup` steps go away: IMDS and DNS
   are on-link in the guest's netstack and DHCP hands out the DNS address.
3. Its existing services plug into guestnet's service table instead of
   `pf rdr` rules (`imds_darwin.go`'s anchors go away): the guest server on
   `169.254.169.254:80` (job-start hook `POST /<token>`), its DNS proxy on
   `169.254.169.253:53` (as guestnet's `Resolver`), the gocache proxy on
   `169.254.169.254:31364`, gomodfs NFS on `<gateway>:2049`, and the git
   proxy on `<gateway>:9418`. Egress policy (the tailnet CGNAT block) comes
   from guestnet's deny list.
4. The rest of the recipe is unchanged: NFS mount, base files tarball, runner
   JIT config and `run.sh --jitconfig`, the same `GuestState`s.
5. Its loopback API gains a dial endpoint (an HTTP upgrade to a TCP stream
   into the guest, as `guestnet.DialGuest`), because there is no host route
   to the guest.

cihostlet, in mantsana mode:

1. Launch each ciguestlet by bootstrapping its plist (no `UserName`, with
   `LimitLoadToSessionType=Background`) into `user/<uid>` of the CI user,
   instead of loading a LaunchDaemon with `UserName` (section 4.1).
2. Its SSH proxy (`POST /api/vms/{name}/ssh`) connects through the
   guestlet's dial endpoint instead of dialing `PrivateIP:22`.
3. Replace `tart pull` of `--tart-image` with an image in the local image
   store: download it (as `cmd/oci-img-dl` does, as a library) when the
   configured reference changes, and build its warm snapshot (`guest.Warm`,
   in a free slot's ciguestlet) before first use and after host OS updates.
4. Drop the `pf` anchor sweeping; keep `caffeinate`, and make sure the
   session never locks (section 4.1).

mantsana itself (this repo) provides the packages (`guestnet`, `imds`,
`mantsana/guest`, `mantsana/vzvm`, `mantsana/image`, the OCI image
download) and development tools (`cmd/mantsana selftest` and probes,
`cmd/oci-img-dl`). The corp ciguestlet and cihostlet code is expected to
move into this repo later, at which point the backend flag and Tart mode
can go.

## 5. Repository layout

```
DESIGN.md
cmd/mantsana/            darwin&&arm64 dev tool: selftest (validate, cold boot, warm, restore) and debugging probes
cmd/mantsana/launchd/    keep-awake LaunchAgent for Macs whose MDM enforces an idle lock
cmd/oci-img-dl/          dev tool: download a VM image published to an OCI registry (as corp CI uses today) as a mantsana image
guestnet/                per-VM netstack: link, DHCP, DNS, NTP, TCP/UDP forwarding, service table, flow log
guestnet/guestnettest/   fake guest stack for tests
imds/                    metadata http.Handler
mantsana/guest/          one VM: network, IMDS, NBD disk, boot/restore with cold fallback, warm-up (portable)
mantsana/guest/guesttest/ fake Backend for tests
mantsana/image/          image dir + manifest, warm-state bookkeeping
mantsana/vzvm/           darwin&&arm64: build config, boot/restore/save, NBD wiring, console
mantsana/screenshot/     guest display capture (mechanism open, section 4.5)
```

`guestnet` and `imds` are top level because Linux guests, and the corp
Linux hostlet when it moves here, will use them too. Everything except
`vzvm`, any darwin half of `screenshot`, and `cmd/mantsana` compiles and is
tested on Linux, where most development happens (`mantsana/guest` is
unix-only because guestbd is).

## 6. Build and deploy

Anything linking vz (the mantsana-mode ciguestlet, `cmd/mantsana`) needs cgo
and the macOS SDK, so it is built on macOS (macOS 14+ SDK for save/restore
and NBD), and ad hoc signed with the virtualization entitlement:

```
go build -o mantsana ./cmd/mantsana
codesign --force --entitlements cmd/mantsana/mantsana.entitlements -s - ./mantsana
```

corp's `deploy.sh` today cross-compiles ciguestlet for darwin/arm64 with cgo
off, which can't include vz. Plan: build and sign it in a single-use macOS
build VM (corp's tempvm service, running on this same infrastructure), then
copy the signed binary to the CI Macs as today; no signing on the targets.
Verified 2026-09-30: a binary built and ad hoc signed on one Mac (macOS 26.5,
SDK 26.4), copied byte-identically via Linux to a different Mac (Mac17,16,
macOS 27.0), passes `codesign -v --strict` there and creates and starts VMs;
the same binary re-signed there without the entitlement is refused ("The
process doesn't have the com.apple.security.virtualization entitlement").
An ad hoc signature is a code-directory hash inside the binary, not bound to
a host, and copying with scp or tailcat adds no quarantine attribute.
`cmd/mantsana entitlementprobe` checks a binary on a Mac without an image.
Keep a manual escape hatch (build on any one Mac by hand) for when the
build VMs themselves are broken.

`mantsana.entitlements` contains `com.apple.security.virtualization` (required),
plus `com.apple.security.network.client` and `.server` (harmless, and required
if the binary is ever sandboxed; the vz NBD docs mention `network.client`).
`com.apple.vm.networking` is not needed since we never use bridged vmnet.

vz checks at runtime that it was built against an SDK new enough for the
APIs it calls (`ErrBuildTargetOSVersion`); a binary built with the 26.4 SDK
ran fine on macOS 27.0.

## 7. Dependencies

* `github.com/Code-Hex/vz/v3`. Fix problems there (via a fork with
  `replace` until upstreamed) rather than work around them.
* `gvisor.dev/gvisor` netstack.
* `github.com/tailscale/tb/guestbd` NBD server.
* `github.com/insomniacslk/dhcp` for DHCPv4 message encoding (already used in
  corp's `dhcp.go`).
* `golang.org/x/net/dns/dnsmessage` for DNS parsing.
* `github.com/tailscale/setec`, first class, for secrets (the GitHub App key
  used to mint runner JIT configs).
* The host's tailscaled (via `tailscale.com/client/local`) for the tailnet
  listener address and WhoIs, as cihostlet does today. Not tsnet.
* A GitHub API client for JIT configs and runner deregistration (corp uses
  go-github).
* Possibly an RFB client for screenshots, depending on section 4.5 (corp
  uses `github.com/mitchellh/go-vnc`).

## 8. Decisions

* mantsana replaces only Tart and `pf`: corp's cihostlet and ciguestlet stay,
  and ciguestlet gets a mantsana backend (section 4.8), selected per host by
  a cihostlet flag while Tart mode remains. (Rejected: one mantsana process
  taking both roles. A VM can't outlive the process that created it, so
  every deploy of that process would kill running builds.)
* One ciguestlet process per VM, with the VM, its netstack and its NBD server
  in-process: 1:1 shared fate, and crash containment per slot.
* The cimgr/hostlet API types stay in corp for now (2026-11-03), so corp
  isn't half migrated. Nothing here needs them: the corp ciguestlet
  imports `mantsana/guest`, not the other way around.
* Userspace netstack per VM instead of vmnet/pf: it is the only way to get
  real isolation, deterministic addressing, and packet-level visibility on
  macOS without root-owned network state that leaks.
* NBD to an in-process guestbd rather than APFS clone per run: no disk space
  churn, identical to the Linux path, and it is the seam for lazy image
  fetching. Clone stays as the fallback if the framework rejects NBD for
  save/restore.
* Fixed guest IP/MAC/gateway MAC shared by all slots so snapshots are
  slot-independent and need no post-restore network fix-up beyond normal
  DHCP renewals.
* No guest agent for now: SSH as `admin` over the API dial upgrade, exactly
  like today. vsock is configured for a future agent.
* Copy tailvisor `netd`'s design, not its module. Import `guestbd`.
* Framework behavior comes from Apple's documentation and our own
  experiments on hardware, not from reading other hypervisors' source.

## 9. Open questions to settle on hardware

Answers so far are from 2026-09-29 on an M1 Pro MacBook Pro (MacBookPro18,2),
macOS 26.5 (25F71), guest macOS Tahoe imported from a Tart VM, 4 CPUs, 8 GiB.

1. Does `ValidateSaveRestoreSupport()` accept `NetworkBlockDeviceStorageDeviceAttachment`
   and `FileHandleNetworkDeviceAttachment`? **Yes**, with and without the
   virtio serial console. Cold boot over NBD with the userspace network
   works: DHCP lease about 6 s after start, sshd reachable at 7.7 to 9.2 s.
   (This needed guestbd to support `NBD_OPT_INFO`, which Apple's NBD
   client sends before `NBD_OPT_GO`.)
2. Can a state saved with a disk-image attachment be restored with an NBD
   attachment on the same virtio-blk device? **No** ("invalid argument"):
   the attachment type must match. A state saved over NBD restores over
   NBD, so warm-up boots over NBD too and persists the guest's writes onto
   an APFS clone of the base disk afterwards (guestbd
   `Server.SharedSnapshot` + `Snapshot.WriteDirtyTo`). Paths may differ
   between save and restore; the aux file may be a clone.
3. How to take screenshots headlessly (section 4.5).
4. Session requirements (observed; Apple doesn't document them). The
   framework's `VirtualMachineArchiveKeyManager` encrypts the saved state to
   a Secure Enclave P-256 key of the user running the VM.
   * As root in a LaunchDaemon (system domain) there is no per-user `secd`:
     cold boot works, but saving fails with -25291 (`errSecNotAvailable`,
     surfaced as "Internal Virtualization error").
   * As a regular user, saving works whether or not the screen is locked
     (it only needs the public key), from the GUI (Aqua) session or a plain
     SSH (Background) session.
   * Restoring needs the private key, which the Secure Enclave only allows
     while that user's session is **unlocked**. With the screen locked it
     fails with -25308 (`errSecInteractionNotAllowed`, surfaced as
     "permission denied"), in any session. Unlocked, restore works from
     Aqua and from SSH alike.

   So mantsana runs as a logged-in regular user whose screen never locks:
   no idle lock (MDM may enforce one, see 4.1),
   display sleep off. corp's cihostlet already runs `caffeinate -dims` for
   sleep. When restore fails this way mantsana cold boots, and should not
   mark the snapshot stale, since it becomes usable again once unlocked.
5. Restore-to-usable latency: **about 4.0 s** from restore start to sshd
   reachable (2.8 GiB state file for 8 GiB of guest memory; saving takes
   4 to 5.5 s, a whole warm-up about 26 s). Still open: whether `timed` corrects the clock fast
   enough via intercepted NTP or we need an explicit `sntp` over SSH.
6. Whether macOS's DHCP client renews properly against our server after a
   long snapshot age (lease should be long; test with expired leases too).
7. Throughput of the datagram link for NFS module cache reads; whether a
   larger MTU is worth baking into images.
8. (Found on hardware) macOS guests ignore `RequestStop` (no shutdown within
   30 s). Since all writes are discarded, normal VMs are force-stopped
   (about 170 ms).
9. (Found on hardware) The guest's DHCP client ARP-probes its offered
   address (RFC 5227) and declines the lease if anything answers. A
   promiscuous gVisor NIC answers ARP for every address, so guestnet only
   passes ARP requests for its own addresses to gVisor.
10. Two VMs from the same image (same machine identifier and MAC, separate
    networks) running at once.

## 10. Testing

* `guestnet`: a second gVisor stack plays the guest over an in-memory link
  pair; tests cover DHCP lease, ARP for gateway and IMDS addresses, TCP egress
  with flow log, service table dispatch, DNS interception and logging, NTP,
  blocked destinations, host-to-guest dial.
* `mantsana/guest`: a fake backend (`guesttest`) whose machines are gVisor
  guests; tests cover cold boot, IMDS from the guest, the NBD socket,
  warm-up, restore, and restore failures (transient and not).
* `image`: manifest round trip, warm-state validity checks.
* `cmd/oci-img-dl`: LZ4 framing, and a fake registry with the bearer token
  challenge, bad responses, sparseness and resume.
* Darwin packages: a `mantsana selftest` subcommand that boots an image cold,
  checks save/restore validation, saves state, restores,
  and reports timings. Run manually on the CI Macs.

## 11. Milestones

1. Prototype (done): `guestnet`, `imds`, `mantsana/image`, `mantsana/guest`,
   `mantsana/vzvm`, `cmd/mantsana selftest`, `cmd/oci-img-dl`.
2. Hardware (done): cold boot, warm-up and restore on a Mac; section 9 items
   1, 2, 4 and 5 settled; launch context for save/restore settled
   (section 4.1).
3. Corp: ciguestlet mantsana backend and cihostlet `--vm-backend` flag
   (section 4.8), with Tart mode unchanged.
4. One Mac switched to `--vm-backend=mantsana`, taking real jobs from cimgr;
   then the rest, one at a time.
5. Screenshots (section 4.5).
6. Image sync (content-addressed chunks, lazy NBD-backed fetch; section 13).
7. Linux guests (EFI boot loader, virtio-gpu or none, more than 2 slots).

## 12. Linux guests (later)

Same slot machinery. Differences: `EFIBootLoader` + variable store or
`LinuxBootLoader` (kernel+initrd), `GenericPlatformConfiguration`, no macOS VM
limit so slot count is CPU/RAM bound, virtio-gpu only if screenshots are
wanted, save/restore is not supported by the framework for Linux so fast boot
is cold boot from a small image (Firecracker-style), guest networking is
identical. The image manifest's `os` field selects the builder.

## 13. Image storage measurements (input to milestone 7)

2026-09-30: page-level similarity between public macOS Tahoe + Xcode VM
images (`ghcr.io/cirruslabs/macos-tahoe-xcode`). Each is a 140 GB raw disk,
about 80.5 GiB of it non-zero, published as 261 LZ4-compressed 512 MiB
chunks (69 GB). Pages are 4 KiB; "reused" means the page's content (SHA-256)
appears somewhere in the older image. Percentages are of the newer image's
non-zero pages.

| old -> new | reused, same offset | reused, moved | dup within new | new content |
|---|---|---|---|---|
| 26.4.1 (2026-05-04) -> 26.5 previous build (2026-05-26) | 34.0% | 10.4% | 1.1% | 54.5% (43.7 GiB) |
| 26.5 previous build (2026-05-26) -> 26.5 rebuild (2026-09-16) | 0.03% | 66.5% | 1.2% | 32.3% (26.0 GiB) |
| 26.4.1 (2026-05-04) -> 26.5 rebuild (2026-09-16) | 0.03% | 22.2% | 1.4% | 76.5% (61.6 GiB) |

(The "previous build" is the untagged `sha256:61f6e857...`, inferred to be
the earlier `26.5` from its date and download count.)

Share of the newer image's non-zero data an aligned fixed-block
content-addressed store would have to fetch, given the older image:

| old -> new | 4 KiB | 16 KiB | 64 KiB | 1 MiB | 16 MiB |
|---|---|---|---|---|---|
| 26.4.1 -> 26.5 previous | 54% | 63% | 66% | 68% | 72% |
| 26.5 previous -> 26.5 rebuild | 32% | 85% | 98% | 100% | 100% |
| 26.4.1 -> 26.5 rebuild | 76% | 93% | 99% | 100% | 100% |

Reused pages that moved mostly moved as long contiguous runs (whole files at
a new 4 KiB-aligned offset): for the 26.5 rebuild, 469K old->new extents
cover all 14.0M reused pages, with 84% of them in extents of 1 MiB or more
and 48% in extents of 64 MiB or more (median extent is 1 page, a long tail
of small ones).

Conclusions so far:

* Rebuilds of "the same" image relocate nearly everything, so anything
  keyed by disk offset (backing-file chains, offset-based block deltas,
  fixed-block rsync) gets no reuse between rebuilds.
* Content addressing must be at 4 KiB granularity, the APFS block size;
  fixed blocks of 16 KiB or more lose most of the reuse because relocated
  files land at arbitrary 4 KiB alignments. Content-defined chunking over
  the raw disk might recover larger chunks; not yet measured.
* Because moved content arrives in long runs, an image can be described
  compactly as extents referring to earlier images' content (about 30x
  fewer entries than per-page hashes), and fetches can be large even though
  addressing is per page.
* About 1% of an image's non-zero pages duplicate other pages in the same
  image; intra-image dedup is not worth much.
