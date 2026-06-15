#!/usr/bin/env python3
"""
main.py -- persistent rootless container network, ROUTED (Cilium-style) model.

Each container hangs off the `router` netns by its OWN /32 routed veth, and the
router forwards between them at L3. There is no shared L2 segment, so there is no
inter-container ARP to poison and no MAC to spoof: a container's only neighbour
is the router, which forwards by destination IP. This file is the orchestration +
CLI; the pieces it composes:

    fabric.py   the model: Host, HOSTS, FABRIC_FLOWS, addressing (ROUTER/GW/...)
    netns.py    enter podman's namespaces, create/open/run-inside netns
    nftlib.py   the nftables DSL + executor; build_nft (below) is the app policy
    verify.py   the `verify` command

Run as your normal login user -- NO `podman unshare` wrapper. main enters
podman's rootless namespaces itself (netns.in_podman_context), so a plain
invocation is enough; it just has to be the venv interpreter that has pyroute2:

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

Order of operations
-------------------
`up` wires first, then tightens: create the namespaces, bring up the gateway,
connect every spoke, and only THEN configure_dataplane() applies the router
sysctls + nft table. The per-veth sysctls (proxy_arp/rp_filter) require the veths
to already exist, so the dataplane pass runs last. `down` just removes the
namespaces -- the router netns owns fabric0, the routes, the sysctls, and the nft
table, so dropping it is a complete teardown (see netns.remove_netns).
"""

import os
import sys

from pyroute2 import NetNS

from fabric import (
    ALL_NS,
    FABRIC_FLOWS,
    FABRIC_IF,
    GW_IP,
    HOST_PREFIX,
    HOSTS,
    ROUTER,
    Host,
)
from netns import (
    NETNS_DIR,
    ensure_netns,
    find_ifindex,
    ifindex,
    in_podman_context,
    open_namespaces,
    path_for,
    remove_netns,
    run_in_netns,
    set_lo_up,
    write_sysctls,
)
import nftlib as nft
from verify import verify

# Map key types of the fabric's two verdict maps (the `type ...` of each).
FLOW_KEY = ["ipv4_addr", "ipv4_addr", "inet_proto", "inet_service"]
HOST_KEY = ["ipv4_addr", "ipv4_addr"]


def build_nft() -> nft.Ruleset:
    """The `inet fabric` ruleset (app policy) built on the nftlib `nft` builder.

    flush-and-reload (Table.reload) so re-running `up` replaces the table
    atomically. Maps precede the rules that reference them. The forward chain
    (policy drop) accepts: established/related (conntrack return path -- so flows
    are one-way in the maps); drops invalid; then for new conns -- ICMP only
    between gateway-authorized pairs, any-port gateway pairs (allowed_hosts), and
    service-scoped pairs (allowed_flows; `th dport` covers tcp AND udp). Else
    policy drop. rp_filter strict per veth is the spoof pin; rules match only v4.
    """
    ip = {h.name: h.ip for h in HOSTS}

    # allowed_flows elements: each FABRIC_FLOWS tuple, both directions.
    flow_elem: list[tuple[nft.Expr, nft.Expr]] = []
    for a, b, proto, port in FABRIC_FLOWS:
        for s, d in ((a, b), (b, a)):
            flow_elem.append((nft.concat(ip[s], ip[d], proto, port), nft.accept()))

    # allowed_hosts elements: every container <-> the gateway, both directions.
    host_elem: list[tuple[nft.Expr, nft.Expr]] = []
    for h in HOSTS:
        host_elem.append((nft.concat(h.ip, GW_IP), nft.accept()))
        host_elem.append((nft.concat(GW_IP, h.ip), nft.accept()))

    # saddr . daddr key, shared by the icmp and any-port host-pair rules.
    sd = [nft.payload("ip", "saddr"), nft.payload("ip", "daddr")]

    t = nft.Table("inet", "fabric")
    return nft.ruleset(
        [
            *t.reload(),
            t.chain("forward", type="filter", hook="forward", prio=0, policy="drop"),
            t.verdict_map("allowed_flows", FLOW_KEY, flow_elem),
            t.verdict_map("allowed_hosts", HOST_KEY, host_elem),
            # ct state established,related accept
            t.rule("forward", nft.ct_state("established", "related"), nft.accept()),
            # ct state invalid drop
            t.rule("forward", nft.ct_state("invalid"), nft.drop()),
            # ct state new meta l4proto icmp ip saddr . ip daddr vmap @allowed_hosts
            t.rule(
                "forward",
                nft.ct_state("new"),
                nft.match(nft.meta("l4proto"), "icmp"),
                nft.vmap(nft.concat(*sd), "allowed_hosts"),
            ),
            # ct state new ip saddr . ip daddr vmap @allowed_hosts
            t.rule(
                "forward",
                nft.ct_state("new"),
                nft.vmap(nft.concat(*sd), "allowed_hosts"),
            ),
            # ct state new ip saddr . ip daddr . meta l4proto . th dport vmap @allowed_flows
            t.rule(
                "forward",
                nft.ct_state("new"),
                nft.vmap(
                    nft.concat(*sd, nft.meta("l4proto"), nft.payload("th", "dport")),
                    "allowed_flows",
                ),
            ),
        ]
    )


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
    after every connect()); `default` covers any veth a later connect() adds."""
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
    sysctls = router_sysctls()
    rules = build_nft()  # typed Ruleset; load() renders it inside the netns

    def apply() -> str:
        write_sysctls(sysctls)
        nft.load(rules)
        return ""

    run_in_netns(path_for(ROUTER), apply)
    print(
        "  router dataplane: ip_forward + per-veth proxy_arp/rp_filter + "
        "ipv6 disabled + nft table 'fabric'"
    )


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
    router.link(
        "add",
        ifname=host.router_if,
        kind="veth",
        peer={"ifname": host.cont_if, "net_ns_fd": cont.status["netns"]},
    )
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
    print(
        f"  wired {host.name}: {host.cont_if} {host.ip}/{HOST_PREFIX} -> gw "
        f"{GW_IP} <-> {host.router_if}@{ROUTER} "
        f"(route {host.ip}/{HOST_PREFIX} dev {host.router_if})"
    )


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


def down() -> None:
    # Removing a netns destroys everything inside it: fabric0 + the routes die
    # with `router`, each veth pair dies with either end's netns, and the nft
    # table (per-netns) dies with router too. So removing the namespaces is a
    # complete teardown -- no per-route / per-sysctl / per-nft-element work.
    for name in [h.name for h in HOSTS] + [ROUTER]:
        remove_netns(name)


if __name__ == "__main__":
    cmd = sys.argv[1] if len(sys.argv) > 1 else "up"
    fn = {"up": up, "verify": verify, "down": down}.get(cmd)
    if fn is None:
        sys.exit(f"usage: {sys.argv[0]} {{up|verify|down}}")
    # Enter podman's rootless user+mount namespaces in-process, then run the
    # command there (replaces wrapping the invocation in `podman unshare`).
    in_podman_context(fn)
