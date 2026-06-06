#!/usr/bin/env python3
"""
main.py -- persistent rootless container network, ROUTED (Cilium-style) model.

Each container hangs off the `router` netns by its OWN /32 routed veth, and the
router forwards between them at L3. There is no shared L2 segment, so there is no
inter-container ARP to poison and no MAC to spoof: a container's only neighbour
is the router, which forwards by destination IP. The plumbing this builds on
(persistent netns under $HOME/netns, why podman's rootless namespaces, the
low-level pyroute2 choice) lives in netns_util.py -- read that first for the "why
rootless" rationale; this file is the routed dataplane.

Run as your normal login user -- NO `podman unshare` wrapper. The script enters
podman's rootless namespaces itself (see "Entering podman's namespaces" below),
so a plain invocation is enough; it just has to be the venv interpreter that has
pyroute2:

    ./.venv/bin/python main.py up
    ./.venv/bin/python main.py verify
    ./.venv/bin/python main.py down

Then attach containers by netns path:

    podman run --network ns:$HOME/netns/zwave ...   (see run-container.sh)

Topology
--------
    router netns:  fabric0  10.0.0.1/32     (dummy; the virtual gateway)
                   |- vethR-zwave  route 10.0.0.11/32 dev vethR-zwave
                   |- vethR-hass   route 10.0.0.12/32 dev vethR-hass
                   |- vethR-proxy  route 10.0.0.13/32 dev vethR-proxy
                   ip_forward=1 ; per-veth proxy_arp=1, rp_filter=1 (strict)
                   ipv6 disabled ; nft table inet fabric: forward flow matrix
    zwave  netns:  eth0 10.0.0.11/32  default via 10.0.0.1
    hass   netns:  eth0 10.0.0.12/32  default via 10.0.0.1
    proxy  netns:  eth0 10.0.0.13/32  default via 10.0.0.1

Every container address is a /32: nothing is on-link, so each container gets an
explicit link-scope route to the gateway, then a default via it. The mirror on
the router side is a single /32 device route per veth -- and that one route is
load-bearing twice over: it's both "how to reach this container" (forwarding)
and "what source is legitimate arriving on this veth" (the reverse-path lookup
strict rp_filter does). One line, two jobs; that pairing is the whole model.

Design: routed, not bridged
---------------------------
On a bridge, every container shares an L2 domain: they ARP for each other, so a
container can answer ARP for a peer's IP (cache poisoning) or spoof a source MAC,
and you defend with MAC pins / ARP filtering / FDB hygiene in nftables. Routed,
there is no shared L2: a container's only neighbour is the router, the router
forwards by destination IP, and `rp_filter` strict on each fabric veth drops any
packet whose source IP doesn't reverse-route back out the veth it arrived on.
That is the IP anti-spoof pin; MAC is irrelevant at L3, so we derive no MAC and
pin none. nftables is then pure L3/L4 flow policy (the allowed_* maps) rather
than identity hygiene.

The virtual gateway (one self-contained deviation)
--------------------------------------------------
In a full Cilium-style fabric the gateway 10.0.0.1 is *virtual*: assigned to no
interface, and proxy_arp answers a container's ARP for it because the router has
a default route (via its uplink) that makes 10.0.0.1 "reachable via some other
interface" -- proxy_arp's precondition. This model is deliberately SELF-CONTAINED
(no host uplink: the host end of an uplink veth lives in the host netns, which is
a separate, rootful job). With no uplink there is no default route, so a purely-
virtual gateway would never answer. So we make the gateway REAL: 10.0.0.1/32 on a
`dummy` (fabric0). A local address is answered by the normal ARP responder on
whichever veth the request arrives on, so the gateway resolves without an uplink.
We still set proxy_arp=1 on each fabric veth -- harmless here (containers only
ARP for the gateway) and correct the day a rootful uplink + default route arrive.

Entering podman's rootless namespaces (in-process)
--------------------------------------------------
Everything must run inside podman's user+mount namespaces: the MOUNT ns so the
persistent $HOME/netns/* bind-mounts are visible, and the USER ns so we hold
CAP_NET_ADMIN over the namespaces podman owns (netns_util.py explains why
ownership must be podman's userns). Rather than wrap the whole script in `podman
unshare`, in_podman_context() does what that wrapper does, in-process: read the
rootless pause process's pid (bootstrapping it with `podman unshare true` if
absent), fork a single-threaded child, and os.setns() into the pause process's
user ns then mount ns. The login user is the OWNER of podman's userns (it created
it), and per user_namespaces(7) a process in the parent userns whose euid matches
the owner has all capabilities there -- so the unprivileged login user gains full
caps on the join (and appears as uid 0 inside, via podman's uid_map). The
dispatched command (up/verify/down) then runs entirely in that child. Keeping the
environment intact (no `podman unshare` reset) means PATH / `nft` / the venv
interpreter resolve normally.

Sysctls and nftables: per-netns, set from inside
------------------------------------------------
pyroute2 drives links/addrs/routes over a netlink socket bound INTO a netns. But
sysctls (/proc/sys/net) and the nft ruleset are properties of the calling
PROCESS's netns -- so to set ip_forward / proxy_arp / rp_filter / disable_ipv6
and load the nft table in `router`, a process must BE in the router netns.
_run_in_netns forks (from within the podman context we're already in) and
os.setns(CLONE_NEWNET)es into router before touching them. The ruleset is built
as libnftables JSON (build_nft) and piped to `nft -j -f -` -- structured
construction from HOSTS/FABRIC_FLOWS, no hand-formatted ruleset text to escape.

Teardown
--------
`down` just removes the namespaces. Removing `router` destroys fabric0, every
vethR-* (and thus each pair, reaping the container end), all routes, the sysctls,
AND the nft table (an nft table is per-netns and dies with it). So the per-element
route/sysctl/nft deletes a *live reconfiguration* would need are unnecessary for a
full teardown -- the netns is the single owner of all of it.
"""

import json
import os
import shutil
import socket
import subprocess
import sys
import traceback
from collections.abc import Callable
from typing import Any

from pyroute2 import NetNS, netns

# Model-agnostic netns plumbing (paths, ifindex lookups, ns open/create, lo-up,
# and the rootless/pyroute2 rationale). Shared so a future bridged or otherwise
# wired fabric can reuse it; this file only adds the routed dataplane.
from netns_util import (
    NETNS_DIR,
    ensure_netns,
    find_ifindex,
    ifindex,
    open_namespaces,
    path_for,
    set_lo_up,
)

# Owned, persistent router netns: hosts the gateway, every router-side veth end,
# the forwarding routes, and the nft table.
ROUTER = "router"

# The virtual gateway, made real on a dummy so it answers ARP with no uplink
# (see the module docstring). /32 everywhere: no on-link subnet exists.
FABRIC_IF = "fabric0"
GW_IP = "10.0.0.1"
HOST_PREFIX = 32


class Host:
    """A container netns wired to the router by a /32 routed veth.

    No MAC: the routed model pins identity at L3 (the per-veth /32 route +
    strict rp_filter), so a derived MAC has nothing to do here.
    """

    def __init__(self, name: str, ip: str) -> None:
        octets = [int(o) for o in ip.split(".")]
        if len(octets) != 4 or any(not 0 <= o <= 255 for o in octets):
            raise ValueError(f"bad IPv4 for {name}: {ip!r}")
        self.name = name
        self.ip = ip

    @property
    def router_if(self) -> str:
        return f"vethR-{self.name}"   # router side; route + rp_filter anchor

    @property
    def cont_if(self) -> str:
        return "eth0"                 # container side (one neighbour: the gw)


# Hardcoded allocation: name -> /32. A=zwave .11, B=hass .12, C=proxy .13.
HOSTS = [
    Host("zwave", "10.0.0.11"),
    Host("hass", "10.0.0.12"),
    Host("proxy", "10.0.0.13"),
]

ALL_NS = [ROUTER] + [h.name for h in HOSTS]

# Flow matrix: who may INITIATE to whom, L4-scoped. Hub-and-spoke with hass as
# the hub: zwave<->hass and hass<->proxy on tcp/443; zwave<->proxy is forbidden
# (no entry -> policy drop). Each tuple expands to BOTH directions in the nft
# map (either side may initiate the listed service). Return traffic rides
# conntrack, so it needs no entry.
FABRIC_FLOWS = [
    ("zwave", "hass", "tcp", 443),
    ("hass", "proxy", "tcp", 443),
]


# --- nftables ruleset (libnftables JSON) -----------------------------------
# Reusable expression fragments, so the rules below read close to their nft
# syntax. Verified against `nft -j list table inet fabric`.

_IP_SADDR: dict[str, Any] = {"payload": {"protocol": "ip", "field": "saddr"}}
_IP_DADDR: dict[str, Any] = {"payload": {"protocol": "ip", "field": "daddr"}}
_L4PROTO: dict[str, Any] = {"meta": {"key": "l4proto"}}
_TH_DPORT: dict[str, Any] = {"payload": {"protocol": "th", "field": "dport"}}


def _ct_state(state: str | list[str]) -> dict[str, Any]:
    """match `ct state <state>` (state: "new"/"invalid" or a list of states)."""
    return {"match": {"op": "in", "left": {"ct": {"key": "state"}}, "right": state}}


def _vmap(parts: list[dict[str, Any]], mapname: str) -> dict[str, Any]:
    """`<concat of parts> vmap @<mapname>` as a verdict-map lookup expr."""
    return {"vmap": {"key": {"concat": parts}, "data": f"@{mapname}"}}


def _table(verb: str) -> dict[str, Any]:
    return {verb: {"table": {"family": "inet", "name": "fabric"}}}


def _rule(*expr: dict[str, Any]) -> dict[str, Any]:
    return {"add": {"rule": {"family": "inet", "table": "fabric",
                             "chain": "forward", "expr": list(expr)}}}


def build_nft() -> dict[str, Any]:
    """Build the `inet fabric` ruleset as a libnftables JSON command list.

    Returns the dict fed (as JSON) to `nft -j -f -`. Replaces hand-formatted nft
    text: the dynamic part (the map elements) is generated from HOSTS /
    FABRIC_FLOWS, the static chain rules are plain dicts -- no escaping, no line
    continuations, no indentation to get wrong. The structure mirrors what
    `nft -j list` emits, so it round-trips.

    Loaded flush-and-reload (add-empty-table / delete / re-add) so re-running
    `up` replaces the table atomically and idempotently. The chain and maps are
    declared before the rules that reference them (@allowed_*). The forward chain
    (policy drop) accepts, in order: established/related (the conntrack return
    path -- so flows are one-way in the maps); drops invalid; then for new conns
    -- ICMP only between gateway-authorized pairs (PMTU etc.; ICMP has no port
    and would otherwise die in the L4 map), any-port gateway pairs
    (allowed_hosts), and service-scoped pairs (allowed_flows; `th dport` covers
    tcp AND udp). Anything else hits policy drop. No MAC checks: rp_filter strict
    on each veth is the spoof pin, and since these rules match only v4 (`ip`) and
    v6 is disabled router-wide, any stray v6 forward also hits policy drop.
    """
    ip = {h.name: h.ip for h in HOSTS}

    flow_elem: list[Any] = []
    for a, b, proto, port in FABRIC_FLOWS:
        for s, d in ((a, b), (b, a)):
            flow_elem.append([{"concat": [ip[s], ip[d], proto, port]},
                              {"accept": None}])

    host_elem: list[Any] = []
    for h in HOSTS:
        host_elem.append([{"concat": [h.ip, GW_IP]}, {"accept": None}])
        host_elem.append([{"concat": [GW_IP, h.ip]}, {"accept": None}])

    return {"nftables": [
        # flush-and-reload the table
        _table("add"), _table("delete"), _table("add"),
        # chain + maps must exist before the rules reference them
        {"add": {"chain": {"family": "inet", "table": "fabric",
                           "name": "forward", "type": "filter",
                           "hook": "forward", "prio": 0, "policy": "drop"}}},
        {"add": {"map": {"family": "inet", "table": "fabric",
                         "name": "allowed_flows", "map": "verdict",
                         "type": ["ipv4_addr", "ipv4_addr",
                                  "inet_proto", "inet_service"],
                         "elem": flow_elem}}},
        {"add": {"map": {"family": "inet", "table": "fabric",
                         "name": "allowed_hosts", "map": "verdict",
                         "type": ["ipv4_addr", "ipv4_addr"],
                         "elem": host_elem}}},
        # forward chain rules, in order
        _rule(_ct_state(["established", "related"]), {"accept": None}),
        _rule(_ct_state("invalid"), {"drop": None}),
        _rule(_ct_state("new"),
              {"match": {"op": "==", "left": _L4PROTO, "right": "icmp"}},
              _vmap([_IP_SADDR, _IP_DADDR], "allowed_hosts")),
        _rule(_ct_state("new"),
              _vmap([_IP_SADDR, _IP_DADDR], "allowed_hosts")),
        _rule(_ct_state("new"),
              _vmap([_IP_SADDR, _IP_DADDR, _L4PROTO, _TH_DPORT], "allowed_flows")),
    ]}


def _find_nft() -> str:
    """Absolute path to `nft`. With the in-process model the environment is
    intact (no `podman unshare` reset), so shutil.which normally finds it; the
    fallbacks (incl. NixOS's current-system path) are belt-and-suspenders."""
    found = shutil.which("nft")
    if found:
        return found
    for cand in ("/run/current-system/sw/bin/nft", "/usr/sbin/nft",
                 "/usr/bin/nft", "/sbin/nft"):
        if os.path.exists(cand):
            return cand
    raise FileNotFoundError(
        "nft binary not found (looked in PATH and common locations)")


# --- entering podman's rootless namespaces ---------------------------------

def _pause_pid() -> int:
    """PID of podman's rootless pause process (holds the user+mount ns alive).

    Read from $XDG_RUNTIME_DIR/libpod/tmp/pause.pid; if the file is missing or
    names a dead process, bootstrap one with `podman unshare true` (a no-op that
    spawns the pause process) and re-read. Runs in the PARENT, where the env and
    PATH are intact, so `podman` resolves.
    """
    runtime = os.environ.get("XDG_RUNTIME_DIR") or f"/run/user/{os.getuid()}"
    path = os.path.join(runtime, "libpod", "tmp", "pause.pid")

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


def in_podman_context(fn: Callable[[], None]) -> None:
    """Run `fn` inside podman's rootless user + mount namespaces.

    Replaces wrapping the whole script in `podman unshare`: fork a single-
    threaded child and os.setns() into the pause process's user ns (we are its
    owner -> full caps, and map to uid 0) then its mount ns (so the persistent
    $HOME/netns/* bind-mounts are visible). The child then runs `fn` -- the whole
    up/verify/down body -- in that context; deeper hops into individual netns
    (for sysctls/nft) are nested forks from here (see _run_in_netns).

    setns(CLONE_NEWUSER) requires a single-threaded caller; CPython + pyroute2
    spawn no threads, so the fork is single-threaded. The child inherits stdout,
    so `fn`'s prints reach the terminal directly -- we just flush before
    os._exit (which skips buffer flushing). A non-zero child exit propagates.
    """
    pid = _pause_pid()
    user_fd = os.open(f"/proc/{pid}/ns/user", os.O_RDONLY)
    mnt_fd = os.open(f"/proc/{pid}/ns/mnt", os.O_RDONLY)

    child = os.fork()
    if child == 0:
        code = 0
        try:
            os.setns(user_fd, os.CLONE_NEWUSER)
            os.setns(mnt_fd, os.CLONE_NEWNS)
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


# --- in-netns execution (sysctls + nft) ------------------------------------

def _run_in_netns(ns_path: str, fn: Callable[[], str]) -> str:
    """Run `fn` inside the netns at `ns_path`; return whatever it prints back.

    Forks (from within the podman context we're already in), os.setns(
    CLONE_NEWNET) into the target netns, runs fn(), and pipes fn's returned
    string back. Used for the two things pyroute2 can't do over its socket:
    writing /proc/sys (per-netns) and loading nft (per-netns). Only the NET hop
    is needed -- we already hold podman's user+mount ns.

    Forking keeps the caller's netns and its open pyroute2 sockets untouched.
    CLONE_NEWNET has no single-thread restriction. A non-zero child exit raises.
    """
    r, w = os.pipe()
    child = os.fork()
    if child == 0:
        os.close(r)
        try:
            ns_fd = os.open(ns_path, os.O_RDONLY)
            os.setns(ns_fd, os.CLONE_NEWNET)
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
        raise RuntimeError(f"in-netns step failed in {ns_path} (see child output)")
    return out.decode()


def _write_sysctls(settings: dict[str, str]) -> None:
    """Write each `net.x.y = value` by translating the dotted key to its
    /proc/sys path. Interface names (with hyphens) carry no dots, so the simple
    dot->slash mapping is unambiguous. Runs inside the target netns."""
    for key, val in settings.items():
        with open("/proc/sys/" + key.replace(".", "/"), "w") as f:
            f.write(val)


def router_sysctls() -> dict[str, str]:
    """The router netns sysctl set.

    ip_forward on (we route); all.rp_filter=0 so the per-veth values are
    authoritative (the kernel uses max(conf.all, conf.<if>), and a fresh netns
    may not default all to 0); then per fabric veth: proxy_arp=1 (answer the
    gateway ARP / future uplink) and rp_filter=1 (STRICT -- the anti-spoof pin,
    paired with that veth's /32 route).

    IPv6 is disabled router-wide: the routed model has no L2 path between
    containers, so the router's veth ends are the sole transit for any
    inter-container v6 -- killing v6 here severs it, even though each container
    keeps a (now inert) fe80:: link-local on its eth0. `all` propagates to the
    veths that already exist when configure_dataplane() applies this (it runs
    after every connect()); `default` covers any veth a later connect() adds.
    Applied AFTER the fabric veths exist so `all` actually reaches them."""
    s = {
        "net.ipv4.ip_forward": "1",
        "net.ipv4.conf.all.rp_filter": "0",
        "net.ipv6.conf.all.disable_ipv6": "1",
        "net.ipv6.conf.default.disable_ipv6": "1",
    }
    for h in HOSTS:
        s[f"net.ipv4.conf.{h.router_if}.proxy_arp"] = "1"
        s[f"net.ipv4.conf.{h.router_if}.rp_filter"] = "1"
    return s


def configure_dataplane() -> None:
    """One forked hop into `router`: write all sysctls, then load the nft table.

    Both depend on the fabric veths already existing (per-veth conf.* dirs, and
    rp_filter's reverse-path lookup), so `up` calls this AFTER every connect().
    """
    nft = _find_nft()
    sysctls = router_sysctls()
    ruleset = json.dumps(build_nft())

    def apply() -> str:
        _write_sysctls(sysctls)
        proc = subprocess.run([nft, "-j", "-f", "-"], input=ruleset,
                              text=True, capture_output=True)
        if proc.returncode != 0:
            # surface nft's diagnostic from inside the child before it exits 1
            sys.stderr.write(proc.stderr)
            raise RuntimeError(f"nft load failed (rc={proc.returncode})")
        return ""

    _run_in_netns(path_for(ROUTER), apply)
    print("  router dataplane: ip_forward + per-veth proxy_arp/rp_filter + "
          "ipv6 disabled + nft table 'fabric'")


# --- gateway + per-container wiring ----------------------------------------

def create_gateway(router: NetNS) -> None:
    """Create + address + bring up fabric0 (the gateway) in the router netns.

    A `dummy` holding 10.0.0.1/32: a real local address so the normal ARP
    responder answers containers' gateway ARP without an uplink (see docstring).
    Idempotent: skip create if present; addr('replace') tolerates a re-run.
    """
    idx = find_ifindex(router, FABRIC_IF)
    if idx is None:
        router.link("add", ifname=FABRIC_IF, kind="dummy")
        idx = ifindex(router, FABRIC_IF)
        print(f"created gateway iface: {FABRIC_IF}")
    else:
        print(f"gateway iface exists, skipping create: {FABRIC_IF}")
    router.addr("replace", index=idx, address=GW_IP, prefixlen=HOST_PREFIX)
    router.link("set", index=idx, state="up")
    print(f"  {FABRIC_IF} addressed {GW_IP}/{HOST_PREFIX} (virtual gateway), set up")


def connect(router: NetNS, cont: NetNS, host: Host) -> None:
    """Wire one container netns to the router with a /32 routed veth pair.

    vethR-<name> stays in router; eth0 is born directly in the container netns
    (peer net_ns_fd taken from the cont handle, so it can't drift). Container
    side: /32 address, an explicit link-scope route to the gateway (nothing is
    on-link under /32), then default via the gateway. Router side: the single
    /32 device route that is both the forwarding entry and rp_filter's
    reverse-path anchor. Idempotent: if vethR-<name> exists, assume wired, skip.

    Sysctls (proxy_arp/rp_filter) are NOT set here -- they need a process inside
    the router netns and are applied in one batch by configure_dataplane().
    """
    if find_ifindex(router, host.router_if) is not None:
        print(f"veth exists, skipping: {host.router_if}")
        return

    # Born in the right namespaces in one shot: peer (eth0) directly in cont.
    router.link("add", ifname=host.router_if, kind="veth",
                peer={"ifname": host.cont_if, "net_ns_fd": cont.status["netns"]})
    ridx = ifindex(router, host.router_if)
    router.link("set", index=ridx, state="up")
    # THE mapping: reach-this-container AND legit-source-on-this-veth, one route.
    router.route("add", dst=f"{host.ip}/{HOST_PREFIX}", oif=ridx)

    # Container end: index unknowable in advance, look it up via the cont handle
    # (link('add') returns only once the kernel made both ends, so it's there).
    cidx = ifindex(cont, host.cont_if)
    cont.addr("replace", index=cidx, address=host.ip, prefixlen=HOST_PREFIX)
    cont.link("set", index=cidx, state="up")
    # /32 => gateway is not on-link; pin it link-scope, then default via it.
    cont.route("add", dst=GW_IP, oif=cidx, scope="link")
    cont.route("add", dst="default", gateway=GW_IP, oif=cidx)
    print(f"  wired {host.name}: {host.cont_if} {host.ip}/{HOST_PREFIX} -> gw "
          f"{GW_IP} <-> {host.router_if}@{ROUTER} "
          f"(route {host.ip}/{HOST_PREFIX} dev {host.router_if})")


# --- orchestration ---------------------------------------------------------

def up() -> None:
    os.makedirs(NETNS_DIR, exist_ok=True)
    # 1. every netns first -- sockets can only open into an existing ns
    for name in ALL_NS:
        ensure_netns(name)
    # 2. one socket per netns, reused for all L3 work below
    with open_namespaces(ALL_NS) as ns:
        for name in ALL_NS:
            set_lo_up(ns[name])
            print(f"  lo up in {name}")
        create_gateway(ns[ROUTER])
        for host in HOSTS:
            connect(ns[ROUTER], ns[host.name], host)
    # 3. sysctls + nft need a process INSIDE router; do it once, after wiring
    configure_dataplane()


def _has_route(ns: NetNS, dst: str, dst_len: int, oif: int) -> bool:
    """True if a route to dst/dst_len out `oif` exists in `ns` (AF_INET)."""
    for r in ns.get_routes(family=socket.AF_INET):
        attrs = dict(r["attrs"])
        if (r["dst_len"] == dst_len and attrs.get("RTA_DST") == dst
                and attrs.get("RTA_OIF") == oif):
            return True
    return False


def _has_default_via(ns: NetNS, gw: str) -> bool:
    """True if `ns` has a default route (dst_len 0) via gateway `gw`."""
    for r in ns.get_routes(family=socket.AF_INET):
        if r["dst_len"] == 0 and dict(r["attrs"]).get("RTA_GATEWAY") == gw:
            return True
    return False


def _router_dataplane_report(nft: str) -> str:
    """Run inside router: read the sysctls and check the nft table. Returns a
    formatted block (piped back to the parent by _run_in_netns)."""
    def read(key: str) -> str:
        try:
            with open("/proc/sys/" + key.replace(".", "/")) as f:
                return f.read().strip()
        except OSError:
            return "?"

    lines = [f"  ip_forward={read('net.ipv4.ip_forward')} "
             f"all.rp_filter={read('net.ipv4.conf.all.rp_filter')} "
             f"ipv6.disabled={read('net.ipv6.conf.all.disable_ipv6')}"]
    for h in HOSTS:
        pa = read(f"net.ipv4.conf.{h.router_if}.proxy_arp")
        rp = read(f"net.ipv4.conf.{h.router_if}.rp_filter")
        lines.append(f"  {h.name}: proxy_arp={pa} rp_filter={rp}")
    chk = subprocess.run([nft, "list", "table", "inet", "fabric"],
                         text=True, capture_output=True)
    if chk.returncode == 0:
        nrules = sum(chk.stdout.count(v) for v in ("accept", "drop"))
        lines.append(f"  nft table 'fabric': present (~{nrules} verdicts)")
    else:
        lines.append("  nft table 'fabric': MISSING")
    return "\n".join(lines) + "\n"


def verify() -> None:
    present = [n for n in ALL_NS if os.path.ismount(path_for(n))]
    for n in ALL_NS:
        if n not in present:
            print(f"{n}: MISSING ({path_for(n)})")
    if not present:
        return

    with open_namespaces(present) as ns:
        # router: gateway, per-veth state + the load-bearing /32 route, then the
        # sysctls + nft table (read from inside via a forked hop)
        if ROUTER in ns:
            r = ns[ROUTER]
            gidx = find_ifindex(r, FABRIC_IF)
            if gidx is None:
                print(f"router: {FABRIC_IF} MISSING (router up but no gateway)")
            else:
                attrs = dict(r.get_links(gidx)[0]["attrs"])
                addrs = [dict(a["attrs"]).get("IFA_ADDRESS")
                         for a in r.get_addr(index=gidx)]
                print(f"router: {FABRIC_IF} oper={attrs.get('IFLA_OPERSTATE')} "
                      f"addrs={addrs}")

            for h in HOSTS:
                ridx = find_ifindex(r, h.router_if)
                if ridx is None:
                    print(f"  {h.name}: {h.router_if} MISSING (no fabric veth)")
                    continue
                attrs = dict(r.get_links(ridx)[0]["attrs"])
                route_ok = ("ok" if _has_route(r, h.ip, HOST_PREFIX, ridx)
                            else "MISSING")
                print(f"  {h.name}: {h.router_if} "
                      f"oper={attrs.get('IFLA_OPERSTATE')} "
                      f"route({h.ip}/{HOST_PREFIX} dev {h.router_if})={route_ok}")

            try:
                report = _run_in_netns(
                    path_for(ROUTER),
                    lambda: _router_dataplane_report(_find_nft()))
                print(report, end="")
            except Exception as e:
                print(f"  dataplane report unavailable: {e}")

        # containers: eth0 state + address + default route via the gateway
        for h in HOSTS:
            if h.name not in ns:
                continue
            c = ns[h.name]
            cidx = find_ifindex(c, h.cont_if)
            if cidx is None:
                print(f"{h.name}: {h.cont_if} MISSING (netns up but no veth end)")
                continue
            attrs = dict(c.get_links(cidx)[0]["attrs"])
            addrs = [dict(a["attrs"]).get("IFA_ADDRESS")
                     for a in c.get_addr(index=cidx)]
            gw_ok = "ok" if _has_default_via(c, GW_IP) else "MISSING"
            print(f"{h.name}: {h.cont_if} oper={attrs.get('IFLA_OPERSTATE')} "
                  f"addrs={addrs} default-via-{GW_IP}={gw_ok}")


def down() -> None:
    # Removing a netns destroys everything inside it: fabric0 + the bridge of
    # routes die with `router`, each veth pair dies with either end's netns, and
    # the nft table (per-netns) dies with router too. So removing the namespaces
    # is a complete teardown -- no per-route / per-sysctl / per-nft-element work.
    for name in [h.name for h in HOSTS] + [ROUTER]:
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
    fn = {"up": up, "verify": verify, "down": down}.get(cmd)
    if fn is None:
        sys.exit(f"usage: {sys.argv[0]} {{up|verify|down}}")
    # Enter podman's rootless user+mount namespaces in-process, then run the
    # command there (replaces wrapping the invocation in `podman unshare`).
    in_podman_context(fn)
