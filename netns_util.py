#!/usr/bin/env python3
"""
netns_util.py -- shared, model-agnostic netns plumbing for the rootless fabric.

The primitives here are the same whether the fabric routes (main.py) or bridges:
locating/creating the persistent namespaces under $HOME/netns, opening one
netlink socket per namespace, and the ifindex lookups every wiring step needs.
main.py builds the routed dataplane on top of these.

Run everything under PLAIN `podman unshare` (NOT --rootless-netns). podman
unshare resets the environment, so call the venv interpreter by path -- a bare
`python3` resolves to system Python without pyroute2:

    podman unshare ./.venv/bin/python main.py up

Why plain `podman unshare` and not --rootless-netns
---------------------------------------------------
Empirically (confirmed on this host):
- A netns created under PLAIN `podman unshare` persists across separate
  invocations: the bind-mount lives in podman's user+mount namespace, which is
  held alive by the rootless pause process. (A netns created under
  --rootless-netns does NOT persist -- podman tears that namespace down, mounts
  and all, once no container is holding it.)
- A netns is owned by the user namespace it was created in. `podman run` runs
  the container in podman's userns, so to `setns()` into our netns by path the
  netns must be owned by THAT userns -- i.e. created inside `podman unshare`.
  Creating it as the plain login user (a different userns) would block the join.

Where owned infrastructure lives
--------------------------------
`IPRoute()`/`NetNS()` act on the netns the socket is opened in. Under plain
`podman unshare` the current netns is the (unowned) host netns, so creating an
interface there returns EPERM. We therefore host all owned networking
infrastructure inside an owned, persistent netns we created (the `router` netns
in the routed model): inside a netns we created we hold CAP_NET_ADMIN, so link
creation succeeds, and because that netns persists, so does the infrastructure.

Why low-level pyroute2 (and not NDB)
------------------------------------
We drive everything with the low-level IPRoute/NetNS API: one netlink socket per
netns, imperative verbs. We tried the high-level NDB ("builder") API, and it
reads/creates fine, but this workload is unusually namespaced: we create veths
whose two ends live in DIFFERENT netns, then mutate the moved end. NDB is a
transactional SQLite-backed projection that aggregates many sources; a commit
snapshots an object AND its dependencies for rollback, and a veth's dependency is
its peer in another source. Snapshotting cross-source dependencies is its
least-robust corner and it failed inconsistently here (a link-level `set` on a
moved veth end crashed inside the snapshot/shadow-table machinery). The low-level
API has no DB and no snapshots, so there is nothing to fight -- the right fit for
raw namespace surgery.

pyroute2 notes
--------------
- netns.create(full_path) honors the path verbatim; runs unshare(CLONE_NEWNET)
  in a child and bind-mounts /proc/<child>/ns/net onto the file. It opens that
  mountpoint O_CREAT|O_EXCL, so the path must NOT already exist (see ensure_netns
  for the stale-placeholder handling).
- `NetNS` is, as of pyroute2 0.9.1+, a thin wrapper around `IPRoute(netns=...)`:
  one netlink socket bound INTO the ns (no forked child). It's cheap to hold open
  and reuse for many ops, so `up` opens each ns once (open_namespaces) and passes
  the handles around rather than re-opening per step. __exit__/close() closes only
  the SOCKET; destroying the ns is the explicit netns.remove(). NOTE: because the
  socket is only bound INTO the ns, /proc/sys (sysctls) and the nftables ruleset
  -- which reflect the calling PROCESS's netns -- are NOT reachable through it;
  setting those needs a process actually IN the ns (see main.py's setns child).
- link('set', index=i, net_ns_fd=PATH): if PATH is a string containing '/', the
  IFLA_NET_NS_FD encoder opens it as a file directly (see ifinfmsg netns_fd),
  so our ./netns/<name> paths move a link into the target ns with no fd juggling.
"""

import contextlib
import os
from collections.abc import Generator, Iterable

from pyroute2 import NetNS, netns

NETNS_DIR = os.path.join(os.environ["HOME"], "netns")


def path_for(name: str) -> str:
    # full path -> pyroute2 uses it verbatim (keeps everything under $HOME/netns)
    return os.path.join(NETNS_DIR, name)


def find_ifindex(ns: NetNS, ifname: str) -> int | None:
    """ifindex of `ifname` in `ns`, or None if absent.

    Raises if MORE than one link matches: a lookup by name should be unique, so
    >1 is an ambiguity we surface rather than silently taking the first. Absent
    (0 matches) stays a legitimate None -- the existence checks (connect/verify)
    rely on it; use ifindex() where it must exist.

    link_lookup(ifname=...) hits pyroute2's fast path: a single RTM_GETLINK
    filtered by name (one netlink round-trip), returning [index] or []. We
    deliberately do NOT cache: an ifindex is reassigned when a link moves netns
    (see connect) or is deleted/recreated, so a cache keyed by name would hand
    back a stale index. The lookup is sub-millisecond anyway.
    """
    found = ns.link_lookup(ifname=ifname)
    if len(found) > 1:
        raise LookupError(
            f"interface {ifname!r} is ambiguous: {len(found)} matches ({found})")
    return found[0] if found else None


def ifindex(ns: NetNS, ifname: str) -> int:
    """ifindex of `ifname` in `ns`; raise if absent. Use where it must exist."""
    idx = find_ifindex(ns, ifname)
    if idx is None:
        raise LookupError(f"interface {ifname!r} not found in this namespace")
    return idx


@contextlib.contextmanager
def open_namespaces(names: Iterable[str]) -> Generator[dict[str, NetNS]]:
    """Open one NetNS socket per name; yield {name: NetNS}; close all on exit.

    flags=0 (not NetNS's default O_CREAT) so opening a MISSING namespace errors
    loudly instead of silently creating one: every ns must already exist via
    ensure_netns(), and we don't want to bypass its stale-placeholder handling.
    """
    with contextlib.ExitStack() as stack:
        yield {name: stack.enter_context(NetNS(path_for(name), flags=0))
               for name in names}


def ensure_netns(name: str) -> None:
    """Create one netns (if absent) at ./netns/<name>. Low-level on purpose.

    A live netns is a bind MOUNT, not a non-empty file (the file is size 0).
    ismount() is True while mounted, False after netns.remove() unmounts it.

    netns.create() opens the mountpoint O_CREAT|O_EXCL, so it requires NO file
    at the path. A stale 0-byte placeholder -- left behind when the mount ns /
    rootless pause process was torn down out-of-band (e.g. `podman system
    migrate`), which drops the mount but not the file -- is NOT a mount yet DOES
    exist. Remove it first; otherwise create() raises EEXIST, and pyroute2's
    ChildProcess wrapper turns that into a HANG rather than a clean error.
    """
    p = path_for(name)
    if os.path.ismount(p):
        print(f"exists, skipping: {p}")
        return
    if os.path.lexists(p):
        os.unlink(p)
        print(f"removed stale placeholder: {p}")
    netns.create(p)
    print(f"created netns: {p}")


def set_lo_up(ns: NetNS) -> None:
    """Bring up loopback in the namespace `ns` is bound to."""
    ns.link("set", index=ifindex(ns, "lo"), state="up")
