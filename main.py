#!/usr/bin/env python3
"""
main.py — persistent rootless container network (netns + bridge + veths).

Run under PLAIN `podman unshare` (NOT --rootless-netns). Note that podman
unshare resets the environment, so call the venv interpreter by absolute/relative
path -- a bare `python3` resolves to system Python without pyroute2:

    podman unshare ./.venv/bin/python main.py up
    podman unshare ./.venv/bin/python main.py verify
    podman unshare ./.venv/bin/python main.py down

Then attach containers to the persistent namespaces by path:

    podman run --network ns:$HOME/netns/zwave ...

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

Where the bridge lives
----------------------
`IPRoute()`/`NetNS()` act on the netns the socket is opened in. Under plain
`podman unshare` the current netns is the (unowned) host netns, so creating a
bridge there returns EPERM. We therefore host the bridge inside an owned,
persistent infra netns (`infra`): inside a netns we created we hold
CAP_NET_ADMIN, so the bridge add succeeds, and because that netns persists, so
does the bridge. The `infra` netns is the home for ALL owned networking
infrastructure: the bridge today, and the host-side veth ends.

Topology
--------
    infra netns:   br-iot 10.88.0.1/24
                   |- v-zwave  --\\
                   |- v-hass    -- veth pairs (host ends enslaved to br-iot)
                   |- v-proxy  --/
    zwave  netns:  c-zwave 10.88.0.11/24  mac 0a:58:0a:58:00:0b
    hass   netns:  c-hass  10.88.0.12/24  mac 0a:58:0a:58:00:0c
    proxy  netns:  c-proxy 10.88.0.13/24  mac 0a:58:0a:58:00:0d

Addressing is a hardcoded name->IP table (see HOSTS); each MAC is DERIVED from
the IPv4 address (0a:58 + the four address octets -- the same locally-
administered scheme the Linux-bridge CNI plugin uses), so the IP defines the
MAC: stable across rebuilds and greppable straight out of a packet capture.

Why low-level pyroute2 (and not NDB)
------------------------------------
We drive everything with the low-level IPRoute/NetNS API: one netlink socket per
netns, imperative verbs. We TRIED the high-level NDB ("builder") API -- see
ndb-spike.py -- and it reads/creates fine, but this workload is unusually
namespaced: we create veths whose two ends live in DIFFERENT netns, then mutate
the moved end. NDB is a transactional SQLite-backed projection that aggregates
many sources; a commit snapshots an object AND its dependencies for rollback,
and a veth's dependency is its peer in another source. Snapshotting cross-source
dependencies is its least-robust corner and it failed inconsistently here (a
link-level `set` on a moved veth end crashed inside the snapshot/shadow-table
machinery). The low-level API has no DB and no snapshots, so there is nothing to
fight -- the right fit for raw namespace surgery.

pyroute2 notes
--------------
- netns.create(full_path) honors the path verbatim; runs unshare(CLONE_NEWNET)
  in a child and bind-mounts /proc/<child>/ns/net onto the file. It opens that
  mountpoint O_CREAT|O_EXCL, so the path must NOT already exist (see ensure_netns
  for the stale-placeholder handling).
- `NetNS` is, as of pyroute2 0.9.1+, a thin wrapper around `IPRoute(netns=...)`:
  one netlink socket bound INTO the ns (no forked child). It's cheap to hold
  open and reuse for many ops, so `up` opens each ns once (open_namespaces) and
  passes the handles around rather than re-opening per step. __exit__/close()
  closes only the SOCKET; destroying the ns is the explicit netns.remove().
- link('set', index=i, net_ns_fd=PATH): if PATH is a string containing '/', the
  IFLA_NET_NS_FD encoder opens it as a file directly (see ifinfmsg netns_fd),
  so our ./netns/<name> paths move a link into the target ns with no fd juggling.
"""

import contextlib
import os
import sys
from collections.abc import Generator, Iterable
from dataclasses import dataclass

from pyroute2 import NetNS, netns

NETNS_DIR = os.path.join(os.environ["HOME"], "netns")

# Owned infra netns: hosts the bridge and every host-side veth end.
INFRA = "infra"

BRIDGE = "br-iot"
BRIDGE_IP = "10.88.0.1"
PREFIX = 24


@dataclass(frozen=True)
class Host:
    """A container netns wired to the bridge: netns name, IP, and MAC.

    Build instances with `Host.alloc(name, ip)`: the MAC is derived from the
    IPv4 address (0a:58 prefix + the four octets in hex), so the IP uniquely
    and deterministically defines the MAC. 0x0a has the locally-administered
    bit set and the multicast bit clear, i.e. a valid unicast LAA.
    """

    name: str
    ip: str
    mac: str

    @classmethod
    def alloc(cls, name: str, ip: str) -> "Host":
        octets = [int(o) for o in ip.split(".")]
        if len(octets) != 4 or any(not 0 <= o <= 255 for o in octets):
            raise ValueError(f"bad IPv4 for {name}: {ip!r}")
        mac = "0a:58:" + ":".join(f"{o:02x}" for o in octets)
        return cls(name=name, ip=ip, mac=mac)

    @property
    def host_if(self) -> str:
        return f"v-{self.name}"   # infra side, enslaved to bridge

    @property
    def cont_if(self) -> str:
        return f"c-{self.name}"   # container side


# Hardcoded allocation: name -> IP (and, by derivation, MAC).
HOSTS = [
    Host.alloc("zwave", "10.88.0.11"),
    Host.alloc("hass", "10.88.0.12"),
    Host.alloc("proxy", "10.88.0.13"),
]

ALL_NS = [INFRA] + [h.name for h in HOSTS]


def path_for(name: str) -> str:
    # full path -> pyroute2 uses it verbatim (keeps everything under $HOME/netns)
    return os.path.join(NETNS_DIR, name)


def find_ifindex(ns: NetNS, ifname: str) -> int | None:
    """ifindex of `ifname` in `ns`, or None if absent.

    Raises if MORE than one link matches: a lookup by name should be unique, so
    >1 is an ambiguity we surface rather than silently taking the first. Absent
    (0 matches) stays a legitimate None -- the existence checks
    (create_bridge/connect/verify) rely on it; use ifindex() where it must exist.

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


# --- namespaces ------------------------------------------------------------

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


# --- bridge (inside the infra netns) ---------------------------------------

def create_bridge(infra: NetNS) -> int:
    """Create + address + bring up `br-iot` in the infra netns; return its index.

    Returning the ifindex lets `up` fetch it once and pass it to every
    `connect`, instead of each call re-looking-up the (stable-for-the-run)
    bridge. Idempotent: skips create if the bridge exists, and addr('replace',
    ...) won't error on a duplicate address.
    """
    br = find_ifindex(infra, BRIDGE)
    if br is None:
        infra.link("add", ifname=BRIDGE, kind="bridge")
        br = ifindex(infra, BRIDGE)
        print(f"created bridge: {BRIDGE}")
    else:
        print(f"bridge exists, skipping create: {BRIDGE}")
    infra.addr("replace", index=br, address=BRIDGE_IP, prefixlen=PREFIX)
    infra.link("set", index=br, state="up")
    print(f"  {BRIDGE} addressed {BRIDGE_IP}/{PREFIX}, set up")
    return br


# --- veth (infra bridge <-> container netns) -------------------------------

def connect(infra: NetNS, cont: NetNS, br_idx: int, host: Host) -> None:
    """Wire container netns `host.name` to the bridge with a veth pair.

    `infra` is the infra-netns handle, `cont` the container-netns handle,
    `br_idx` the bridge ifindex (from create_bridge). v-<name> stays in infra
    enslaved to the bridge; c-<name> is created directly in the container netns,
    given host.mac (while down), and addressed host.ip. Idempotent: if the
    host-side end already exists, assume it's wired and skip.
    """
    if find_ifindex(infra, host.host_if) is not None:
        print(f"veth exists, skipping: {host.host_if}")
        return

    # create the pair with the peer born DIRECTLY in the container netns: no
    # separate move step, and no infra-side lookup of the container end. The
    # target ns is taken from the `cont` handle itself (status["netns"] is the
    # full path it's bound to) so it can't drift from where cont actually points;
    # the '/'-containing path is opened verbatim by the ifinfmsg encoder.
    infra.link("add", ifname=host.host_if, kind="veth",
               peer={"ifname": host.cont_if, "net_ns_fd": cont.status["netns"]})
    hidx = ifindex(infra, host.host_if)
    infra.link("set", index=hidx, master=br_idx)
    infra.link("set", index=hidx, state="up")

    # the peer lives in `cont`'s namespace; its index there is unknowable in
    # advance, so look it up via the cont handle. link('add') returns only after
    # the kernel has created both ends, so a live lookup here always sees it (no
    # wait needed -- unlike NDB, whose async DB required interfaces.wait()).
    cidx = ifindex(cont, host.cont_if)
    cont.link("set", index=cidx, address=host.mac)   # set MAC while still down
    cont.addr("replace", index=cidx, address=host.ip, prefixlen=PREFIX)
    cont.link("set", index=cidx, state="up")
    print(f"  wired {host.name}: {host.cont_if} {host.ip}/{PREFIX} mac {host.mac} "
          f"<-> {host.host_if}@{BRIDGE}")


# --- orchestration ---------------------------------------------------------

def up() -> None:
    os.makedirs(NETNS_DIR, exist_ok=True)
    # 1. create every netns first -- sockets can only open into existing ns
    for name in ALL_NS:
        ensure_netns(name)
    # 2. open one socket per netns, reuse for all the work below
    with open_namespaces(ALL_NS) as ns:
        for name in ALL_NS:
            set_lo_up(ns[name])
            print(f"  lo up in {name}")
        br_idx = create_bridge(ns[INFRA])
        for host in HOSTS:
            connect(ns[INFRA], ns[host.name], br_idx, host)


def verify() -> None:
    present = [n for n in ALL_NS if os.path.ismount(path_for(n))]
    for n in ALL_NS:
        if n not in present:
            print(f"{n}: MISSING ({path_for(n)})")
    if not present:
        return

    with open_namespaces(present) as ns:
        # infra: bridge state + addrs + enslaved ports
        if INFRA in ns:
            r = ns[INFRA]
            br = find_ifindex(r, BRIDGE)
            if br is None:
                print(f"{BRIDGE}: MISSING (infra netns up but no bridge)")
            else:
                attrs = dict(r.get_links(br)[0]["attrs"])
                addrs = [dict(a["attrs"]).get("IFA_ADDRESS")
                         for a in r.get_addr(index=br)]
                ports = [dict(l["attrs"]).get("IFLA_IFNAME")
                         for l in r.get_links()
                         if dict(l["attrs"]).get("IFLA_MASTER") == br]
                print(f"{BRIDGE}: oper={attrs.get('IFLA_OPERSTATE')} "
                      f"addrs={addrs} ports={ports}")

        # container netns: container interface state + mac + address
        for host in HOSTS:
            if host.name not in ns:
                continue
            c = ns[host.name]
            cidx = find_ifindex(c, host.cont_if)
            if cidx is None:
                print(f"{host.name}: {host.cont_if} MISSING (netns up but no veth end)")
                continue
            attrs = dict(c.get_links(cidx)[0]["attrs"])
            mac = attrs.get("IFLA_ADDRESS")
            addrs = [dict(a["attrs"]).get("IFA_ADDRESS")
                     for a in c.get_addr(index=cidx)]
            mac_ok = "ok" if mac == host.mac else f"!= {host.mac}"
            print(f"{host.name}: {host.cont_if} oper={attrs.get('IFLA_OPERSTATE')} "
                  f"mac={mac} ({mac_ok}) addrs={addrs}")


def down() -> None:
    # Removing a netns destroys the links inside it: the bridge dies with
    # `infra`, and each veth pair dies with either end's netns. So tearing
    # down every namespace is sufficient -- no explicit link deletion needed.
    for name in [h.name for h in HOSTS] + [INFRA]:
        p = path_for(name)
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


if __name__ == "__main__":
    cmd = sys.argv[1] if len(sys.argv) > 1 else "up"
    {"up": up, "verify": verify, "down": down}.get(
        cmd, lambda: sys.exit(f"usage: {sys.argv[0]} {{up|verify|down}}")
    )()
