#!/usr/bin/env python3
"""
lan-attach.py -- ROOTFUL POC: a minimal cross-netns veth connection.

Lays one veth pair between the HOST netns and the internal container bridge:

    host netns          router netns (podman's, persistent)
    ----------          ---------------------------------
    lan-host  <======== veth ========>  lan-iot --(enslaved)--> br-iot

The host end (lan-host) is left for networkd to bridge onto the physical LAN.
The other end (lan-iot) is moved into the `router` netns and enslaved to
`br-iot` (the internal bridge created by main.py / networkd -- we do NOT create
it here).

    sudo ./.venv/bin/python lan-attach.py up
    sudo ./.venv/bin/python lan-attach.py down

Why root, and why we enter podman's namespaces by hand
------------------------------------------------------
Moving a netdevice across namespaces needs CAP_NET_ADMIN over BOTH ends; a
rootless userns never has it over the host netns, so the move runs as root.

The `router` netns is a bind-mount living in podman's MOUNT namespace, invisible
from the host -- so we can't open it by path here directly. Instead of the
pid-tracing trick (net_ns_pid), we reach it manually: read podman's pause-process
pid from XDG_RUNTIME_DIR, os.setns() into its MOUNT ns to make the bind-mount
live, open it to get an fd, move the veth end in by that fd, then os.setns() into
the NET ns to enslave it to br-iot. Real root keeps full caps throughout (it
outranks podman's userns), so it can drive both sides.

Layout: the veth is created up-front in the host netns (a pure host-netns op).
Only the namespace-crossing -- moving one end in, then enslaving it -- runs in a
fork()ed child. That child needs setns(CLONE_NEWNS), which requires a
single-threaded caller; pyroute2's IPRoute spawns no threads, so the parent forks
single-threaded and the child inherits a clean, unshared fs to setns into.
"""

import argparse
import os
import sys
import traceback

from pyroute2 import IPRoute

# Must match main.py: the persistent infra netns and the bridge inside it.
NETNS_DIR = os.path.join(os.environ["HOME"], "netns")
ROUTER = "router"
BRIDGE = "br-iot"

HOST_IF = "lan-host"  # stays in the host netns (networkd bridges this to the LAN)
IOT_IF = "lan-iot"  # moved into the router netns, enslaved to br-iot


def pause_pid_path() -> str:
    # The pause process belongs to the LOGIN user, not root: under sudo its
    # runtime dir is /run/user/<SUDO_UID>, not root's. Fall back to
    # XDG_RUNTIME_DIR when not invoked via sudo.
    uid = os.environ.get("SUDO_UID")
    runtime = f"/run/user/{uid}" if uid else os.environ.get("XDG_RUNTIME_DIR")
    if not runtime:
        sys.exit("can't locate runtime dir (run via sudo, or set XDG_RUNTIME_DIR)")
    return os.path.join(runtime, "libpod", "tmp", "pause.pid")


def read_pause_pid() -> int:
    path = pause_pid_path()
    try:
        with open(path) as f:
            return int(f.read().strip())
    except (OSError, ValueError) as e:
        sys.exit(f"could not read podman pause pid from {path}: {e}")


def _cross_into_router(mnt_fd: int) -> None:
    """Runs in the forked child. Enters podman's namespaces and does ONLY the
    cross-netns work: move the already-created IOT_IF end into the router netns,
    then enslave it to br-iot. Exits the process; never returns."""
    try:
        # Enter podman's MOUNT ns first, while still single-threaded, so the
        # router bind-mount becomes openable. Net ns is unchanged -> still host.
        os.setns(mnt_fd, os.CLONE_NEWNS)
        ns_fd = os.open(os.path.join(NETNS_DIR, ROUTER), os.O_RDONLY)

        # Still in the HOST netns: hand IOT_IF across the boundary, by fd.
        with IPRoute() as ipr:
            ipr.link("set", index=ipr.link_lookup(ifname=IOT_IF)[0], net_ns_fd=ns_fd)

        # Enter the router NET ns (by the fd we hold) and enslave the moved end.
        # CLONE_NEWNET has no single-thread restriction, so this is safe here.
        os.setns(ns_fd, os.CLONE_NEWNET)
        with IPRoute() as ipr:
            bridge = ipr.link_lookup(ifname=BRIDGE)
            if not bridge:
                sys.exit(
                    f"{BRIDGE} not found in {ROUTER} netns "
                    f"(bring the internal network up first)"
                )
            iidx = ipr.link_lookup(ifname=IOT_IF)[0]
            ipr.link("set", index=iidx, master=bridge[0])
            ipr.link("set", index=iidx, state="up")
        os._exit(0)
    except SystemExit as e:
        print(e, file=sys.stderr)
        os._exit(1)
    except Exception:
        traceback.print_exc()
        os._exit(1)


def up() -> None:
    if os.geteuid() != 0:
        sys.exit("must run as root")
    pid = read_pause_pid()
    mnt_fd = os.open(f"/proc/{pid}/ns/mnt", os.O_RDONLY)  # host /proc, pre-fork

    # Create the veth pair in the host netns -- a pure host-netns op, no ns hop.
    with IPRoute() as ipr:
        if ipr.link_lookup(ifname=HOST_IF):
            sys.exit(f"{HOST_IF} already exists -- run `down` first")
        ipr.link("add", ifname=HOST_IF, kind="veth", peer=IOT_IF)
        ipr.link("set", index=ipr.link_lookup(ifname=HOST_IF)[0], state="up")

    # Fork: the child does ONLY the cross-namespace move + enslave.
    child = os.fork()
    if child == 0:
        _cross_into_router(mnt_fd)  # never returns
    _, status = os.waitpid(child, 0)
    if not (os.WIFEXITED(status) and os.WEXITSTATUS(status) == 0):
        # Roll back the half-built pair so `up` stays retryable.
        with IPRoute() as ipr:
            idx = ipr.link_lookup(ifname=HOST_IF)
            if idx:
                ipr.link("del", index=idx[0])
        sys.exit("attach failed (see child output above); rolled back veth")

    print(f"attached: {HOST_IF}@host <-> {IOT_IF}@{BRIDGE} (in {ROUTER} netns)")
    print(f"  next: point networkd at {HOST_IF} to bridge it onto the LAN")


def down() -> None:
    if os.geteuid() != 0:
        sys.exit("must run as root")
    # The host end lives in the host netns -- delete it directly (no ns hop).
    # Removing it tears down the whole pair; the peer in router goes with it.
    with IPRoute() as ipr:
        idx = ipr.link_lookup(ifname=HOST_IF)
        if not idx:
            print(f"{HOST_IF}: already gone")
            return
        ipr.link("del", index=idx[0])
    print(f"detached {HOST_IF} (and its peer {IOT_IF} in {ROUTER})")


if __name__ == "__main__":
    ap = argparse.ArgumentParser(description="POC: cross-netns veth, host <-> br-iot")
    sub = ap.add_subparsers(dest="cmd", required=True)
    sub.add_parser("up", help="create the veth and attach it to br-iot")
    sub.add_parser("down", help="remove the veth")
    args = ap.parse_args()
    {"up": up, "down": down}[args.cmd]()
