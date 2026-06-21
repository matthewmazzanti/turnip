# go-netns-bootstrap spike

De-risks the **bootstrap + fd-collection** step of turnip's Go rewrite (rootful
only): get every netns fd into one place in the root host process, so the host
program can then drive sysctls / nft / netlink against them.

## What it proves

The Python tool enters podman's user+mount namespaces **in-process** — viable
because CPython is single-threaded, which `setns(CLONE_NEWUSER)` requires. Go's
runtime is multithreaded from startup, so it can't. This spike uses **`podman
unshare` as an exec boundary** instead: podman drops a *fresh* process inside its
userns for us, so we never do the single-threaded userns setns from Go at all.

Chain:

1. **parent** (real root, via sudo) drops to the rootless user and runs
   `podman unshare <self> --phase1 <names...>`, passing one end of a SEQPACKET
   socketpair as fd 3. In-process drop via `SysProcAttr.Credential` — `sudo -u`
   would close the inherited fd.
2. **phase1 child** (inside podman's userns, mapped to uid 0) `unshare`s a fresh
   netns per name and ships each fd back over SCM_RIGHTS (name + fd in one message).
3. **parent** collects every `(name, fd)` into one registry and checks each fd is:
   - **present** — all requested names came back;
   - **distinct** — distinct nsfs inode (not one ns aliased N times);
   - **operable as root** — enter via `netlink.NewHandleAt(fd)` and create a dummy
     link. Green here is the rootful thesis: **init-root holds CAP_NET_ADMIN over
     the podman-userns-owned netns** (it can `setns` in because it has CAP_SYS_ADMIN
     in both its own userns and the target's).

The two unknowns it actually answers: **(a)** does init-root operating on a
child-userns-owned netns by fd work, and **(b)** does fd 3 survive `podman
unshare`'s re-exec.

## Run

As root, with `$SUDO_USER` = the rootless-podman owner (i.e. just `sudo` from that
user's session), in a shell that has a Go toolchain + `podman`:

`CGO_ENABLED=0` matters: `os/user.Lookup` defaults to the C `getpwnam` path, and the
dev VM has no `gcc`. Disabling cgo switches it to the pure-Go `/etc/passwd` parser
(fine for resolving the rootless owner); the netlink/netns/x/sys deps are all pure Go.

```sh
cd spike/go-netns-bootstrap
go mod tidy                            # resolve deps (needs network once); commits go.sum
CGO_ENABLED=0 go build -o /tmp/spike-bin .
sudo ./...                             # see below
```

Build first, then `sudo` the binary: `sudo go run .` would re-exec a binary under
root's build cache that the dropped rootless user can't read, breaking the
`podman unshare <self>` step. A world-readable built binary avoids that. The dev VM
mounts the repo read-only (9p), so build from a writable copy (`cp -r ... /tmp/...`).

The parent reads `$SUDO_USER` to find the rootless-podman owner. Run it from that
user's own `sudo` session and it's already correct; in the dev VM you `sudo` as `dev`
but podman belongs to `homelab`, so override it:

```sh
sudo env SUDO_USER=homelab /tmp/spike-bin
```

Expected tail (validated in the dev VM, 2026-06-21 — **PASS**):

```
[parent] collected 4 netns fd(s) in one place
  [ok] all 4 requested netns present, keyed by name
  [ok] 4 distinct netns inode(s)
  [ok] "router:fabric": entered as root + created link (CAP_NET_ADMIN over podman netns)
  ...
PASS
```

## Findings

- **fd 3 survives `podman unshare`.** Ordinary `ExtraFiles` fd inheritance makes it
  across podman's re-exec — the abstract-socket fallback below was NOT needed.
- **No setns back to the host netns from inside podman's userns.** Phase 1 runs in
  podman's userns; the host netns is owned by the INIT userns (an ancestor), where our
  mapped-root holds no caps, so `setns` back into it is EPERM. `unshare(CLONE_NEWNET)`
  always mints a fresh netns regardless of the current one, so phase 1 chains forward
  (each new netns owned by podman's userns, which the root parent has caps over) and
  exits in the last one — it never returns to the host netns. The real port's phase 1
  must respect this when it adds bind-mounts.
- **Rootful thesis holds.** The root parent enters every podman-userns-owned netns by
  fd (`NewHandleAt`) and does CAP_NET_ADMIN ops — no in-process userns entry needed.

## Known risks / fallbacks

- **fd 3 across `podman unshare`** (resolved above; kept for the record). If a future
  podman drops inherited fds, phase1 fails fast with `fd 3 is not a socket` — the fix
  is an **abstract-namespace unix socket** whose name is passed via an env var (env
  survives the re-exec) and dialed by the child; the SCM_RIGHTS transfer is identical.
- **No persistence.** These netns are anonymous, alive only while the parent holds
  the fds — fine for proving the bridge. The real tool must also **bind-mount** each
  netns under a user-writable state dir (`/run/user/<uid>/turnip/...`, NOT
  `/run/netns`, which the mapped-root user can't write) so `podman run --network
  ns:<path>` can attach later. `vishvananda/netns.NewNamed` targets `/run/netns`, so
  the port will replicate pyroute2's explicit bind-mount instead.

## Not covered here (next spikes)

- Applying the **dataplane** over a collected fd: sysctls (manual `LockOSThread` +
  setns episode), nft via `google/nftables` `WithNetNSFd(fd)`, addrs/routes.
- The **host-edge** setns-free ops from init: veth peer born in a target netns via
  `netlink.NsFd`, and `LinkSetNsFd` device moves.
