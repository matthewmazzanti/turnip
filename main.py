#!/usr/bin/env python3
"""
main.py -- persistent rootless container network, ROUTED (Cilium-style) model.

Each container hangs off the `router` netns by its OWN /32 routed veth, and the
router forwards between them at L3. There is no shared L2 segment, so there is no
inter-container ARP to poison and no MAC to spoof: a container's only neighbour
is the router, which forwards by destination IP. The plumbing this builds on
(persistent netns under $HOME/netns, why plain `podman unshare`, the low-level
pyroute2 choice) lives in netns_util.py -- read that first for the "why rootless"
rationale; this file is the routed dataplane.

Run under PLAIN `podman unshare`, calling the venv interpreter by path (unshare
resets the environment, so a bare `python3` misses pyroute2):

    podman unshare ./.venv/bin/python main.py up
    podman unshare ./.venv/bin/python main.py verify
    podman unshare ./.venv/bin/python main.py down

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
(no host uplink: in rootless `podman unshare` we sit in the unowned host netns
and cannot create the host end of an uplink veth -- that is a separate, rootful
job). With no uplink there is no default route, so a purely-virtual gateway would
never answer. So we make the gateway REAL: 10.0.0.1/32 on a `dummy` (fabric0). A
local address is answered by the normal ARP responder on whichever veth the
request arrives on, so the gateway resolves without an uplink. We still set
proxy_arp=1 on each fabric veth -- harmless here (containers only ARP for the
gateway) and correct the day a rootful uplink + default route are added.

Sysctls and nftables: why a forked setns child (not pyroute2)
-------------------------------------------------------------
pyroute2 drives links/addrs/routes over a netlink socket bound INTO a netns, and
we use it for all of those. But sysctls (/proc/sys/net) and the nft ruleset are
properties of the CALLING PROCESS's netns, not of a bound socket -- so to set
ip_forward / proxy_arp / rp_filter / disable_ipv6 and load the nft table in
`router`, a process must BE in the router netns. We fork a child, os.setns(
CLONE_NEWNET) into router, then write /proc/sys directly and exec `nft -f -`
there (see _run_in_netns). Under `podman unshare` the netns bind-mount is already
visible (we're in podman's mount ns), so only the NET hop is needed. Forking
keeps the parent's netns and its open pyroute2 sockets untouched.

`nft` is the one external binary we shell out to: rendering this map/vmap ruleset
through pyroute2's low-level nftables API would be far less legible than the CLI
syntax the policy is naturally written in. We resolve its absolute path in the
parent (PATH is intact there) and pass it into the child, since `podman unshare`
resets PATH.

Teardown
--------
`down` just removes the namespaces. Removing `router` destroys fabric0, every
vethR-* (and thus each pair, reaping the container end), all routes, the sysctls,
AND the nft table (an nft table is per-netns and dies with it). So the per-element
route/sysctl/nft deletes a *live reconfiguration* would need are unnecessary for a
full teardown -- the netns is the single owner of all of it.
"""

import os
import shutil
import socket
import subprocess
import sys
import traceback
from collections.abc import Callable

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


# --- nftables ruleset ------------------------------------------------------

def render_nft() -> str:
    """Render the `inet fabric` ruleset from HOSTS + FABRIC_FLOWS.

    Generated from the same tables that drive the dataplane so the policy can't
    drift from the wiring. Loaded flush-and-reload (create-empty / delete /
    define) so re-running `up` replaces the table atomically and idempotently.

    The forward chain (policy drop) accepts in order:
      1. established/related (conntrack return path -- so flows are one-way in
         the maps); invalid dropped.
      2. ICMP only between L3-authorized pairs (here: container<->gateway), for
         PMTU etc. -- ICMP has no port and would otherwise die in the L4 map.
      3. any-port L3 pairs (allowed_hosts: every container <-> the gateway).
      4. service-scoped pairs (allowed_flows; `th dport` covers tcp AND udp).
    Anything else hits policy drop. No input chain / no MAC checks: rp_filter
    strict on each fabric veth is the spoofing pin. IPv6 is disabled router-wide
    (see router_sysctls), and these rules match only `ip` (v4), so any stray v6
    forward would also hit policy drop.
    """
    ip = {h.name: h.ip for h in HOSTS}

    flow_lines: list[str] = []
    for a, b, proto, port in FABRIC_FLOWS:
        flow_lines.append(f"            {ip[a]} . {ip[b]} . {proto} . {port} : accept")
        flow_lines.append(f"            {ip[b]} . {ip[a]} . {proto} . {port} : accept")
    flow_elems = ",\n".join(flow_lines)

    host_lines: list[str] = []
    for h in HOSTS:
        host_lines.append(f"            {h.ip} . {GW_IP} : accept")
        host_lines.append(f"            {GW_IP} . {h.ip} : accept")
    host_elems = ",\n".join(host_lines)

    return f"""\
table inet fabric {{}}
delete table inet fabric
table inet fabric {{
    map allowed_flows {{
        type ipv4_addr . ipv4_addr . inet_proto . inet_service : verdict
        elements = {{
{flow_elems}
        }}
    }}

    map allowed_hosts {{
        type ipv4_addr . ipv4_addr : verdict
        elements = {{
{host_elems}
        }}
    }}

    chain forward {{
        type filter hook forward priority 0; policy drop;

        ct state established,related accept
        ct state invalid drop

        ct state new meta l4proto icmp \\
            ip saddr . ip daddr vmap @allowed_hosts

        ct state new ip saddr . ip daddr vmap @allowed_hosts

        ct state new \\
            ip saddr . ip daddr . meta l4proto . th dport \\
            vmap @allowed_flows
    }}
}}
"""


def _find_nft() -> str:
    """Absolute path to `nft`. Resolved in the PARENT, where PATH is intact;
    `podman unshare` resets PATH, so the forked child can't look it up itself.
    Falls back to common system locations (incl. NixOS's current-system path)."""
    found = shutil.which("nft")
    if found:
        return found
    for cand in ("/run/current-system/sw/bin/nft", "/usr/sbin/nft",
                 "/usr/bin/nft", "/sbin/nft"):
        if os.path.exists(cand):
            return cand
    raise FileNotFoundError(
        "nft binary not found (looked in PATH and common locations)")


# --- in-netns execution (sysctls + nft) ------------------------------------

def _run_in_netns(ns_path: str, fn: Callable[[], str]) -> str:
    """Run `fn` inside the netns at `ns_path`; return whatever it prints back.

    Forks, os.setns(CLONE_NEWNET) into the target netns, runs fn(), and pipes
    fn's returned string back to the parent. Used for the two things pyroute2
    can't do over its socket: writing /proc/sys (per-netns) and loading nft
    (per-netns). Only the NET hop is needed -- under `podman unshare` the netns
    bind-mount is already visible (we're in podman's mount ns).

    Forking keeps the PARENT's netns and its open pyroute2 sockets untouched.
    CLONE_NEWNET has no single-thread restriction (pyroute2 spawns no threads
    anyway), so this is safe. A non-zero child exit raises.
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
    ruleset = render_nft()

    def apply() -> str:
        _write_sysctls(sysctls)
        proc = subprocess.run([nft, "-f", "-"], input=ruleset,
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
    {"up": up, "verify": verify, "down": down}.get(
        cmd, lambda: sys.exit(f"usage: {sys.argv[0]} {{up|verify|down}}")
    )()
