#!/usr/bin/env python3
"""
main.py -- CLI + orchestration, driven by the declarative config (turnip.json).

This is the imperative shell: it does the IO (read the config file + environment,
resolve the runtime), builds a small runtime *model* from the config, and operates
the pure modules (`config` = model + validation, `netns` = namespace ops) over it.

The runtime model -- `Container` / `Network` / `Endpoint` -- is plain dataclasses
built from the config (`build_model`). Each netns-owning node carries its derived
`netns_path` and, *while bound* (inside `with model.bound():`), a live `.netns` handle;
the wiring functions then operate over those objects instead of re-deriving names
and looking handles up in side maps. It is NOT a pure IR: with a single consumer
(`up`) there's nothing to re-slice, so the model just holds what we operate on.
Two things deliberately stay off the model: the live handle is valid only inside
the bind scope (nulled on exit), and ifindexes are never stashed (they go stale
when a link is recreated -- see netns.find_ifindex), so they remain locals.

It grows one milestone at a time (the old literal-driven version is kept as
`main.py.bak` for reference -- see IMPLEMENTATION-PLAN.md):

    1. netns setup       create/remove a netns per container + per router
    2. netns linking     gateway + /32 veth pairs + routes
    3. nft application   the forward flow matrix per router netns
    4. uplinks           (the rootful host edge)
    5. links             (container host-netdev holes)

Run as your normal login user -- no `podman unshare` wrapper. main enters
podman's rootless user+mount namespaces in-process (netns.in_podman_context):

    uv run turnip up        # create + wire the namespaces the config implies
    uv run turnip down      # remove them
"""

import argparse
import contextlib
import json
import os
import pwd
import socket
import sys
from collections.abc import Callable, Generator
from dataclasses import dataclass, field
from pathlib import Path
from typing import Self

from pyroute2 import IPRoute, NetNS

from . import nftlib as nft
from .config import (
    HOST_PREFIX,
    LINK_PREFIX,
    Egress,
    Flow,
    IngressRule,
    IpvlanLink,
    IpvlanMode,
    Link,
    MacvlanLink,
    NetworkType,
    PhysLink,
    Proto,
    ResolvedRuntime,
    Runtime,
    Turnip,
    VethLink,
)
from .netns import (
    collect_fds_from_child,
    create_netns,
    enter_podman,
    find_ifindex,
    ifindex,
    in_podman_context,
    remove_netns,
    run_in_netns,
    run_in_netns_fd,
    set_lo_up,
    write_sysctls,
)

IFNAMSIZ = 15  # kernel cap on an interface name; a derived veth name must fit
NFT_TABLE = "turnip"  # the per-router-netns nft table (one per netns; constant name)

# Map key type of the allowed_flows verdict map (its `type ...`).
FLOW_KEY = ["ipv4_addr", "ipv4_addr", "inet_proto", "inet_service"]


# --- IO: config discovery + runtime resolution (kept here, out of the modules) ---


def load_config(path: Path | None = None) -> Turnip:
    """Read + validate the config. Discovery: an explicit `path` (--config), else
    $TURNIP_CONFIG, else ./turnip.json. The file/env reads live here; `config` only
    validates the parsed data."""
    path = path or Path(os.environ.get("TURNIP_CONFIG", "turnip.json"))
    return Turnip.model_validate(json.loads(path.read_text()))


def resolve_runtime(rt: Runtime) -> ResolvedRuntime:
    """Fill in `Runtime`'s environment-dependent defaults, resolved by the TARGET
    user so it stays correct under sudo.

    User: explicit `runtime.user`, else `$SUDO_USER`, else -- ONLY when unprivileged
    -- the current login user. Running privileged (euid 0, i.e. under sudo) the
    current user is root, which is never the rootless-podman owner, so we refuse the
    current-user fallback: an explicit `runtime.user`/`$SUDO_USER` is required, and it
    must be non-root. (Capability-based privilege -- running as the user with
    CAP_NET_ADMIN instead of root -- is deferred; see todo.md. For now privileged
    means euid 0.)

    Dirs follow the target UID (`/run/user/<uid>/turnip`), NOT `$XDG_RUNTIME_DIR`,
    which under sudo is root's; for the rootless user the two coincide. The netns
    can't outlive a reboot and hosts files are regenerated each `up`, so the user's
    runtime tmpfs is the right home."""
    if os.geteuid() == 0:
        user = rt.user or os.environ.get("SUDO_USER")
        if not user:
            raise ValueError(
                "running as root: set runtime.user (the rootless-podman owner), "
                "or invoke via sudo so $SUDO_USER is set"
            )
    else:
        user = rt.user or os.environ.get("SUDO_USER") or pwd.getpwuid(os.getuid()).pw_name
    pw = pwd.getpwnam(user)  # raises KeyError for an unknown user -- fail closed
    if pw.pw_uid == 0:
        raise ValueError(
            f"runtime.user {user!r} resolves to root; it must be the unprivileged owner"
        )
    return ResolvedRuntime(
        user=user,
        uid=pw.pw_uid,
        gid=pw.pw_gid,
        home=Path(pw.pw_dir),
        state_dir=rt.state_dir or Path(f"/run/user/{pw.pw_uid}") / "turnip",
        nft=rt.nft,
        podman=rt.podman,
    )


# --- the runtime model: objects we operate over ----------------------------
# Built from config by build_model(); each netns-owning node carries its path and,
# while bound, its live handle. `handle` is the non-optional accessor (raises if
# used outside a bind scope -- catches use-after-close instead of poking a closed
# socket). netns live in two symmetric subdirs so a router can't collide with a
# container; the container leaf is the name verbatim (podman joins it by path).


@dataclass
class HostLink:
    """A lowered container link: the config `spec` plus the two facts build_model
    derives, so the wiring reads them and never re-derives (parallels Endpoint).

    - `default`: effective default-route ownership -- configured `default`, OR the
      container's sole interface (config only *requires* an explicit default when a
      container has >1 interface, so a single-homed container's lone link implicitly
      owns it).
    - `host_if`: the host-side veth end's derived name, for veth links only; None for
      macvlan/ipvlan/phys, which move/create a device directly with no host-side veth.
    """

    spec: Link
    default: bool
    host_if: str | None = None


@dataclass
class Container:
    """A container's on-disk state: its netns and its generated hosts file, both
    under `<state_dir>/containers/<name>/`, plus its `links` -- host-netdev holes
    moved into its netns (milestone 5), lowered to HostLink."""

    name: str
    netns_path: str  # containers/<name>/netns (the bind-mount)
    hosts_path: str  # containers/<name>/hosts (bind-mounted to /etc/hosts)
    links: list[HostLink] = field(default_factory=list["HostLink"])
    netns: NetNS | None = None  # bound only inside `with model.bound():`

    @property
    def handle(self) -> NetNS:
        if self.netns is None:
            raise RuntimeError(f"container {self.name!r} netns is not bound")
        return self.netns

    @property
    def state_dir(self) -> str:
        """The per-container directory holding netns + hosts."""
        return str(Path(self.netns_path).parent)


@dataclass
class Endpoint:
    """A container's attachment to a network: the /32 routed veth between them."""

    container: Container
    router_if: str  # router-side veth name (vethR-<container>)
    cont_if: str  # container-side iface name (from attach.interface)
    ip: str  # the container's /32 on this network
    default: bool = False  # effective default-route owner (configured, or sole iface)
    egress: Egress = False  # outbound allowance out this network's uplink (bool | rules)
    # host_port -> container DNATs (published ports)
    ingress: list[IngressRule] = field(default_factory=list[IngressRule])


@dataclass
class Uplink:
    """A network's host edge: a point-to-point /31 veth between its router netns and
    the init (host) netns. The host end + masquerade/DNAT are wired by the privileged
    parent in phase 2; the router end + its default route live in the router netns.
    The two /31 addresses are derived from the config base: host = base, router =
    base+1, and the router default-routes via the host end."""

    host_if: str  # veth end in the init netns
    router_if: str  # veth end in the router netns
    host_ip: str  # /31 host (init) end -- the router's gateway out
    router_ip: str  # /31 router end
    nat: bool  # host-side masquerade (vs routed)


@dataclass
class Network:
    """A router netns: its gateway, the endpoints hung off it, and its flow policy."""

    name: str
    netns_path: str
    gateway: str
    gateway_if: str
    endpoints: list[Endpoint]
    flows: list[Flow]
    uplink: Uplink | None = None  # the host edge, if this network has one
    netns: NetNS | None = None  # bound only inside `with model.bound():`

    @property
    def handle(self) -> NetNS:
        if self.netns is None:
            raise RuntimeError(f"network {self.name!r} netns is not bound")
        return self.netns


@dataclass
class Model:
    """Owns the netns lifetime. Two lifetimes, deliberately separate:

    - the netns themselves (bind-mounts) are PERSISTENT -- they outlive the process
      so containers can attach by path -- so create()/teardown() manage them
      explicitly; they are NOT tied to the context manager.
    - the open NetNS sockets ARE scoped: `with model.bound():` opens one per node
      and binds it to `.netns`, closing them and clearing `.netns` on exit. So the
      bound() block cleans up the sockets (the with-scoped resource), never the
      persistent netns.

    bound() is a method (not a free function) to sit with create()/teardown(): all
    three are netns resource-lifecycle, so they live together on the owner. The
    dataplane *policy* (gateway/veths/nft) stays free functions over the objects.
    """

    containers: list[Container]
    networks: list[Network]

    @property
    def nodes(self) -> list[Container | Network]:
        """Every netns-owning node (containers + routers) -- the create/remove/bind
        unit. Containers first, then routers."""
        return [*self.containers, *self.networks]

    def create(self) -> None:
        """Create a fresh netns for every node. Clean slate -- callers teardown()
        first (a node's netns must not already exist)."""
        for node in self.nodes:
            create_netns(node.netns_path)

    def teardown(self) -> None:
        """Remove every node's netns (which destroys whatever lives inside it --
        veths, routes, the nft table) and each container's hosts file. The whole
        teardown.

        The container state dir itself is LEFT: rmdir'ing it now hits EBUSY (the
        netns inside is lazily unmounted -- MNT_DETACH -- so the dir stays busy
        while the detach completes), and create()'s makedirs(exist_ok) reuses it
        next time anyway. So `down` may leave empty containers/<name>/ dirs."""
        for node in self.nodes:
            remove_netns(node.netns_path)
        for container in self.containers:
            with contextlib.suppress(FileNotFoundError):
                os.unlink(container.hosts_path)

    @contextlib.contextmanager
    def bound(self) -> Generator[Self]:
        """Open a NetNS socket per node and bind it to `.netns` for the block,
        closing + clearing on exit (including a partial open -- the ExitStack
        unwinds, the finally clears). flags=0 so opening a missing netns errors
        (every one must already exist via create()). Outside the block `.netns` is
        None, so `.handle` fails loudly instead of poking a closed socket. A local
        stack, so it's reentrant-safe and holds no lifecycle state on the model."""
        with contextlib.ExitStack() as stack:
            for node in self.nodes:
                node.netns = stack.enter_context(NetNS(node.netns_path, flags=0))
            try:
                yield self
            finally:
                for node in self.nodes:
                    node.netns = None


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


def link_host_if(container: str, link: VethLink) -> str:
    """Host-side veth name for a container's veth link. Like router_if it only needs
    to be unique in the init netns, so it keys on (container, link name) -- a container
    may have several veth links. Rejects an over-long name rather than letting the
    kernel truncate it into a silent collision; the general hashing scheme is deferred
    with the multi-network veth-name work."""
    name = f"vethL-{container}-{link.name}"
    if len(name) > IFNAMSIZ:
        raise ValueError(
            f"link host veth name {name!r} exceeds IFNAMSIZ ({IFNAMSIZ}); "
            f"shorten {container!r}/{link.name!r}"
        )
    return name


def build_model(turnip: Turnip, state_dir: Path) -> Model:
    """Lower the validated config into the runtime model (no IO, no handles yet).

    Containers come from the top-level `containers` map (the authoritative set -- a
    links-only container with no attachment still gets its netns); each network's
    endpoints reference those shared container objects (so a multi-homed container
    is one netns with N endpoints).

    Effective default-route ownership is resolved here once: an interface owns the
    container's default route if it is configured `default`, OR it is the container's
    sole interface (links + attachments). config's _cross_cutting guarantees at most
    one configured default and forbids an undefaulted multi-interface container, so
    this lowers cleanly to exactly one owner per container -- stored on Endpoint /
    HostLink so connect()/link_connect() stay dumb."""
    iface_total = {name: len(c.links) for name, c in turnip.containers.items()}
    for net in turnip.networks.values():
        for cname in net.attach:
            iface_total[cname] += 1

    containers = {
        name: Container(
            name,
            netns_path=str(state_dir / "containers" / name / "netns"),
            hosts_path=str(state_dir / "containers" / name / "hosts"),
            links=[
                HostLink(
                    spec=link,
                    default=link.default or iface_total[name] == 1,
                    host_if=link_host_if(name, link) if isinstance(link, VethLink) else None,
                )
                for link in c.links
            ],
        )
        for name, c in turnip.containers.items()
    }
    networks: list[Network] = []
    for net_name, net in turnip.networks.items():
        if net.type is not NetworkType.ROUTER:
            raise NotImplementedError(
                f"network {net_name!r}: only router networks are wired so far"
            )
        if net.gateway_if is None:  # validator guarantees this for a router network
            raise ValueError(f"router network {net_name!r} has no gateway_if")
        endpoints = [
            Endpoint(
                containers[cname],
                router_if(cname),
                att.interface,
                str(att.ip),
                default=att.default or iface_total[cname] == 1,
                egress=att.egress,
                ingress=att.ingress,
            )
            for cname, att in net.attach.items()
        ]
        uplink = None
        if net.uplink is not None:
            base = net.uplink.link  # the even /31 base (validated in config)
            uplink = Uplink(
                host_if=net.uplink.host_if,
                router_if=net.uplink.router_if,
                host_ip=str(base),  # host (init) end = base; the router's gateway out
                router_ip=str(base + 1),  # router end = base+1
                nat=net.uplink.nat,
            )
        networks.append(
            Network(
                name=net_name,
                netns_path=str(state_dir / "routers" / net_name),
                gateway=str(net.gateway),
                gateway_if=net.gateway_if,
                endpoints=endpoints,
                flows=list(net.flows),
                uplink=uplink,
            )
        )
    return Model(list(containers.values()), networks)


# --- wiring (free functions over the model objects) ------------------------


def create_gateway(network: Network) -> None:
    """Create + address + bring up the dummy gateway in a (fresh) router netns.

    A `dummy` holding <gateway>/32: a real local address, so the normal ARP
    responder answers containers' gateway ARP without an uplink. The router netns
    is recreated clean by `up`, so this just builds -- no existence checks."""
    router = network.handle
    router.link("add", ifname=network.gateway_if, kind="dummy")
    idx = ifindex(router, network.gateway_if)
    router.addr("add", index=idx, address=network.gateway, prefixlen=HOST_PREFIX)
    router.link("set", index=idx, state="up")
    print(f"  {network.gateway_if} addressed {network.gateway}/{HOST_PREFIX} (gateway), set up")


def connect(network: Network, ep: Endpoint) -> None:
    """Wire one endpoint's container netns to its router with a /32 routed veth.

    vethR-<container> stays in the router; the container iface is born directly in
    the container netns (peer net_ns_fd from the cont handle, so it can't drift).
    Container side: /32 address, an explicit link-scope route to the gateway
    (nothing is on-link under /32), then -- only if this endpoint owns the container's
    default route (ep.default) -- default via it. A container whose default lives on
    another interface (another network, or a `default` link) gets gateway reachability
    here but no 0.0.0.0/0. Router side: the single /32 device route that is both the
    forwarding entry and rp_filter's reverse-path anchor. Both netns are recreated
    clean by `up`, so this just builds."""
    router, cont = network.handle, ep.container.handle

    # Born in the right namespaces in one shot: peer (cont_if) directly in cont.
    router.link(
        "add", ifname=ep.router_if, kind="veth",
        peer={"ifname": ep.cont_if, "net_ns_fd": cont.status["netns"]},
    )
    ridx = ifindex(router, ep.router_if)
    router.link("set", index=ridx, state="up")
    # THE mapping: reach-this-container AND legit-source-on-this-veth, one route.
    router.route("add", dst=f"{ep.ip}/{HOST_PREFIX}", oif=ridx)

    # Container end: index unknowable in advance, look it up via the cont handle
    # (link('add') returns only once the kernel made both ends, so it's there).
    cidx = ifindex(cont, ep.cont_if)
    cont.addr("add", index=cidx, address=ep.ip, prefixlen=HOST_PREFIX)
    cont.link("set", index=cidx, state="up")
    # /32 => gateway is not on-link; pin it link-scope, then default via it iff this
    # endpoint owns the container's default route.
    cont.route("add", dst=network.gateway, oif=cidx, scope="link")
    if ep.default:
        cont.route("add", dst="default", gateway=network.gateway, oif=cidx)
    print(
        f"  wired {ep.container.name}: {ep.cont_if} {ep.ip}/{HOST_PREFIX} -> gw "
        f"{network.gateway}{' (default)' if ep.default else ''} <-> {ep.router_if} "
        f"(route {ep.ip}/{HOST_PREFIX} dev {ep.router_if})"
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
    for ep in network.endpoints:
        sysctls[f"net.ipv4.conf.{ep.router_if}.proxy_arp"] = "1"
        sysctls[f"net.ipv4.conf.{ep.router_if}.rp_filter"] = "1"
    if network.uplink is not None:
        # strict rp_filter on the uplink too: the reverse path for an internet source
        # is the default route = the uplink, while a container-spoofed source resolves
        # to its own /32 veth (not the uplink) and is dropped -- the anti-spoof pin.
        sysctls[f"net.ipv4.conf.{network.uplink.router_if}.rp_filter"] = "1"
    return sysctls


def build_nft(network: Network) -> nft.Ruleset:
    """The `inet turnip` ruleset for one router netns: the forward flow matrix.

    flush-and-reload (Table.reload) so re-applying replaces the table atomically. The
    forward chain (policy drop) accepts: established/related (the conntrack return path
    -- so flows are one-way in the map); drops invalid; then for new conns the
    service-scoped intra-network flows (allowed_flows; `th dport` covers tcp AND udp)
    and the host-edge egress/ingress allows. Else policy drop.

    A second base chain, INPUT (policy drop), locks down the router's OWN address (the
    gateway + the uplink end -- container<->gateway traffic is INPUT/OUTPUT, not
    forwarded): it accepts loopback, the conntrack return, and icmp (the gateway ping),
    dropping tcp/udp so no router-local service is exposed without a deliberate allow.
    OUTPUT stays default-accept -- the router originates nothing untrusted."""
    ip = {ep.container.name: ep.ip for ep in network.endpoints}

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

    sd = [nft.payload("ip", "saddr"), nft.payload("ip", "daddr")]  # shared saddr.daddr key
    table = nft.Table("inet", NFT_TABLE)

    # The host-edge allow rules (only meaningful with an uplink), appended after the
    # intra-network vmaps, before the policy drop. The return path always rides ct
    # established/related, so these are one-directional.
    #   egress  -- a container may INITIATE out the uplink (oif = uplink, saddr =
    #              container). `True` = any; a list = scoped to (proto, port).
    #   ingress -- post-DNAT, allow IN the uplink to a published container port (iif =
    #              uplink, daddr = container, the CONTAINER port). Keyed on dest, since
    #              the client source is a wildcard after the host's DNAT.
    edge_rules: list[nft.Add] = []
    if network.uplink is not None:
        uplink_if = network.uplink.router_if
        for ep in network.endpoints:
            if ep.egress:
                scope = (
                    nft.ct_state("new"),
                    nft.match(nft.meta("oifname"), uplink_if),
                    nft.match(nft.payload("ip", "saddr"), ep.ip),
                )
                if ep.egress is True:
                    edge_rules.append(table.rule("forward", *scope, nft.accept()))
                else:
                    for er in ep.egress:
                        for proto in er.proto:
                            exprs = [*scope, nft.match(nft.meta("l4proto"), proto.value)]
                            if proto is not Proto.ICMP and er.port not in (None, "any"):
                                exprs.append(nft.match(nft.payload("th", "dport"), er.port))
                            edge_rules.append(table.rule("forward", *exprs, nft.accept()))
            for ing in ep.ingress:
                edge_rules.append(
                    table.rule(
                        "forward",
                        nft.ct_state("new"),
                        nft.match(nft.meta("iifname"), uplink_if),
                        nft.match(nft.payload("ip", "daddr"), ep.ip),
                        nft.match(nft.meta("l4proto"), ing.proto.value),
                        nft.match(nft.payload("th", "dport"), ing.port or ing.host_port),
                        nft.accept(),
                    )
                )

    return nft.ruleset(
        [
            *table.reload(),
            table.chain("forward", type="filter", hook="forward", prio=0, policy="drop"),
            table.chain("input", type="filter", hook="input", prio=0, policy="drop"),
            table.verdict_map("allowed_flows", FLOW_KEY, flow_elem),
            # forward: the intra-network flow matrix + the host-edge egress/ingress allows
            table.rule("forward", nft.ct_state("established", "related"), nft.accept()),
            table.rule("forward", nft.ct_state("invalid"), nft.drop()),
            table.rule(
                "forward",
                nft.ct_state("new"),
                nft.vmap(
                    nft.concat(*sd, nft.meta("l4proto"), nft.payload("th", "dport")),
                    "allowed_flows",
                ),
            ),
            *edge_rules,
            # input: the router's OWN address (gateway, uplink end) is default-deny.
            # Accept loopback, the conntrack return path, and icmp (the gateway ping +
            # /31 diagnostics); tcp/udp fall to the policy drop, so no router-local
            # service is reachable from containers without a deliberate allow.
            table.rule("input", nft.match(nft.meta("iifname"), "lo"), nft.accept()),
            table.rule("input", nft.ct_state("established", "related"), nft.accept()),
            table.rule("input", nft.match(nft.meta("l4proto"), "icmp"), nft.accept()),
        ]
    )


def _apply_router_dataplane(network: Network, run: Callable[[Callable[[], str]], str]) -> None:
    """Write a router netns's sysctls + load its nft table, entering the netns via
    `run` (a run_in_netns / run_in_netns_fd hop). Both need the veths to already exist
    (per-veth conf.* dirs, rp_filter's reverse-path lookup) -- and for an uplinked
    network, the uplink veth too, which is why that case runs in phase 2."""
    sysctls = router_sysctls(network)
    rules = build_nft(network)

    def apply() -> str:
        write_sysctls(sysctls)
        nft.load(rules)
        return ""

    run(apply)
    print(
        f"  {network.name} dataplane: ip_forward + per-veth proxy_arp/rp_filter + "
        f"ipv6 off + nft '{NFT_TABLE}'"
    )


def configure_dataplane(network: Network) -> None:
    """Apply a router's dataplane from WITHIN podman's userns (phase 1), entering the
    netns by PATH. Used for networks without an uplink: entering a netns (setns
    CLONE_NEWNET) needs CAP_SYS_ADMIN in the CALLER's own userns, which the rootless
    user holds only inside podman's userns (owner-match grants caps in the TARGET
    userns, not the caller's)."""
    _apply_router_dataplane(network, lambda apply: run_in_netns(network.netns_path, apply))


def apply_dataplane_fd(network: Network, router_fd: int) -> None:
    """Apply a router's dataplane from the init-side parent (phase 2), entering the
    netns by FD. Used for UPLINKED networks: their dataplane (egress rules + the uplink
    veth's rp_filter) needs the uplink veth, created host-side in phase 2 -- and root
    *does* hold CAP_SYS_ADMIN in the init userns, so it may setns by fd."""
    _apply_router_dataplane(network, lambda apply: run_in_netns_fd(router_fd, apply))


def build_host_nft(network: Network) -> nft.Ruleset:
    """The host-side nat zone for a network's uplink, in the init netns: one
    `ip turnip_host_<net>` table. Postrouting masquerades traffic forwarded IN from the
    uplink (egress SNAT; iif-matched -- the routed /32 model declares no subnet).
    Prerouting DNATs published host ports to container:port (ingress / port forwards),
    added only when some attachment has ingress. The egress (iif uplink) vs ingress
    (iif WAN) split means masquerade fires only on egress, DNAT only on ingress, and
    each connection's NAT is decided on its first packet -- they don't collide."""
    up = network.uplink
    assert up is not None  # only called for uplinked networks
    table = nft.Table("ip", f"turnip_host_{network.name}")

    dnat_rules: list[nft.Add] = []
    for ep in network.endpoints:
        for ing in ep.ingress:
            exprs: list[nft.Expr] = []
            if str(ing.listen) != "0.0.0.0":  # default 0.0.0.0 = any host address
                exprs.append(nft.match(nft.payload("ip", "daddr"), str(ing.listen)))
            exprs += [
                nft.match(nft.meta("l4proto"), ing.proto.value),
                nft.match(nft.payload("th", "dport"), ing.host_port),
                nft.dnat(ep.ip, ing.port or ing.host_port),  # port defaults to host_port
            ]
            dnat_rules.append(table.rule("prerouting", *exprs))

    cmds: list[nft.Command] = [
        *table.reload(),
        table.chain("postrouting", type="nat", hook="postrouting", prio=100, policy="accept"),
        table.rule("postrouting", nft.match(nft.meta("iifname"), up.host_if), nft.masquerade()),
    ]
    if dnat_rules:
        cmds.append(
            table.chain("prerouting", type="nat", hook="prerouting", prio=-100, policy="accept")
        )
        cmds.extend(dnat_rules)
    return nft.ruleset(cmds)


def configure_host_nat(network: Network) -> None:
    """ip_forward + the host masquerade zone + host routes to the containers, in the
    init netns (phase 2, root parent). Runs directly (the parent IS in the init netns)
    -- no setns hop. The /32 routes (container via the router end) let the host forward
    TO containers (ingress/DNAT) and satisfy rp_filter for egress -- the reverse path to
    a container source resolves back out the uplink. They die with the host veth on
    teardown. ip_forward is a host-global capability we set on (not turnip per-run
    state), so `down` leaves it; the masquerade table is flushed."""
    up = network.uplink
    assert up is not None
    write_sysctls({"net.ipv4.ip_forward": "1"})
    nft.load(build_host_nft(network))
    with IPRoute() as ipr:
        oif = ifindex(ipr, up.host_if)
        for ep in network.endpoints:
            ipr.route("add", dst=f"{ep.ip}/{HOST_PREFIX}", gateway=up.router_ip, oif=oif)
    print(
        f"  {network.name} host nat: ip_forward + masquerade iif {up.host_if} + "
        f"{len(network.endpoints)} container route(s) via {up.router_ip}"
    )


# --- per-container hosts files (a projection over the model) ---------------
# The reverse view the object graph is built for: container -> its endpoints ->
# networks -> flows -> peer IPs. Computed on demand (not stored), so the wiring
# graph stays forward-only; if more consumers want the reverse, add back-refs.


def container_peers(model: Model, container: Container) -> dict[str, str]:
    """`{name: ip}` for the peers `container` may *initiate* to -- the targets of
    its outbound flows (flows are directional, so `from == container`). Resolved
    per network via that network's name->ip map."""
    peers: dict[str, str] = {}
    for net in model.networks:
        ips = {ep.container.name: ep.ip for ep in net.endpoints}
        if container.name not in ips:  # the reverse filter: is the container here?
            continue
        for flow in net.flows:
            if flow.from_ == container.name:
                peers[flow.to] = ips[flow.to]
    return peers


def hosts_file(model: Model, container: Container) -> str:
    """The /etc/hosts body for a container: localhost, the container's own name on
    each network it's attached to (so it resolves itself -- the bind-mount replaces
    podman's generated file), then the peers it may reach by name."""
    own = [
        (ep.ip, container.name)
        for net in model.networks
        for ep in net.endpoints
        if ep.container is container  # identity: the shared object the graph gives us
    ]
    lines = ["127.0.0.1 localhost"]
    lines += [f"{ip} {name}" for ip, name in own]
    lines += [f"{ip} {name}" for name, ip in container_peers(model, container).items()]
    return "\n".join(lines) + "\n"


def write_hosts(model: Model) -> None:
    """Write each container's hosts file into its state dir (created by
    model.create()); run-container.sh bind-mounts it to /etc/hosts."""
    for container in model.containers:
        Path(container.hosts_path).write_text(hosts_file(model, container))
        print(f"  wrote hosts: {container.hosts_path}")


# --- commands --------------------------------------------------------------
# Phase 1 ships netns fds up to the init parent keyed BY TYPE ("router:<net>" /
# "container:<name>") so a network and a container that happen to share a name can't
# collide -- they live in symmetric subdirs precisely because they can. These two
# accessors keep the prefix scheme in one place.


def router_fd(fds: dict[str, int], net: Network) -> int:
    return fds[f"router:{net.name}"]


def container_fd(fds: dict[str, int], container: Container) -> int:
    return fds[f"container:{container.name}"]


def wire_in_podman(model: Model, runtime: ResolvedRuntime) -> dict[str, int]:
    """Phase 1, run in a forked child: become the rootless user, enter podman's ns,
    rebuild the netns clean-slate, wire each network (gateway + /32 routed veths + hosts
    files), and apply each router's dataplane (sysctls + nft) -- all the netns work,
    done from inside podman's userns where the rootless user holds CAP_SYS_ADMIN. Then
    open and RETURN the netns fds the init parent's phase 2 needs: a "router:<net>" fd
    per network (for the uplink host edge), and a "container:<name>" fd per LINKED
    container (for the link host edge -- rootless containers need none). The netns
    persist by bind-mount, so the fds stay valid after this child exits.

    Clean slate: model.teardown() is the netns side; the host-edge side is cleared by
    the parent (teardown_host_edge)."""
    enter_podman(runtime)
    # TODO: refuse when a running container is attached to a target netns -- the
    #       teardown below would orphan it. For now assume containers are down.
    model.teardown()  # clean slate (netns side)
    model.create()
    write_hosts(model)  # per-container hosts files (config-derived; dirs now exist)
    with model.bound():  # open + bind a netns socket per node, closed on exit
        for node in model.nodes:
            set_lo_up(node.handle)
        for network in model.networks:
            create_gateway(network)
            for ep in network.endpoints:
                connect(network, ep)
    # sockets closed; dataplane (sysctls + nft) for NON-uplinked routers, here inside
    # podman's userns. Uplinked routers defer their dataplane to phase 2 (it needs the
    # uplink veth, which the parent creates), applied by the root parent over the fd.
    for network in model.networks:
        if network.uplink is None:
            configure_dataplane(network)
    fds = {f"router:{net.name}": os.open(net.netns_path, os.O_RDONLY) for net in model.networks}
    fds.update(
        {f"container:{c.name}": os.open(c.netns_path, os.O_RDONLY)
         for c in model.containers if c.links}
    )
    return fds


def teardown_host_edge(model: Model) -> None:
    """Remove host-side state in the init netns that does NOT die with the router/
    container netns: each network's uplink host veth end + host nat zone, AND each veth
    link's host-side end. Runs in the privileged parent, before re-wiring (up = down +
    build) and on `down`.

    The link host end is deleted by name, idempotently: its container-side peer dies
    when the container netns is torn down, but the init-side end may survive that (as
    the uplink host end does), so we delete it here -- and if the kernel already reaped
    it with its peer, find_ifindex is None and we skip. Deleting either end of a veth
    pair destroys both, which is fine: the container side is being rebuilt (up) or
    removed (down) anyway. A no-op without uplinks AND without links, so the rootless
    path never touches the init netns here."""
    uplinked = [(net.name, up) for net in model.networks if (up := net.uplink) is not None]
    link_host_ifs = [
        link.host_if for c in model.containers for link in c.links if link.host_if is not None
    ]
    if not uplinked and not link_host_ifs:
        return
    with IPRoute() as ipr:
        for _name, up in uplinked:
            idx = find_ifindex(ipr, up.host_if)
            if idx is not None:
                ipr.link("del", index=idx)
                print(f"  removed host uplink veth {up.host_if}")
        for host_if in link_host_ifs:
            idx = find_ifindex(ipr, host_if)
            if idx is not None:
                ipr.link("del", index=idx)
                print(f"  removed link host veth {host_if}")
    for name, _up in uplinked:
        table = nft.Table("ip", f"turnip_host_{name}")
        nft.load(nft.ruleset([nft.Add(table), nft.Delete(table)]))
        print(f"  flushed host nat zone turnip_host_{name}")


def host_edge_connect(network: Network, router_fd: int) -> None:
    """Wire a network's uplink veth across the init<->router boundary -- the host edge,
    run in the privileged init-side parent (phase 2). No-op without an uplink.

    The host end is born in the init netns and the router end DIRECTLY in the router
    netns (peer net_ns_fd -- IFLA_NET_NS_FD, which needs only CAP_NET_ADMIN, held in
    init and flowing down to podman's userns). Birthing the router end in place (rather
    than creating both in init then moving one) keeps the router-scoped name out of the
    init netns entirely, so it can't collide there. The host end is addressed on the /31
    init-side; the router end + its default route via the host end are set inside the
    router netns, entered by fd (setns, which root may do from the init userns). No
    nft/masquerade yet."""
    up = network.uplink
    if up is None:
        return
    # host end in init, router end born directly in the router netns via the fd
    with IPRoute() as ipr:
        ipr.link(
            "add",
            ifname=up.host_if,
            kind="veth",
            peer={"ifname": up.router_if, "net_ns_fd": router_fd},
        )
        hidx = ifindex(ipr, up.host_if)
        ipr.addr("add", index=hidx, address=up.host_ip, prefixlen=LINK_PREFIX)
        ipr.link("set", index=hidx, state="up")

    def configure_router_end() -> str:
        with IPRoute() as r:
            ridx = ifindex(r, up.router_if)
            r.addr("add", index=ridx, address=up.router_ip, prefixlen=LINK_PREFIX)
            r.link("set", index=ridx, state="up")
            r.route("add", dst="default", gateway=up.host_ip, oif=ridx)
        return ""

    run_in_netns_fd(router_fd, configure_router_end)
    print(
        f"  {network.name} uplink: {up.host_if} {up.host_ip} <-> {up.router_if} "
        f"{up.router_ip}/{LINK_PREFIX} (router default via {up.host_ip})"
    )


# IFLA_IPVLAN_MODE numeric values (linux/if_link.h). pyroute2 has a name map for
# macvlan modes but not ipvlan's, so we resolve ipvlan's ourselves.
_IPVLAN_MODE = {IpvlanMode.L2: 0, IpvlanMode.L3: 1, IpvlanMode.L3S: 2}


def link_connect(container: Container, link: HostLink, fd: int) -> None:
    """Wire one container link -- a host-netdev hole into the container's netns, OUTSIDE
    every router and its nft policy (the deliberate L2 trust escape). Runs in the
    privileged init-side parent (phase 2): the anchor lives in the init netns where root
    holds CAP_NET_ADMIN, and the container end is born directly in the container netns
    via IFLA_NET_NS_FD (the cap flows down into podman's userns -- the same cross-userns
    move the uplink veth proves, here targeting a container netns instead of a router).

    The host-side (init-netns) half differs by ownership -- own (create) vs borrow
    (move) -- implied by `type`:
    - veth->bridge: a veth pair, container end born in the container netns, host end
      enslaved to the named host bridge (a shared L2 segment with the host).
    - veth->host:   the same, but the host end is left bare in the init (root) netns for
      the operator to route to -- the point-to-point escape hatch. turnip adds NO host
      route; what the host does with its end is the host's call.
    - macvlan/ipvlan: born directly into the container netns off the host `parent` --
      `link=` (IFLA_LINK) resolves the parent in the INIT netns while `net_ns_fd` places
      the new device in the container netns (one shot, so the device never appears named
      in init). OWNED (virtual): reaped with the netns.
    - phys: the existing host device is MOVED into the container netns (set net_ns_fd on
      it). BORROWED: it returns to init when the netns is destroyed (kernel
      default_device_exit), so there is no teardown code -- and it arrives named after
      the host `dev`, so configure_iface renames it to the configured `name` first.

    The in-container half (rename-if-needed, address/mac/mtu/routes/default) is uniform
    and entered by fd."""
    spec = link.spec
    entry_name = spec.name  # the device's name on entry; phys keeps its host name (below)

    def configure_iface() -> str:
        # in-container half: address (real on-link subnet -- no /32 link-scope pin),
        # optional mac/mtu, up, static routes, and default-via-gateway iff this link
        # owns the container's default route.
        with IPRoute() as r:
            idx = ifindex(r, entry_name)
            if entry_name != spec.name:  # phys: arrives named after its host dev; rename
                r.link("set", index=idx, ifname=spec.name)  # (down post-move, so rename is ok)
            if spec.mac is not None:
                r.link("set", index=idx, address=spec.mac)
            if spec.mtu is not None:
                r.link("set", index=idx, mtu=spec.mtu)
            r.addr(
                "add", index=idx,
                address=str(spec.address.ip), prefixlen=spec.address.network.prefixlen,
            )
            r.link("set", index=idx, state="up")
            for route in spec.routes:
                r.route("add", dst=str(route), oif=idx)
            if link.default and spec.gateway is not None:
                r.route("add", dst="default", gateway=str(spec.gateway), oif=idx)
        return ""

    match spec:
        case VethLink():
            assert link.host_if is not None  # build_model derives it for veth links
            with IPRoute() as ipr:
                ipr.link(
                    "add", ifname=link.host_if, kind="veth",
                    peer={"ifname": spec.name, "net_ns_fd": fd},
                )
                hidx = ifindex(ipr, link.host_if)
                if spec.bridge is not None:  # veth->bridge: enslave the host end
                    ipr.link("set", index=hidx, master=ifindex(ipr, spec.bridge))
                ipr.link("set", index=hidx, state="up")
            anchor = f"bridge {spec.bridge}" if spec.bridge is not None else "host (root netns)"
        case MacvlanLink():
            with IPRoute() as ipr:
                ipr.link(
                    "add", ifname=spec.name, kind="macvlan",
                    link=ifindex(ipr, spec.parent),  # parent resolved in INIT netns
                    macvlan_mode=spec.mode.value,
                    net_ns_fd=fd,  # ... born into the container netns
                )
            anchor = f"macvlan on {spec.parent} ({spec.mode.value})"
        case IpvlanLink():
            with IPRoute() as ipr:
                ipr.link(
                    "add", ifname=spec.name, kind="ipvlan",
                    link=ifindex(ipr, spec.parent),
                    # pyroute2 maps macvlan mode NAMES but not ipvlan's, so IFLA_IPVLAN_MODE
                    # must be the numeric value (linux/if_link.h: l2=0, l3=1, l3s=2).
                    ipvlan_mode=_IPVLAN_MODE[spec.mode],
                    net_ns_fd=fd,
                )
            anchor = f"ipvlan on {spec.parent} ({spec.mode.value})"
        case PhysLink():
            entry_name = spec.dev  # moved in under its host name; configure_iface renames
            with IPRoute() as ipr:
                ipr.link("set", index=ifindex(ipr, spec.dev), net_ns_fd=fd)
            anchor = f"phys {spec.dev}"

    run_in_netns_fd(fd, configure_iface)
    print(
        f"  linked {container.name}: {spec.name} {spec.address} via {anchor}"
        f"{' (default)' if link.default else ''}"
    )


def _link_kind(ns: IPRoute, idx: int) -> str | None:
    """IFLA_INFO_KIND for a link ('bridge', 'veth', 'dummy', ...) -- the kernel's own
    type label. None for a device with no link-kind (a physical NIC carries no
    IFLA_LINKINFO), which the borrow/own doctrine reads as 'physical'."""
    info = ns.link("get", index=idx)[0].get_attr("IFLA_LINKINFO")
    return info.get_attr("IFLA_INFO_KIND") if info is not None else None


def _is_wireless(ifname: str) -> bool:
    """True if `ifname` is a wireless netdev (it exposes /sys/class/net/<if>/wireless).
    macvlan can't bridge onto a wireless parent -- the AP drops the extra MACs -- so the
    doctrine rejects it and points at ipvlan (single MAC) instead."""
    return os.path.exists(f"/sys/class/net/{ifname}/wireless")


def _default_route_oifs(ns: IPRoute) -> set[int]:
    """Oifs carrying an IPv4 default route in `ns` -- the host's primary NIC(s). Used to
    refuse moving the host's own uplink into a container (which would sever the host)."""
    return {
        oif
        for r in ns.route("dump", family=socket.AF_INET)
        if r["dst_len"] == 0 and (oif := r.get_attr("RTA_OIF")) is not None
    }


def validate_link_anchors(model: Model) -> None:
    """Validate each link's host-side anchor in the init netns BEFORE any netns is built,
    so a bad anchor fails fast (cheap, and the alternative is a confusing mid-wiring
    kernel error). Anchors are BORROWED -- we only check, never create. Per type:
    - veth->bridge: the named bridge exists and is kind=bridge.
    - veth->host:   no anchor (the root netns is always present).
    - macvlan/ipvlan: the `parent` exists; a macvlan parent must not be wireless.
    - phys: the `dev` exists and is NOT the host's primary (default-route) NIC -- moving
      that into a container would sever the host.
    A no-op when no container has links."""
    specs = [(c.name, link.spec) for c in model.containers for link in c.links]
    if not specs:
        return
    # macvlan and ipvlan cannot share a parent (kernel: a device is a macvlan master XOR
    # an ipvlan master) -- catch it as a clear error, not a raw EBUSY mid-wiring.
    flavor: dict[str, str] = {}
    for _cname, spec in specs:
        if isinstance(spec, MacvlanLink | IpvlanLink):
            if (prev := flavor.setdefault(spec.parent, spec.type.value)) != spec.type.value:
                raise ValueError(
                    f"parent {spec.parent!r}: macvlan and ipvlan cannot share a parent device "
                    f"({prev} vs {spec.type.value})"
                )
    with IPRoute() as ipr:
        default_oifs = _default_route_oifs(ipr)
        for cname, spec in specs:
            _validate_anchor(ipr, cname, spec, default_oifs)


def _validate_anchor(ipr: IPRoute, cname: str, spec: Link, default_oifs: set[int]) -> None:
    """Anchor check for one link (init netns); see validate_link_anchors. Borrowed
    anchors are validated, never created -- a bad one raises here, before any build."""
    match spec:
        case VethLink(bridge=str(bridge)):
            idx = find_ifindex(ipr, bridge)
            if idx is None:
                raise ValueError(f"{cname}: link bridge {bridge!r} not found in host netns")
            if (kind := _link_kind(ipr, idx)) != "bridge":
                raise ValueError(f"{cname}: link anchor {bridge!r} is kind {kind!r}, not a bridge")
        case VethLink():  # peer="host" -- the root netns, always present
            pass
        case MacvlanLink(parent=parent):
            if find_ifindex(ipr, parent) is None:
                raise ValueError(f"{cname}: macvlan parent {parent!r} not found in host netns")
            if _is_wireless(parent):
                raise ValueError(f"{cname}: macvlan parent {parent!r} is wireless; use ipvlan")
        case IpvlanLink(parent=parent):
            if find_ifindex(ipr, parent) is None:
                raise ValueError(f"{cname}: ipvlan parent {parent!r} not found in host netns")
        case PhysLink(dev=dev):
            idx = find_ifindex(ipr, dev)
            if idx is None:
                raise ValueError(f"{cname}: phys dev {dev!r} not found in host netns")
            if idx in default_oifs:
                raise ValueError(
                    f"{cname}: phys dev {dev!r} is the host's primary (default-route) "
                    f"NIC; refusing to move it into a container"
                )


def up(model: Model, runtime: ResolvedRuntime) -> None:
    """Two phases joined by the fd bridge. Phase 1 (a podman child, wire_in_podman) does
    all the netns work -- rebuild clean-slate, wire each network, apply the dataplane --
    inside podman's userns, and ships a netns fd per network (+ per linked container).
    Phase 2 (this init-side parent) connects each network's host edge (uplink veth) and
    each container's links over those fds; it needs only CAP_NET_ADMIN (the
    IFLA_NET_NS_FD move) which root holds in the init netns, and is a no-op for networks
    without an uplink / containers without links. Clean slate spans both halves (up =
    down + build): the host-edge teardown here + the netns teardown in phase 1."""
    teardown_host_edge(model)  # parent: clear prior host state (no-op without uplinks/links)
    validate_link_anchors(model)  # fail fast on a bad anchor before building any netns
    fds = collect_fds_from_child(lambda: wire_in_podman(model, runtime))
    try:
        for network in model.networks:
            if network.uplink is None:
                continue  # fully wired (incl. dataplane) in phase 1
            host_edge_connect(network, router_fd(fds, network))  # uplink veth
            configure_host_nat(network)  # ip_forward + host masquerade (init netns)
            apply_dataplane_fd(network, router_fd(fds, network))  # router dataplane via fd
        for container in model.containers:
            for link in container.links:
                link_connect(container, link, container_fd(fds, container))
    finally:
        for fd in fds.values():
            os.close(fd)


def down(model: Model, runtime: ResolvedRuntime) -> None:
    """Tear down both halves: host-edge state in the init parent, then the netns in a
    podman child (which reaps everything inside -- veths, routes, nft)."""
    teardown_host_edge(model)
    in_podman_context(runtime, model.teardown)


def parse_args(argv: list[str] | None = None) -> argparse.Namespace:
    """The CLI surface. Subcommands `up`/`down`; bare `turnip` defaults to `up`.
    `--config` overrides the $TURNIP_CONFIG / ./turnip.json discovery."""
    parser = argparse.ArgumentParser(
        prog="turnip",
        description="A persistent rootless container network for podman -- routed L3, "
        "default-deny, driven by a declarative turnip.json.",
    )
    parser.add_argument(
        "-c",
        "--config",
        type=Path,
        default=None,
        metavar="PATH",
        help="config file (default: $TURNIP_CONFIG, else ./turnip.json)",
    )
    sub = parser.add_subparsers(dest="command")
    sub.add_parser("up", help="create + wire the namespaces the config implies")
    sub.add_parser("down", help="tear them down")
    return parser.parse_args(argv)


def main() -> None:
    args = parse_args()

    # Resolve everything env-dependent here in the PARENT (env + passwd db intact),
    # build the runtime model, then run the command -- which forks into podman for the
    # netns work and does the host edge here in the init netns.
    turnip = load_config(args.config)
    runtime = resolve_runtime(turnip.runtime)
    # The host edge (any uplink/links) needs the init netns -> privilege. For now that
    # means sudo; CAP_NET_ADMIN-as-user is deferred (todo.md).
    if turnip.requires_root and os.geteuid() != 0:
        sys.exit("config needs the host edge (uplink/links) -- run via sudo")
    model = build_model(turnip, runtime.state_dir)

    match args.command:
        case "down":
            down(model, runtime)
        case _:  # "up", or no subcommand -> default to up
            up(model, runtime)


if __name__ == "__main__":
    main()
