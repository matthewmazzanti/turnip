#!/usr/bin/env python3
"""
netns.py -- the namespace layer: enter podman's namespaces, create/open/query
netns, and run code inside a netns.

Everything the fabric does happens inside podman's rootless namespaces. This
module owns getting there (in_podman_context), the persistent-netns lifecycle
(create_netns / remove_netns), the ifindex lookups, and executing code inside a
specific netns
(run_in_netns / write_sysctls). main.py builds the routed dataplane on top.

(Note: this module is named `netns`, and it imports pyroute2's `netns` submodule
as the name `netns` for its own use -- they don't collide. Other modules import
the helpers by name, e.g. `from netns import create_netns`, so they never rebind
the `netns` name themselves.)

Why podman's rootless namespaces (and not --rootless-netns)
-----------------------------------------------------------
Empirically (confirmed on this host):
- A netns created in podman's user+mount namespace persists across separate
  invocations: the bind-mount is held alive by the rootless pause process.
  (A netns created under --rootless-netns does NOT persist -- podman tears that
  namespace down, mounts and all, once no container is holding it.)
- A netns is owned by the user namespace it was created in. `podman run` runs
  the container in podman's userns, so to `setns()` into our netns by path the
  netns must be owned by THAT userns -- i.e. created inside podman's user+mount
  namespace. Creating it as the plain login user's own userns would block the join.

Entering podman's namespaces in-process (in_podman_context)
-----------------------------------------------------------
Rather than wrap the whole script in `podman unshare`, in_podman_context() does
what that wrapper does, in-process: read the rootless pause process's pid
(bootstrapping it with `podman unshare true` if absent), fork a single-threaded
child, and os.setns() into the pause process's USER ns then MOUNT ns. The login
user is the OWNER of podman's userns (it created it), and per user_namespaces(7)
a process in the parent userns whose euid matches the owner has all capabilities
there -- so the unprivileged login user gains full caps on the join (and appears
as uid 0 inside, via podman's uid_map). The MOUNT ns makes the persistent
$HOME/netns/* bind-mounts visible; the USER ns gives CAP_NET_ADMIN over the
namespaces podman owns. The command runs entirely in that child; env stays intact
(no `podman unshare` reset), so PATH / `nft` / the venv interpreter resolve
normally. setns(CLONE_NEWUSER) needs a single-threaded caller; CPython + pyroute2
spawn no threads, so the fork is single-threaded.

Where owned infrastructure lives
--------------------------------
`IPRoute()`/`NetNS()` act on the netns the socket is opened in. Inside podman's
namespaces the current netns is still the (unowned) host netns -- we enter the
user+mount ns, not a net ns -- so creating an interface there returns EPERM. We
therefore host all owned networking infrastructure inside an owned, persistent
netns we created (the `router` netns): inside a netns we created we hold
CAP_NET_ADMIN, so link creation succeeds, and because that netns persists, so
does the infrastructure.

Sysctls and nftables are per-netns
----------------------------------
A NetNS socket is bound INTO a netns, but /proc/sys (sysctls) and the nftables
ruleset reflect the calling PROCESS's netns -- not reachable through that socket.
So setting sysctls / loading nft needs a process actually IN the target netns:
run_in_netns forks (from within the podman context) and os.setns(CLONE_NEWNET)es
in before touching them.

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
API has no DB and no snapshots, so there is nothing to fight.

pyroute2 notes
--------------
- netns.create(full_path) honors the path verbatim; runs unshare(CLONE_NEWNET)
  in a child and bind-mounts /proc/<child>/ns/net onto the file. It opens that
  mountpoint O_CREAT|O_EXCL, so the path must NOT already exist (see create_netns
  for the stale-placeholder handling).
- `NetNS` is, as of pyroute2 0.9.1+, a thin wrapper around `IPRoute(netns=...)`:
  one netlink socket bound INTO the ns (no forked child). Cheap to hold open and
  reuse, so the `main.Model` context manager opens each ns once and stashes the
  handle on the node. close() closes only the SOCKET; destroying the ns is
  remove_netns().
- link('set', index=i, net_ns_fd=PATH): if PATH is a string containing '/', the
  IFLA_NET_NS_FD encoder opens it as a file directly, so our ./netns/<name> paths
  move a link into the target ns with no fd juggling.
"""

import os
import socket
import subprocess
import sys
import traceback
from collections.abc import Callable

from pyroute2 import IPRoute, NetNS, netns

from .config import ResolvedRuntime

# This module holds no netns-root state: every helper takes a full path that
# `main` builds from the resolved `runtime.state_dir` (so the env read stays in the
# shell). Paths may carry a subdir -- routers/<net>, containers/<container>.


# --- entering podman's rootless namespaces ---------------------------------


def _pause_pid(runtime_dir: str) -> int:
    """PID of podman's rootless pause process (holds the user+mount ns alive).

    Read from <runtime_dir>/libpod/tmp/pause.pid; if missing or naming a dead
    process, bootstrap one with `podman unshare true` and re-read. `runtime_dir` is
    the TARGET user's runtime dir (passed in, not read from the env), so it's correct
    after a root->user drop under sudo, where $XDG_RUNTIME_DIR would still be root's.
    """
    path = os.path.join(runtime_dir, "libpod", "tmp", "pause.pid")

    def read() -> int:
        with open(path) as f:
            return int(f.read().strip())

    try:
        pid = read()
        if os.path.exists(f"/proc/{pid}"):
            return pid
    except (OSError, ValueError):
        pass
    subprocess.run(["podman", "unshare", "true"], check=True)
    return read()


def enter_podman(runtime: ResolvedRuntime) -> None:
    """Become the rootless user (iff we started as root) and enter podman's user +
    mount ns by owner-match. Call inside a freshly-forked, single-threaded child.

    The drop is root-only: you can only drop *to* a user from root, and as the user
    the launcher already did it (initgroups would need the CAP_SETGID we lack). After
    the drop euid == the podman-userns owner, so the setns joins are permitted and
    inside we map to container-uid-0. Env (XDG_RUNTIME_DIR/HOME) is corrected to the
    target user so `podman` + the libpod paths resolve (under sudo they'd be root's).

    (Capability-based privilege -- staying an unprivileged user holding CAP_NET_ADMIN
    rather than dropping from root -- is deferred; see todo.md.)"""
    if os.geteuid() == 0:
        os.setresgid(runtime.gid, runtime.gid, runtime.gid)
        os.initgroups(runtime.user, runtime.gid)
        os.setresuid(runtime.uid, runtime.uid, runtime.uid)
    runtime_dir = f"/run/user/{runtime.uid}"
    os.environ["XDG_RUNTIME_DIR"] = runtime_dir
    os.environ["HOME"] = str(runtime.home)
    pid = _pause_pid(runtime_dir)
    os.setns(os.open(f"/proc/{pid}/ns/user", os.O_RDONLY), os.CLONE_NEWUSER)
    os.setns(os.open(f"/proc/{pid}/ns/mnt", os.O_RDONLY), os.CLONE_NEWNS)


def in_podman_context(runtime: ResolvedRuntime, fn: Callable[[], None]) -> None:
    """Fork a single-threaded child that becomes the rootless user, enters podman's
    user+mount ns (enter_podman), and runs `fn()` there -- for side-effecting work
    that returns nothing (e.g. `down`'s netns teardown). Work that must hand fds back
    to the init-side parent uses collect_fds_from_child with a produce_fds that calls
    enter_podman.

    The child inherits stdout, so `fn`'s prints reach the terminal; we flush before
    os._exit (which skips buffering). A non-zero child exit fails this process."""
    sys.stdout.flush()  # flush BEFORE fork so the child can't inherit + re-emit our buffer
    sys.stderr.flush()
    child = os.fork()
    if child == 0:
        code = 0
        try:
            enter_podman(runtime)
            fn()
        except SystemExit as e:
            code = e.code if isinstance(e.code, int) else 1
        except BaseException:
            traceback.print_exc()
            code = 1
        finally:
            sys.stdout.flush()
            sys.stderr.flush()
        os._exit(code)
    _, status = os.waitpid(child, 0)
    if not (os.WIFEXITED(status) and os.WEXITSTATUS(status) == 0):
        sys.exit("operation failed inside podman namespace context")


# --- the host<->podman fd bridge (SCM_RIGHTS over SOCK_SEQPACKET) -----------
# The rootful two-phase flow's join: a podman-side child opens router-netns fds (the
# bind-mounts live in podman's MOUNT ns, invisible to the init ns) and ships them to
# the init-side parent, which can't see those paths but can use the fds directly.
# Fd-passing needs no privilege -- creds are checked at open() and at the privileged
# op (the veth move), never at transfer. One (name, fd) per message so name<->fd can't
# be misaligned, over SEQPACKET for explicit per-message framing.


def send_fds_by_name(sock: socket.socket, fds: dict[str, int]) -> None:
    """Send a {name: fd} mapping, one entry per message -- each name rides in the SAME
    SCM_RIGHTS message as its fd, so the two cannot be misaligned in transit."""
    for name, fd in fds.items():
        socket.send_fds(sock, [name.encode()], [fd])


def recv_fds_by_name(sock: socket.socket) -> dict[str, int]:
    """Receive what send_fds_by_name sent -> {name: fd}, looping until EOF (the sender
    closes the socket after the last entry). SEQPACKET => one message per recv."""
    out: dict[str, int] = {}
    while True:
        msg, fds, _flags, _addr = socket.recv_fds(sock, 4096, 1)
        if not fds:  # EOF: peer closed after the last fd
            return out
        out[msg.decode()] = fds[0]


def collect_fds_from_child(produce_fds: Callable[[], dict[str, int]]) -> dict[str, int]:
    """Fork; the child runs `produce_fds()` -> {name: fd}, which THIS function ships
    over the socket, then exits. The parent collects the mapping, waits for the child,
    and returns it. The returned fds outlive the child (independent open file
    descriptions; the netns they refer to are pinned by their bind-mounts), so the
    init-side caller can use them with the child long gone.

    Generic over what the child produces -- the podman entry + netns creation are the
    `produce_fds` callback's job, kept out of here so the bridge is testable without
    podman."""
    parent_sock, child_sock = socket.socketpair(socket.AF_UNIX, socket.SOCK_SEQPACKET)
    sys.stdout.flush()  # flush BEFORE fork so the child can't inherit + re-emit our buffer
    sys.stderr.flush()
    pid = os.fork()
    if pid == 0:
        parent_sock.close()
        code = 0
        try:
            send_fds_by_name(child_sock, produce_fds())
        except BaseException:
            traceback.print_exc()
            code = 1
        finally:
            child_sock.close()
            sys.stdout.flush()
            sys.stderr.flush()
        os._exit(code)
    child_sock.close()
    fds = recv_fds_by_name(parent_sock)
    parent_sock.close()
    _, status = os.waitpid(pid, 0)
    if not (os.WIFEXITED(status) and os.WEXITSTATUS(status) == 0):
        raise RuntimeError("fd-producing child failed (see its output above)")
    return fds


# --- netns lifecycle + handles ---------------------------------------------


def create_netns(p: str) -> None:
    """Create a fresh netns at full path `p`. Assumes the path is clear -- `up`
    runs clean-slate (down() then build), so the prior namespace is already gone.

    Guards the two ways a leftover could remain so netns.create()'s O_CREAT|O_EXCL
    can't EEXIST -> HANG (pyroute2's ChildProcess wrapper hangs on EEXIST rather
    than erroring): a still-live mount means teardown failed, so error loudly; a
    stale 0-byte placeholder (mount dropped out-of-band, e.g. `podman system
    migrate`) is just unlinked.
    """
    if os.path.ismount(p):
        raise RuntimeError(f"netns still mounted at {p} after teardown; refusing to clobber")
    if os.path.lexists(p):
        os.unlink(p)  # stale placeholder, not a live mount
    # `p` may carry a subdir (routers/<net>, containers/<container>); the
    # mountpoint's parent must exist before netns.create() opens it O_CREAT|O_EXCL.
    os.makedirs(os.path.dirname(p), exist_ok=True)
    netns.create(p)
    print(f"created netns: {p}")


def remove_netns(p: str) -> None:
    """Remove the netns at full path `p`; tolerate already-gone and stuck mounts.

    Removing a netns destroys everything inside it (links, routes, sysctls, and
    any per-netns nft table), so this is the whole teardown for a netns. If the
    unmount fails but a leftover file remains, unlink it so a later create() won't
    trip on the placeholder.
    """
    try:
        netns.remove(p)
        print(f"removed: {p}")
    except FileNotFoundError:
        print(f"already gone: {p}")
    except OSError as e:
        print(f"could not remove {p}: {e}")
        if os.path.exists(p):
            try:
                os.unlink(p)
            except OSError:
                pass


def find_ifindex(ns: IPRoute, ifname: str) -> int | None:
    """ifindex of `ifname` in `ns`, or None if absent. `ns` is any netlink socket --
    a `NetNS` bound into a netns, or a plain `IPRoute` (the init netns / host edge).

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
        raise LookupError(f"interface {ifname!r} is ambiguous: {len(found)} matches ({found})")
    return found[0] if found else None


def ifindex(ns: IPRoute, ifname: str) -> int:
    """ifindex of `ifname` in `ns`; raise if absent. Use where it must exist."""
    idx = find_ifindex(ns, ifname)
    if idx is None:
        raise LookupError(f"interface {ifname!r} not found in this namespace")
    return idx


def set_lo_up(ns: NetNS) -> None:
    """Bring up loopback in the namespace `ns` is bound to."""
    ns.link("set", index=ifindex(ns, "lo"), state="up")


# --- executing inside a netns (sysctls + nft) ------------------------------


def _run_entered(enter: Callable[[], None], fn: Callable[[], str]) -> str:
    """Fork, run `enter()` (which os.setns(CLONE_NEWNET)es into the target netns),
    then `fn()`, piping fn's returned string back. Forking keeps the caller's netns
    and its open pyroute2 sockets untouched; CLONE_NEWNET has no single-thread
    restriction. A non-zero child exit raises. Used for the two things pyroute2 can't
    do over its socket: writing /proc/sys and loading nft (both per-netns)."""
    r, w = os.pipe()
    child = os.fork()
    if child == 0:
        os.close(r)
        try:
            enter()
            os.write(w, fn().encode())
            os._exit(0)
        except Exception:
            traceback.print_exc()
            os._exit(1)
    os.close(w)
    out = b""
    while chunk := os.read(r, 65536):
        out += chunk
    os.close(r)
    _, status = os.waitpid(child, 0)
    if not (os.WIFEXITED(status) and os.WEXITSTATUS(status) == 0):
        raise RuntimeError("in-netns step failed (see child output)")
    return out.decode()


def run_in_netns(ns_path: str, fn: Callable[[], str]) -> str:
    """Run `fn` inside the netns at `ns_path` (opened by path -- caller is in the
    podman mount ns where the bind-mount is visible). Returns fn's printed string."""
    return _run_entered(lambda: os.setns(os.open(ns_path, os.O_RDONLY), os.CLONE_NEWNET), fn)


def run_in_netns_fd(ns_fd: int, fn: Callable[[], str]) -> str:
    """Run `fn` inside the netns referred to by `ns_fd` -- the init-side parent holds
    router-netns fds passed up from the podman child and can't see the bind-mount
    path, so it enters by fd (permitted via owner-match / cap-flow-down)."""
    return _run_entered(lambda: os.setns(ns_fd, os.CLONE_NEWNET), fn)


def write_sysctls(settings: dict[str, str]) -> None:
    """Write each `net.x.y = value` by translating the dotted key to its
    /proc/sys path. Interface names (with hyphens) carry no dots, so the simple
    dot->slash mapping is unambiguous. Runs inside the target netns."""
    for key, val in settings.items():
        with open("/proc/sys/" + key.replace(".", "/"), "w") as f:
            f.write(val)
