#!/usr/bin/env python3
"""
main.py -- CLI + orchestration, driven by the declarative config (turnip.json).

This is the imperative shell: it does the IO (read the config file + environment,
resolve the runtime), derives the concrete names the mechanism acts on, and calls
into the pure modules (`config` = model + validation, `netns` = namespace ops)
with explicit values. The modules hold no environment reads and no mutable state.

It grows one milestone at a time (the old literal-driven version is kept as
`main.py.bak` for reference -- see IMPLEMENTATION-PLAN.md):

    1. netns setup       create/remove a netns per container + per router
    2. netns linking  <-- here: gateway + /32 veth pairs + routes
    3. nft application   (the forward flow matrix per router netns)
    4. uplinks           (the rootful host edge)
    5. links             (container host-netdev holes)

Run as your normal login user -- no `podman unshare` wrapper. main enters
podman's rootless user+mount namespaces in-process (netns.in_podman_context):

    uv run turnip up        # create + wire the namespaces the config implies
    uv run turnip down      # remove them
"""

import json
import os
import pwd
import sys
from pathlib import Path

from pyroute2 import NetNS

from . import nftlib as nft
from .config import (
    HOST_PREFIX,
    Attachment,
    Network,
    NetworkType,
    Proto,
    ResolvedRuntime,
    Runtime,
    Turnip,
)
from .netns import (
    create_netns,
    ifindex,
    in_podman_context,
    open_namespaces,
    remove_netns,
    run_in_netns,
    set_lo_up,
    write_sysctls,
)

IFNAMSIZ = 15  # kernel cap on an interface name; a derived veth name must fit
NFT_TABLE = "turnip"  # the per-router-netns nft table (one per netns; constant name)

# Map key types of the two verdict maps (the `type ...` of each).
FLOW_KEY = ["ipv4_addr", "ipv4_addr", "inet_proto", "inet_service"]
HOST_KEY = ["ipv4_addr", "ipv4_addr"]

# --- IO: config discovery + runtime resolution (kept here, out of the modules) ---


def load_config() -> Turnip:
    """Read + validate the config. Discovery: $TURNIP_CONFIG, else ./turnip.json.
    The file/env reads live here; `config` only validates the parsed data."""
    path = Path(os.environ.get("TURNIP_CONFIG", "turnip.json"))
    return Turnip.model_validate(json.loads(path.read_text()))


def resolve_runtime(rt: Runtime) -> ResolvedRuntime:
    """Fill in `Runtime`'s environment-dependent defaults from the env + passwd db.

    User: explicit `runtime.user`, else `$SUDO_USER`, else the current login user
    -- the rootless baseline runs *as* the user (no sudo, $SUDO_USER unset), so the
    current-user fallback makes a plain `turnip up` work; an explicit `user`
    decouples ownership from the invoker. netns_dir defaults to <user>'s ~/netns.
    (The privileged path -- milestone 4 -- will require an explicit user.)"""
    user = rt.user or os.environ.get("SUDO_USER") or pwd.getpwuid(os.getuid()).pw_name
    pw = pwd.getpwnam(user)  # raises KeyError for an unknown user -- fail closed
    return ResolvedRuntime(
        user=user,
        uid=pw.pw_uid,
        gid=pw.pw_gid,
        netns_dir=rt.netns_dir or Path(pw.pw_dir) / "netns",
        nft=rt.nft,
        podman=rt.podman,
    )


# --- config -> the names the mechanism acts on -----------------------------
# Pure derivations (no IO). The seed of a lowered layout/IR; later milestones grow
# these into richer per-network / per-endpoint structures (see IMPLEMENTATION-PLAN
# "Compass"). A netns "name" here is RELATIVE to netns_dir (the stable logical key
# used to look up an open handle); main joins it to a full path at the netns
# boundary. The two symmetric subdirs mean a router can't collide with a container.


def router_netns(network: str) -> str:
    return f"routers/{network}"


def container_netns(container: str) -> str:
    # the leaf is the container name verbatim -- podman joins it by this path
    return f"containers/{container}"


def netns_names(turnip: Turnip) -> list[str]:
    """Every netns the config implies: one per declared container (the
    authoritative set -- a links-only container still needs its netns), one per
    network's router. The two scopes a later IR will grow from."""
    return [container_netns(name) for name in turnip.containers] + [
        router_netns(name) for name in turnip.networks
    ]


def router_if(container: str) -> str:
    """The router-side veth name for a container's attachment. For the baseline
    (one network) it only needs to be unique within the router netns, so it keys on
    the container; the general (network, container) scheme that fits IFNAMSIZ is
    deferred with multi-network. Rejects an over-long name rather than letting the
    kernel truncate it into a silent collision."""
    name = f"vethR-{container}"
    if len(name) > IFNAMSIZ:
        raise ValueError(
            f"router veth name {name!r} exceeds IFNAMSIZ ({IFNAMSIZ}); shorten {container!r}"
        )
    return name


# --- wiring (mechanism composed over the netns primitives) -----------------


def create_gateway(router: NetNS, network: Network) -> None:
    """Create + address + bring up the dummy gateway in a (fresh) router netns.

    A `dummy` holding <gateway>/32: a real local address, so the normal ARP
    responder answers containers' gateway ARP without an uplink. The router netns
    is recreated clean by `up`, so this just builds -- no existence checks."""
    if network.gateway_if is None:  # validator guarantees this for a router network
        raise ValueError("router network has no gateway_if")
    gw, gw_if = str(network.gateway), network.gateway_if
    router.link("add", ifname=gw_if, kind="dummy")
    idx = ifindex(router, gw_if)
    router.addr("add", index=idx, address=gw, prefixlen=HOST_PREFIX)
    router.link("set", index=idx, state="up")
    print(f"  {gw_if} addressed {gw}/{HOST_PREFIX} (gateway), set up")


def connect(router: NetNS, cont: NetNS, network: Network, container: str, att: Attachment) -> None:
    """Wire one container netns to its network's router with a /32 routed veth pair.

    vethR-<container> stays in the router; the container iface (att.interface) is
    born directly in the container netns (peer net_ns_fd from the cont handle, so
    it can't drift). Container side: /32 address, an explicit link-scope route to
    the gateway (nothing is on-link under /32), then default via it. Router side:
    the single /32 device route that is both the forwarding entry and rp_filter's
    reverse-path anchor. Both netns are recreated clean by `up`, so this just
    builds -- no existence checks."""
    rif, cif = router_if(container), att.interface
    ip, gw = str(att.ip), str(network.gateway)

    # Born in the right namespaces in one shot: peer (cif) directly in cont.
    router.link(
        "add", ifname=rif, kind="veth",
        peer={"ifname": cif, "net_ns_fd": cont.status["netns"]},
    )
    ridx = ifindex(router, rif)
    router.link("set", index=ridx, state="up")
    # THE mapping: reach-this-container AND legit-source-on-this-veth, one route.
    router.route("add", dst=f"{ip}/{HOST_PREFIX}", oif=ridx)

    # Container end: index unknowable in advance, look it up via the cont handle
    # (link('add') returns only once the kernel made both ends, so it's there).
    cidx = ifindex(cont, cif)
    cont.addr("add", index=cidx, address=ip, prefixlen=HOST_PREFIX)
    cont.link("set", index=cidx, state="up")
    # /32 => gateway is not on-link; pin it link-scope, then default via it.
    cont.route("add", dst=gw, oif=cidx, scope="link")
    cont.route("add", dst="default", gateway=gw, oif=cidx)
    print(
        f"  wired {container}: {cif} {ip}/{HOST_PREFIX} -> gw {gw} <-> {rif} "
        f"(route {ip}/{HOST_PREFIX} dev {rif})"
    )


# --- dataplane: sysctls + the nft flow matrix (per router netns) ------------


def router_sysctls(network: Network) -> dict[str, str]:
    """The sysctl set for a network's router netns.

    ip_forward on (we route); all.rp_filter=0 so the per-veth values are
    authoritative (kernel uses max(conf.all, conf.<if>), and a fresh netns may not
    default all to 0); ipv6 disabled router-wide (the routed model has no L2 path
    between containers, so killing v6 on the router severs inter-container v6);
    then per fabric veth: proxy_arp=1 (answer the gateway ARP / a future uplink)
    and rp_filter=1 (STRICT -- the anti-spoof pin, paired with that veth's /32
    route). Applied after wiring, so the per-veth conf.* dirs exist."""
    sysctls = {
        "net.ipv4.ip_forward": "1",
        "net.ipv4.conf.all.rp_filter": "0",
        "net.ipv6.conf.all.disable_ipv6": "1",
        "net.ipv6.conf.default.disable_ipv6": "1",
    }
    for container in network.attach:
        rif = router_if(container)
        sysctls[f"net.ipv4.conf.{rif}.proxy_arp"] = "1"
        sysctls[f"net.ipv4.conf.{rif}.rp_filter"] = "1"
    return sysctls


def build_nft(network: Network) -> nft.Ruleset:
    """The `inet turnip` ruleset for one router netns: the forward flow matrix.

    flush-and-reload (Table.reload) so re-applying replaces the table atomically.
    Maps precede the rules that use them. The forward chain (policy drop) accepts:
    established/related (conntrack return path -- so flows are one-way in the maps);
    drops invalid; then for new conns -- ICMP only between gateway-authorized pairs,
    any-port gateway pairs (allowed_hosts), and service-scoped pairs (allowed_flows;
    `th dport` covers tcp AND udp). Else policy drop."""
    gw = str(network.gateway)
    ip = {container: str(att.ip) for container, att in network.attach.items()}

    # allowed_flows: one entry per flow, DIRECTIONAL -- `from` may initiate to
    # `to` on (proto, port), and that is all; the return path rides ct
    # established/related, so no reverse entry. (icmp / port="any" in flows need a
    # different map shape -- deferred; the baseline carries concrete ports.)
    flow_elem: list[tuple[nft.Expr, nft.Expr]] = []
    for flow in network.flows:
        if flow.proto is Proto.ICMP or flow.port == "any":
            raise NotImplementedError("icmp / port='any' in flows is not wired yet")
        key = nft.concat(ip[flow.from_], ip[flow.to], flow.proto.value, flow.port)
        flow_elem.append((key, nft.accept()))

    # allowed_hosts: every attached container <-> the gateway, both directions.
    host_elem: list[tuple[nft.Expr, nft.Expr]] = []
    for cip in ip.values():
        host_elem.append((nft.concat(cip, gw), nft.accept()))
        host_elem.append((nft.concat(gw, cip), nft.accept()))

    sd = [nft.payload("ip", "saddr"), nft.payload("ip", "daddr")]  # shared saddr.daddr key
    table = nft.Table("inet", NFT_TABLE)
    return nft.ruleset(
        [
            *table.reload(),
            table.chain("forward", type="filter", hook="forward", prio=0, policy="drop"),
            table.verdict_map("allowed_flows", FLOW_KEY, flow_elem),
            table.verdict_map("allowed_hosts", HOST_KEY, host_elem),
            table.rule("forward", nft.ct_state("established", "related"), nft.accept()),
            table.rule("forward", nft.ct_state("invalid"), nft.drop()),
            table.rule(
                "forward",
                nft.ct_state("new"),
                nft.match(nft.meta("l4proto"), "icmp"),
                nft.vmap(nft.concat(*sd), "allowed_hosts"),
            ),
            table.rule("forward", nft.ct_state("new"), nft.vmap(nft.concat(*sd), "allowed_hosts")),
            table.rule(
                "forward",
                nft.ct_state("new"),
                nft.vmap(
                    nft.concat(*sd, nft.meta("l4proto"), nft.payload("th", "dport")),
                    "allowed_flows",
                ),
            ),
        ]
    )


def configure_dataplane(net_name: str, router_path: str, network: Network) -> None:
    """One forked hop into a router netns: write its sysctls, then load its nft
    table. Both need the fabric veths to already exist (per-veth conf.* dirs, and
    rp_filter's reverse-path lookup), so `up` calls this after all wiring."""
    sysctls = router_sysctls(network)
    rules = build_nft(network)

    def apply() -> str:
        write_sysctls(sysctls)
        nft.load(rules)
        return ""

    run_in_netns(router_path, apply)
    print(
        f"  {net_name} dataplane: ip_forward + per-veth proxy_arp/rp_filter + "
        f"ipv6 off + nft '{NFT_TABLE}'"
    )


# --- commands --------------------------------------------------------------


def up(turnip: Turnip, netns_dir: Path) -> None:
    """(Re)create the netns the config implies, wire each network (gateway + a /32
    routed veth per attached container), then apply each router's dataplane (sysctls
    + the nft flow matrix).

    Clean slate: tear everything down first (up = down() + build), so `up` always
    converges to the current config and shares ONE teardown path with the `down`
    command -- which grows host-side rootful state in milestone 4 that up's
    clean-slate must also clear.

    Order: wire ALL networks first, then the dataplane pass -- the per-veth sysctls
    (proxy_arp/rp_filter) and rp_filter's reverse-path lookup need the veths to
    exist, and the dataplane runs via a forked setns hop (run_in_netns), so it goes
    after the netlink sockets are closed."""
    # TODO: refuse when a running container is attached to a target netns -- the
    #       teardown below would orphan it (it keeps the old ns while a fresh one
    #       takes the path). For now assume containers are down (the systemd unit
    #       orders them before this).
    down(turnip, netns_dir)
    paths = {name: str(netns_dir / name) for name in netns_names(turnip)}
    for path in paths.values():  # fresh ns; sockets can only open into an existing one
        create_netns(path)
    with open_namespaces(paths) as ns:  # one socket per netns, reused below
        for handle in ns.values():
            set_lo_up(handle)
        for net_name, network in turnip.networks.items():
            if network.type is not NetworkType.ROUTER:
                raise NotImplementedError(
                    f"network {net_name!r}: only router networks are wired so far"
                )
            router = ns[router_netns(net_name)]
            create_gateway(router, network)
            for cname, att in network.attach.items():
                connect(router, ns[container_netns(cname)], network, cname, att)

    # sockets closed; now the in-netns dataplane (sysctls + nft) per router netns
    for net_name, network in turnip.networks.items():
        configure_dataplane(net_name, paths[router_netns(net_name)], network)


def down(turnip: Turnip, netns_dir: Path) -> None:
    """Remove every netns the config implies. Removing a netns destroys whatever
    lives inside it (veths, routes, and -- once milestone 3 lands -- the nft
    table), so this stays the whole teardown."""
    for name in netns_names(turnip):
        remove_netns(str(netns_dir / name))


def main() -> None:
    cmd = sys.argv[1] if len(sys.argv) > 1 else "up"
    fn = {"up": up, "down": down}.get(cmd)
    if fn is None:
        sys.exit(f"usage: {sys.argv[0]} {{up|down}}")

    # Resolve everything env-dependent in the PARENT (env + passwd db intact), then
    # run the command inside podman's namespaces, closing over the resolved values
    # -- the forked child inherits the closure, no module global needed.
    turnip = load_config()
    runtime = resolve_runtime(turnip.runtime)
    in_podman_context(lambda: fn(turnip, runtime.netns_dir))


if __name__ == "__main__":
    main()
