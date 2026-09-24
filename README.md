# tb: Tailscale Build

This is the future home of pieces of Tailscale's build system, at least
the shared reusable parts.

The name is 1) short, 2) plausibly an acronym for Tailscale Build, 3) maybe a nod to
Go's [testing.TB](https://pkg.go.dev/testing#TB)? (Maybe a stretch.)

Currently our build system is spread across various repos:

* `tailscale/corp` (closed source). We want to move as much possible out of there. That contains our VM management servers (to make Firecracker & QEMU & Virtualization.Framework VMs of Linux, FreeBSD, Windows, macOS, and Plan 9 VMs), making throwaway fresh VMs in seconds, and the tooling to quickly do interactive development or batch builds on them.
* [`tailscale/gomodfs`](https://github.com/tailscale/gomodfs/), our GOMODCACHE Go module cache FUSE + NFS + WebDAV + WinFsp filesystem. That will stay where it is.
* [`bradfitz/go-tool-cache`](https://github.com/bradfitz/go-tool-cache), Brad's personal repo where he developed the [GOCACHEPROG](https://pkg.go.dev/cmd/go/internal/cacheprog) proposal. That repo should probably be kept as a stub for historical reasons with onward links, but most of its `gocached` server & client that we use in production should move elsewhere. Probably to this repo.
* [`tailscale/gotst`](https://github.com/tailscale/gotst), an alternative Go test runner, which is a work in progress, but will continue to live there.
* [`bradfitz/guestbd`](https://github.com/bradfitz/guestbd), the NBD server used by our Firecracker & QEMU VMs. Maybe that should move here.
* ... others we're probably forgetting

## Contributing

As of 2026-09-24, we don't expect to take many community contributions
for a number of months until we have things all moved around and made
generic. In the short term, we just want move the code to this open
source repo while not worrying too much about whether it's still too
Tailscale-specific. But eventually we want it to be usable by anybody.

