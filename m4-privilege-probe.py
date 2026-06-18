#!/usr/bin/env python3
"""
m4-privilege-probe.py -- standalone proof of turnip's M4 privilege primitive.

NOT part of the package. A throwaway harness to prove the privilege plumbing in
isolation BEFORE any uplink/nft logic rides on it (IMPLEMENTATION-PLAN.md M4:
"prove fork + host-netns op + netns op + fd-pass in isolation first").

It demonstrates the unified, source-agnostic model we settled on:

  1. UNIVERSAL TOP LAYER -- become `user` + CAP_NET_ADMIN (ambient). When launched
     as root we re-exec through setpriv(1), the standard tool for this exact drop
     (it owns the keepcaps + setgid/initgroups/setresuid + raise-ambient dance, so
     we don't hand-roll it); when already the user with the ambient cap (the systemd
     User= + AmbientCapabilities path) it's a no-op. After this point the host-edge
     branch holds ONLY CAP_NET_ADMIN -- never full root, even under sudo (min priv).
  2. FORK into the two branches the userns split forces apart:
       - parent = HOST-EDGE branch: keeps the cap; does init-netns ops
         (veth, ip_forward, host nft masquerade), then moves the veth far-end into
         the router netns via an fd the child passes.
       - child  = NETNS branch: DROPS the cap entirely (it gets its powers from
         joining podman's userns by owner-match, not from any init-userns cap),
         enters podman's user+mount ns, creates a router netns, and hands its fd
         back over SCM_RIGHTS.
  3. The fd round-trip proves the load-bearing kernel fact: the host-edge parent,
     holding CAP_NET_ADMIN only in the INIT userns, can still operate on a netns
     owned by podman's (descendant) userns -- caps flow DOWN to descendants, and
     CAP_NET_ADMIN is exactly enough for the IFLA_NET_NS_FD move (no CAP_SYS_ADMIN,
     no uid 0). That is the whole proof that "user+cap == sudo" for the host edge.

Run it BOTH ways; both must reach the identical end state. Use the venv
interpreter explicitly -- under sudo a bare `python3` won't have pyroute2:

    # source = real root -> we re-exec through setpriv internally (the DROP path):
    sudo .venv/bin/python m4-privilege-probe.py <user>

    # source = user + ambient cap (the systemd service shape) -> our drop is a
    # no-op; we land as <user> holding only CAP_NET_ADMIN, no root:
    sudo setpriv --reuid <user> --regid <gid> --init-groups \
         --inh-caps +net_admin --ambient-caps +net_admin \
         .venv/bin/python m4-privilege-probe.py <user>

`<user>` is the rootless-podman owner (the run target). It must not be root.
"""

import ctypes
import ctypes.util
import os
import pwd
import socket
import subprocess
import sys

from pyroute2 import IPRoute, NetNS, netns

# --- capability syscalls (stdlib `os` covers uid/gid/setns/fork, not caps) ---

libc = ctypes.CDLL(ctypes.util.find_library("c") or "libc.so.6", use_errno=True)

# setpriv handles the acquire/drop-to-user direction; the only cap ops we still do
# in-process are the child's drop (clear ambient + zero the sets) and IS_SET reporting.
PR_CAP_AMBIENT = 47
PR_CAP_AMBIENT_IS_SET = 1
PR_CAP_AMBIENT_CLEAR_ALL = 4

CAP_NET_ADMIN = 12
CAP_MASK = 1 << CAP_NET_ADMIN  # CAP_NET_ADMIN lives in the first u32 (caps 0..31)
_CAP_VERSION_3 = 0x20080522


class _CapHeader(ctypes.Structure):
    _fields_ = [("version", ctypes.c_uint32), ("pid", ctypes.c_int)]


class _CapData(ctypes.Structure):
    # one struct per 32-cap block; version 3 => an array of 2 (caps 0..63)
    _fields_ = [
        ("effective", ctypes.c_uint32),
        ("permitted", ctypes.c_uint32),
        ("inheritable", ctypes.c_uint32),
    ]


def _prctl(option: int, arg2: int = 0, arg3: int = 0) -> int:
    r = libc.prctl(option, arg2, arg3, 0, 0)
    if r < 0:
        e = ctypes.get_errno()
        raise OSError(e, f"prctl({option}, {arg2}): {os.strerror(e)}")
    return r


def _capset(data: ctypes.Array[_CapData]) -> None:
    hdr = _CapHeader(_CAP_VERSION_3, 0)
    if libc.capset(ctypes.byref(hdr), data) != 0:
        e = ctypes.get_errno()
        raise OSError(e, f"capset: {os.strerror(e)}")


# --- state reporting (so both launch modes produce a comparable transcript) ---


def _capeff_has_net_admin() -> bool:
    """Read CapEff straight from /proc/self/status -- an independent view of the
    effective set, not our ctypes mutation, so the transcript is trustworthy."""
    for line in open("/proc/self/status"):
        if line.startswith("CapEff:"):
            return bool(int(line.split()[1], 16) & CAP_MASK)
    return False


def _ambient_has_net_admin() -> bool:
    return _prctl(PR_CAP_AMBIENT, PR_CAP_AMBIENT_IS_SET, CAP_NET_ADMIN) == 1


def report(tag: str) -> None:
    ruid, euid, suid = os.getresuid()
    print(
        f"  [{tag}] ruid/euid/suid={ruid}/{euid}/{suid}  "
        f"CAP_NET_ADMIN: eff={_capeff_has_net_admin()} ambient={_ambient_has_net_admin()}",
        flush=True,
    )


# --- step 1: the universal top layer -- become user + CAP_NET_ADMIN ----------


def become_user_with_cap(pw: pwd.struct_passwd) -> None:
    """The universal top layer: land as `user` + ambient CAP_NET_ADMIN.

    - Already there (systemd User=+AmbientCapabilities, or our own post-re-exec
      state): no-op.
    - Root: re-exec through setpriv(1), which performs the audited drop -- keepcaps,
      setresgid + initgroups (setgroups while still privileged: the CAP_SETGID step
      we can't do as the user), setresuid, and raise the inheritable + ambient cap.
      setpriv owns exactly this dance, so we don't hand-roll it. Ambient (not just
      effective) because the host edge shells out -- `nft -f` keeps the cap across
      execve.
    - Neither root nor already-cap'd: we can't acquire it -> fail loudly.

    The process's own (uid, cap) state is the re-exec guard: after setpriv we satisfy
    the first branch, so there is no loop and no sentinel env var. os.execvp replaces
    the image, so nothing after it runs -- main() re-enters from the top as user+cap."""
    if os.geteuid() == pw.pw_uid and _capeff_has_net_admin():
        return
    if os.geteuid() != 0:
        sys.exit(
            "no CAP_NET_ADMIN and not root: launch via sudo (we re-exec through "
            "setpriv to drop) or as the user with ambient CAP_NET_ADMIN"
        )
    print(f"  re-exec via setpriv -> {pw.pw_name!r} + ambient CAP_NET_ADMIN", flush=True)
    os.execvp(
        "setpriv",
        [
            "setpriv",
            "--reuid", str(pw.pw_uid),
            "--regid", str(pw.pw_gid),
            "--init-groups",
            "--inh-caps", "+net_admin",  # ambient requires the cap be inheritable
            "--ambient-caps", "+net_admin",
            sys.executable, os.path.abspath(__file__), pw.pw_name,
        ],
    )


def drop_cap_entirely() -> None:
    """The netns child sheds CAP_NET_ADMIN before podman work: it needs no init-
    userns cap (owner-match grants full caps INSIDE podman's userns on the join),
    so dropping is strictly safer and costs nothing. Unconditional."""
    _prctl(PR_CAP_AMBIENT, PR_CAP_AMBIENT_CLEAR_ALL)
    _capset((_CapData * 2)())  # zeroed effective/permitted/inheritable, both blocks


# --- step 2a: the HOST-EDGE branch (parent) -- init netns, keeps the cap ------

HOST_VETH = "veth-probe-h"  # stays in the init netns
ROUTER_VETH = "veth-probe-r"  # moved into the child's router netns via the passed fd
PROBE_NFT_TABLE = "turnip_probe"


def host_edge_branch(sock: socket.socket) -> None:
    """Runs in the init netns + init userns, holding ONLY CAP_NET_ADMIN.

    Proves the three host-edge op classes need nothing more than that cap:
    (a) write /proc/sys ip_forward, (b) load a host nft NAT table, (c) create a
    veth in the init netns -- then receive the router-netns fd from the child and
    move the far end in, the cross-userns step that is the crux of the model."""
    report("host-edge: start")

    # (a) ip_forward -- write the CURRENT value back: proves the CAP_NET_ADMIN-gated
    #     /proc/sys write without perturbing the host's real forwarding state.
    path = "/proc/sys/net/ipv4/ip_forward"
    cur = open(path).read().strip()
    open(path, "w").write(cur)
    print(f"  host-edge: wrote ip_forward (unchanged value {cur!r}) OK", flush=True)

    # (b) host nft NAT zone -- load then delete a throwaway masquerade table.
    ruleset = f"""
    table ip {PROBE_NFT_TABLE} {{
      chain postrouting {{
        type nat hook postrouting priority srcnat; policy accept;
        ip saddr 169.254.99.0/31 masquerade
      }}
    }}
    """
    subprocess.run(["nft", "-f", "-"], input=ruleset, text=True, check=True)
    subprocess.run(["nft", "delete", "table", "ip", PROBE_NFT_TABLE], check=True)
    print(f"  host-edge: loaded + removed nft NAT table 'ip {PROBE_NFT_TABLE}' OK", flush=True)

    # (c) veth in the init netns; one end will be moved into the child's netns.
    ipr = IPRoute()
    try:
        ipr.link("add", ifname=HOST_VETH, kind="veth", peer={"ifname": ROUTER_VETH})
        print(f"  host-edge: created veth {HOST_VETH} <-> {ROUTER_VETH} in init netns", flush=True)

        # Receive the router-netns fd the child opened inside podman's mount ns.
        msg, fds, _flags, _addr = socket.recv_fds(sock, 64, 1)
        if not fds:
            raise RuntimeError(f"no fd received from netns child (msg={msg!r})")
        router_fd = fds[0]
        print(f"  host-edge: received router-netns fd={router_fd} via SCM_RIGHTS", flush=True)

        # THE crux: move ROUTER_VETH into a netns owned by podman's userns, holding
        # CAP_NET_ADMIN only in the init userns. Permitted because caps flow down to
        # descendant userns and CAP_NET_ADMIN alone authorizes IFLA_NET_NS_FD.
        idx = ipr.link_lookup(ifname=ROUTER_VETH)[0]
        ipr.link("set", index=idx, net_ns_fd=router_fd)
        print(f"  host-edge: moved {ROUTER_VETH} into the passed netns fd OK", flush=True)

        os.close(router_fd)
        sock.send(b"moved")  # ack so the child can verify
    finally:
        # host end stays in init netns -> clean it up (the router end left with the
        # veth move; deleting either end reaps the pair).
        leftover = ipr.link_lookup(ifname=HOST_VETH)
        if leftover:
            ipr.link("del", index=leftover[0])
        ipr.close()
    report("host-edge: done")


# --- step 2b: the NETNS branch (child) -- drop cap, enter podman, own a netns -


def _pause_pid(runtime_dir: str) -> int:
    """podman's rootless pause pid (holds the user+mount ns alive). Bootstrap with
    `podman unshare true` if absent. Runs as the dropped user with the env already
    corrected (XDG_RUNTIME_DIR/HOME), so podman resolves the right runtime dir."""
    path = os.path.join(runtime_dir, "libpod", "tmp", "pause.pid")
    try:
        pid = int(open(path).read().strip())
        if os.path.exists(f"/proc/{pid}"):
            return pid
    except (OSError, ValueError):
        pass
    subprocess.run(["podman", "unshare", "true"], check=True)
    return int(open(path).read().strip())


def netns_branch(pw: pwd.struct_passwd, sock: socket.socket) -> None:
    """Runs as the user; drops the cap; enters podman's user+mount ns; creates a
    router netns and passes its fd to the host-edge parent; verifies the move."""
    drop_cap_entirely()
    report("netns: cap dropped")

    # Derive everything from the TARGET uid, never the (possibly root-owned) env --
    # the same hardening resolve_runtime will get. Correct the env so the podman
    # subprocess + libpod paths resolve to the user's runtime dir under sudo.
    runtime_dir = f"/run/user/{pw.pw_uid}"
    os.environ["XDG_RUNTIME_DIR"] = runtime_dir
    os.environ["HOME"] = pw.pw_dir

    pid = _pause_pid(runtime_dir)
    user_fd = os.open(f"/proc/{pid}/ns/user", os.O_RDONLY)
    mnt_fd = os.open(f"/proc/{pid}/ns/mnt", os.O_RDONLY)
    os.setns(user_fd, os.CLONE_NEWUSER)  # owner-match (euid==owner) grants full caps here
    os.setns(mnt_fd, os.CLONE_NEWNS)  # persistent bind-mounts now visible
    report("netns: in podman user+mnt ns")  # euid now maps to container-uid-0

    ns_path = os.path.join(runtime_dir, "turnip-probe", "router")
    os.makedirs(os.path.dirname(ns_path), exist_ok=True)
    if os.path.lexists(ns_path):
        netns.remove(ns_path)
    netns.create(ns_path)  # CAP_SYS_ADMIN via owner-match => unshare+bind-mount OK
    print(f"  netns: created router netns at {ns_path}", flush=True)

    try:
        ns_fd = os.open(ns_path, os.O_RDONLY)
        socket.send_fds(sock, [b"router-ns"], [ns_fd])
        os.close(ns_fd)
        print("  netns: sent router-netns fd to host-edge parent (SCM_RIGHTS)", flush=True)

        if sock.recv(16) != b"moved":
            raise RuntimeError("host-edge parent did not ack the veth move")

        # Verify the parent's cross-userns move actually landed in OUR netns.
        with NetNS(ns_path, flags=0) as ns:
            found = ns.link_lookup(ifname=ROUTER_VETH)
        if not found:
            raise RuntimeError(f"{ROUTER_VETH} not present in router netns after move")
        print(f"  netns: confirmed {ROUTER_VETH} in router netns (idx={found[0]})", flush=True)
    finally:
        netns.remove(ns_path)


# --- driver ------------------------------------------------------------------


def main() -> None:
    if len(sys.argv) != 2:
        sys.exit(f"usage: {sys.argv[0]} <rootless-podman-user>")
    user = sys.argv[1]
    pw = pwd.getpwnam(user)
    if pw.pw_uid == 0:
        sys.exit("target user must not be root (rootless podman is not root-owned)")

    print(f"== M4 privilege probe: target user {user!r} (uid={pw.pw_uid}) ==", flush=True)
    report("launch")

    become_user_with_cap(pw)
    report("after top-layer drop")
    if not _capeff_has_net_admin():
        sys.exit("no CAP_NET_ADMIN after drop: launch via sudo or with ambient CAP_NET_ADMIN")

    # Fork the two branches. parent = host-edge (keeps cap); child = netns (drops).
    parent_sock, child_sock = socket.socketpair(socket.AF_UNIX, socket.SOCK_STREAM)
    pid = os.fork()
    if pid == 0:
        parent_sock.close()
        code = 0
        try:
            netns_branch(pw, child_sock)
        except BaseException:
            import traceback

            traceback.print_exc()
            code = 1
        finally:
            child_sock.close()
            sys.stdout.flush()
        os._exit(code)

    child_sock.close()
    host_code = 0
    try:
        host_edge_branch(parent_sock)
    except BaseException:
        import traceback

        traceback.print_exc()
        host_code = 1
    finally:
        parent_sock.close()

    _, status = os.waitpid(pid, 0)
    child_ok = os.WIFEXITED(status) and os.WEXITSTATUS(status) == 0
    if host_code == 0 and child_ok:
        print("== PROBE OK: both branches succeeded, fd round-trip + cross-userns move proven ==")
    else:
        sys.exit(f"== PROBE FAILED: host_code={host_code} child_ok={child_ok} ==")


if __name__ == "__main__":
    main()
