#!/usr/bin/env python3
"""
m4-privilege-probe.py -- standalone proof of turnip's M4 host<->podman netns bridge.

NOT part of the package. A throwaway harness proving the privilege/namespace plumbing
in isolation before any uplink/nft logic rides on it.

SCOPE (deliberately reduced): this runs at **sudo level** -- as root throughout. The
capability route (run as an unprivileged user + ambient CAP_NET_ADMIN instead of
root) is DEFERRED; see todo.md. At sudo level the privilege handling is trivial: the
podman child does a plain setresuid drop to the rootless user (it needs no init-userns
cap -- owner-match re-grants caps inside podman's userns), and the host-edge parent
just stays root. No ctypes, no cap dance.

What this proves: the host<->podman netns bridge, as two SEQUENTIAL phases joined by
an fd-passing socket -- the design that replaces the old concurrent fork (which raced
on teardown):

  Phase 1  (podman world, a child): drop to the rootless user, enter podman's user +
    mount ns, create a router netns per network, send their fds back over a socket,
    and EXIT. The fds stay valid after the child is gone -- they're independent open
    file descriptions in the parent, and each netns is pinned by its bind-mount.

  Phase 2  (init world, the parent, still root): collect the fds into a {network: fd}
    mapping and run the host edge -- per network, a veth whose far end is moved into
    that netns fd (root has CAP_NET_ADMIN in init + CAP_SYS_ADMIN in podman's userns
    via cap-flow-down), plus a host op (ip_forward). No concurrency, no handshake.

Run (sudo only):
    sudo <python-with-pyroute2> m4-privilege-probe.py <rootless-podman-user>
e.g. in the dev VM:
    sudo /run/current-system/sw/bin/python3 /mnt/turnip/m4-privilege-probe.py homelab
"""

import json
import os
import pwd
import socket
import subprocess
import sys
import traceback
from collections.abc import Callable

from pyroute2 import IPRoute, netns

# --- reporting (CapEff read is pure /proc -- no ctypes) ---------------------

CAP_NET_ADMIN = 12


def _capeff_has_net_admin() -> bool:
    for line in open("/proc/self/status"):
        if line.startswith("CapEff:"):
            return bool(int(line.split()[1], 16) & (1 << CAP_NET_ADMIN))
    return False


def report(tag: str) -> None:
    ruid, euid, suid = os.getresuid()
    print(
        f"  [{tag}] uid {ruid}/{euid}/{suid}  CAP_NET_ADMIN(eff)={_capeff_has_net_admin()}",
        flush=True,
    )


# --- the fd bridge: pass netns fds across a socket (SCM_RIGHTS) -------------
# The reusable core to extract into turnip. A child opens fds (in podman's mount ns,
# where the netns bind-mounts live) and ships them to the parent (in the init mount
# ns, which can't see those paths). Fd-passing needs no privilege -- creds are checked
# at open() and at the privileged op (the veth move), never at transfer.

def send_fds_by_name(sock: socket.socket, fds: dict[str, int]) -> None:
    """Send a {name: fd} mapping, one entry per message -- each name rides in the SAME
    SCM_RIGHTS message as its fd, so the two can't be misaligned (no positional coupling
    to maintain, which for netns fds means no veth-into-wrong-network risk)."""
    for name, fd in fds.items():
        socket.send_fds(sock, [name.encode()], [fd])


def recv_fds_by_name(sock: socket.socket) -> dict[str, int]:
    """Receive what send_fds_by_name sent -> {name: fd}, looping until EOF (the sender
    closes the socket after the last entry). The socketpair is SOCK_SEQPACKET, so each
    recv returns exactly one message -- explicit framing, no reliance on SCM boundaries."""
    out: dict[str, int] = {}
    while True:
        msg, fds, _flags, _addr = socket.recv_fds(sock, 4096, 1)
        if not fds:  # EOF: peer closed after the last fd
            return out
        out[msg.decode()] = fds[0]


def collect_fds_from_child(produce_fds: Callable[[], dict[str, int]]) -> dict[str, int]:
    """Run `produce_fds()` in a forked child -- it RETURNS a {name: fd} mapping, which
    THIS function ships over the socket -- collect it in the parent, wait for the child
    to EXIT, and return it. Concentrating the fd transfer here keeps the work function
    socket-agnostic. The returned fds outlive the child (independent OFDs; the netns are
    pinned by their bind-mounts), so phase 2 can use them with the child long gone."""
    parent_sock, child_sock = socket.socketpair(socket.AF_UNIX, socket.SOCK_SEQPACKET)
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
        sys.exit("phase 1 (podman netns branch) failed -- see child output above")
    return fds


# --- phase 1: the netns branch (podman world) ------------------------------


def drop_to(pw: pwd.struct_passwd) -> None:
    """Plain root -> user drop (no caps retained). At sudo level the child needs no
    init-userns capability: owner-match grants a full set INSIDE podman's userns on
    the setns join. So this is just setresgid + initgroups + setresuid -- stdlib, no
    ctypes. (The capability-retaining drop for the no-root path is deferred -- todo.md.)"""
    os.setresgid(pw.pw_gid, pw.pw_gid, pw.pw_gid)
    os.initgroups(pw.pw_name, pw.pw_gid)
    os.setresuid(pw.pw_uid, pw.pw_uid, pw.pw_uid)


def _pause_pid(runtime_dir: str) -> int:
    """podman's rootless pause pid (holds the user+mount ns alive); bootstrap with
    `podman unshare true` if absent. Runs as the dropped user with the env corrected."""
    path = os.path.join(runtime_dir, "libpod", "tmp", "pause.pid")
    try:
        pid = int(open(path).read().strip())
        if os.path.exists(f"/proc/{pid}"):
            return pid
    except (OSError, ValueError):
        pass
    subprocess.run(["podman", "unshare", "true"], check=True)
    return int(open(path).read().strip())


def enter_podman(pw: pwd.struct_passwd) -> None:
    """Enter podman's user + mount ns by owner-match (euid == owner after drop_to)."""
    runtime_dir = f"/run/user/{pw.pw_uid}"
    os.environ["XDG_RUNTIME_DIR"] = runtime_dir
    os.environ["HOME"] = pw.pw_dir
    pid = _pause_pid(runtime_dir)
    os.setns(os.open(f"/proc/{pid}/ns/user", os.O_RDONLY), os.CLONE_NEWUSER)
    os.setns(os.open(f"/proc/{pid}/ns/mnt", os.O_RDONLY), os.CLONE_NEWNS)


def netns_branch(pw: pwd.struct_passwd, names: list[str]) -> dict[str, int]:
    """The child's whole job: drop, enter podman's ns, create a router netns per name,
    and RETURN {name: fd}. It does no socket work -- collect_fds_from_child owns the
    transfer (and then exits us)."""
    drop_to(pw)
    report("netns: dropped to user")
    enter_podman(pw)
    report("netns: in podman user+mnt ns")  # euid now maps to container-uid-0

    base = f"/run/user/{pw.pw_uid}/turnip-probe"
    os.makedirs(base, exist_ok=True)
    fds: dict[str, int] = {}
    for name in names:
        path = os.path.join(base, name)
        if os.path.lexists(path):
            netns.remove(path)  # reap a prior run's leftover (we're in podman's mnt ns)
        netns.create(path)
        fds[name] = os.open(path, os.O_RDONLY)
        print(f"  netns: created router netns {name!r}", flush=True)
    return fds


# --- phase 2: the host edge (init world, root) -----------------------------


def links_in(fd: int) -> list[str]:
    """Link names in the netns referred to by `fd`, read by entering it in a fork.
    The parent is root -> holds CAP_SYS_ADMIN in podman's userns via cap-flow-down,
    so it may setns into that (descendant-userns-owned) netns to look."""
    r, w = os.pipe()
    pid = os.fork()
    if pid == 0:
        os.close(r)
        os.setns(fd, os.CLONE_NEWNET)
        names = [link.get_attr("IFLA_IFNAME") for link in IPRoute().get_links()]
        os.write(w, json.dumps(names).encode())
        os._exit(0)
    os.close(w)
    buf = b""
    while chunk := os.read(r, 65536):
        buf += chunk
    os.close(r)
    os.waitpid(pid, 0)
    return json.loads(buf.decode())


def host_edge(netns_fds: dict[str, int]) -> None:
    """Runs in the parent (root, init netns) AFTER the podman child has exited. Per
    network: a host op (ip_forward) + a veth whose far end is moved into that network's
    router netns via the passed fd, verified by entering the netns."""
    report("host-edge: start (root, init netns)")

    path = "/proc/sys/net/ipv4/ip_forward"  # a host-edge op: write it back unchanged
    cur = open(path).read().strip()
    open(path, "w").write(cur)
    print(f"  host-edge: ip_forward write-back OK (unchanged {cur!r})", flush=True)

    ipr = IPRoute()
    created: list[str] = []
    try:
        for name, fd in netns_fds.items():
            host_if, router_if = f"vh-{name}", f"vr-{name}"
            ipr.link("add", ifname=host_if, kind="veth", peer={"ifname": router_if})
            created.append(host_if)
            ipr.link("set", index=ipr.link_lookup(ifname=router_if)[0], net_ns_fd=fd)
            present = router_if in links_in(fd)
            print(f"  host-edge: {name}: {router_if} present in netns: {present}", flush=True)
            if not present:
                raise RuntimeError(f"{router_if} not in {name!r} netns after move")
    finally:
        for host_if in created:  # delete the host end (reaps the pair); netns reaped next run
            leftover = ipr.link_lookup(ifname=host_if)
            if leftover:
                ipr.link("del", index=leftover[0])
        ipr.close()
    report("host-edge: done")


# --- driver ----------------------------------------------------------------

NETWORKS = ["lan", "dmz"]  # demo: two router netns, to exercise the {name: fd} mapping


def main() -> None:
    if len(sys.argv) != 2:
        sys.exit(f"usage: sudo {sys.argv[0]} <rootless-podman-user>")
    user = sys.argv[1]
    pw = pwd.getpwnam(user)
    if pw.pw_uid == 0:
        sys.exit("target user must not be root (rootless podman is not root-owned)")
    if os.geteuid() != 0:
        sys.exit("run under sudo -- this probe is sudo-level (user+cap path deferred; see todo.md)")

    print(f"== M4 bridge probe: user {user!r} (uid={pw.pw_uid}), networks {NETWORKS} ==")
    report("launch (root)")

    # Phase 1: podman world -- a child creates a router netns per network and ships the
    # fds back, then exits. Parent collects the {network: fd} mapping.
    netns_fds = collect_fds_from_child(lambda: netns_branch(pw, NETWORKS))
    report("after phase 1 (child exited; fds still valid)")
    print(f"  fd mapping: {netns_fds}", flush=True)

    # Phase 2: init world (root) -- host edge per network, using the collected fds.
    host_edge(netns_fds)
    print("== PROBE OK: podman->host fd bridge proven (sequential, sudo-level) ==")


if __name__ == "__main__":
    main()
