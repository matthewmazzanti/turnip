# go-netns-bootstrap spike

De-risks the **bootstrap + fd-collection + dataplane-over-fd + netns-persistence** steps
of turnip's Go rewrite (rootful only): get every netns fd into one place in the root host
process, drive sysctls / nft / netlink against each by fd, AND prove a netns bind-mounted
at an arbitrary path survives so `podman run --network ns:<path>` can attach later.

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
2. **phase1 child** (inside podman's userns, mapped to uid 0) per name: creates a netns
   **pinned at a bind-mount path** under the user runtime dir (`unshare(CLONE_NEWNET)` +
   `mount(/proc/.../ns/net, path, MS_BIND)` — the `ip netns add` idiom at an arbitrary
   path), drops a persistent `marker0` iface, and ships the fd back over SCM_RIGHTS.
3. **parent** collects every `(name, fd)` into one registry and checks each fd is:
   - **present** — all requested names came back;
   - **distinct** — distinct nsfs inode (not one ns aliased N times);
   - **operable as root** — apply the full dataplane over the fd: **netlink** (`gw0`
     dummy + `/32` addr + link route via `NewHandleAt`), **sysctl** (`ip_forward` set +
     read back via a `setns` episode), **nft** (an `inet` table via `google/nftables`
     `WithNetNSFd` — no `nft` subprocess). Green is the rootful thesis: **init-root holds
     CAP_NET_ADMIN over the podman-userns-owned netns** (it has CAP_SYS_ADMIN in both its
     own userns and the target's, so it can `setns` in *and back out*).
4. **parent** then CLOSES all fds (so only the bind-mount keeps each netns alive) and
   launches a SEPARATE `podman unshare <self> --verify <paths...>` that must still find
   each netns live at its path (NSFS magic + `marker0` present) — the persistence proof.

Unknowns it answers: **(a)** init-root operating on a child-userns-owned netns by fd;
**(b)** fd 3 surviving `podman unshare`'s re-exec; **(c)** whether a bind-mount made in
one `podman unshare` survives into a separate one (i.e. `podman unshare` joins the
persistent pause-process mount ns) — the prerequisite for container attach.

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
[parent] fds closed; re-entering a FRESH `podman unshare` to check persistence
  [ok] /run/user/1001/turnip-spike/router_fabric persisted (live netns mount, marker "marker0" present)
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
  bind-mounts each WHILE IN IT before moving on — it never returns to the host netns.
- **Rootful thesis holds.** The root parent enters every podman-userns-owned netns by
  fd (`NewHandleAt`) and does CAP_NET_ADMIN ops — no in-process userns entry needed.
- **All three kernel-config interfaces work over an fd from the root parent.** netlink
  (vishvananda `NewHandleAt`) and nft (`google/nftables` `WithNetNSFd`) both accept a
  netns fd and `setns` at socket-dial under the hood — so nft needs no `nft` subprocess.
  sysctls have no netlink verb, so they need an explicit `setns` episode; the root parent
  can do it because it returns to the host netns afterwards (CAP_SYS_ADMIN in its own init
  userns) — the dual of the phase-1 constraint. This pins WHERE each lives in the port:
  the root parent owns sysctls/nft/netns-bound netlink; phase 1 only mints + pins netns.
- **Bind-mounts persist across `podman unshare` invocations.** A netns pinned at an
  arbitrary path in one `podman unshare` is still live in a *separate* one (after phase 1
  exits and all fds are closed) — so `podman unshare` joins podman's PERSISTENT
  pause-process mount ns, not a transient one. This is the prerequisite for
  `podman run --network ns:<path>` attach, and it means the exec-boundary bootstrap is a
  viable foundation (no cgo `nsenter` shim into the pause process needed). The pin is
  built from scratch (`unshare` + `mount` MS_BIND at a `$XDG_RUNTIME_DIR`-relative path),
  since `vishvananda/netns.NewNamed` only targets `/run/netns`, which mapped-root can't
  write.

## Known risks / fallbacks

- **fd 3 across `podman unshare`** (resolved above; kept for the record). If a future
  podman drops inherited fds, phase1 fails fast with `fd 3 is not a socket` — the fix
  is an **abstract-namespace unix socket** whose name is passed via an env var (env
  survives the re-exec) and dialed by the child; the SCM_RIGHTS transfer is identical.
- **`podman run --network ns:<path>` attach is proven by proxy, not directly.** The
  two-`podman-unshare` test confirms the mount ns is shared, and turnip's Python tool
  already attaches containers this way — but this spike does not itself launch a
  container (keeps it image-free). That end-to-end check is the obvious next confirmation.
- **Re-run hygiene.** Pins persist by design, so `--phase1` lazily unmounts + removes a
  stale pin before recreating; leftover `/run/user/<uid>/turnip-spike/*` mounts clear on
  reboot (tmpfs) or via `umount`.

## Not covered here (next spikes)

- The **host-edge** setns-free ops from init: veth peer born in a target netns via
  `netlink.NsFd`, and `LinkSetNsFd` device moves (the uplink/links primitives).
- An end-to-end `podman run --network ns:<path>` attach to a pinned netns (the direct
  form of the persistence proof above).
