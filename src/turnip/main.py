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

import contextlib
import json
import os
import pwd
import sys
from collections.abc import Generator
from dataclasses import dataclass
from pathlib import Path
from typing import Self

from pyroute2 import IPRoute, NetNS

from . import nftlib as nft
from .config import (
    HOST_PREFIX,
    LINK_PREFIX,
    Flow,
    NetworkType,
    Proto,
    ResolvedRuntime,
    Runtime,
    Turnip,
)
from .netns import (
    collect_fds_from_child,
    create_netns,
    enter_podman,
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
class Container:
    """A container's on-disk state: its netns and its generated hosts file, both
    under `<state_dir>/containers/<name>/`. `links` arrive in milestone 5."""

    name: str
    netns_path: str  # containers/<name>/netns (the bind-mount)
    hosts_path: str  # containers/<name>/hosts (bind-mounted to /etc/hosts)
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


def build_model(turnip: Turnip, state_dir: Path) -> Model:
    """Lower the validated config into the runtime model (no IO, no handles yet).

    Containers come from the top-level `containers` map (the authoritative set -- a
    links-only container with no attachment still gets its netns); each network's
    endpoints reference those shared container objects (so a multi-homed container
    is one netns with N endpoints)."""
    containers = {
        name: Container(
            name,
            netns_path=str(state_dir / "containers" / name / "netns"),
            hosts_path=str(state_dir / "containers" / name / "hosts"),
        )
        for name in turnip.containers
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
            Endpoint(containers[cname], router_if(cname), att.interface, str(att.ip))
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
    (nothing is on-link under /32), then default via it. Router side: the single
    /32 device route that is both the forwarding entry and rp_filter's reverse-path
    anchor. Both netns are recreated clean by `up`, so this just builds."""
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
    # /32 => gateway is not on-link; pin it link-scope, then default via it.
    cont.route("add", dst=network.gateway, oif=cidx, scope="link")
    cont.route("add", dst="default", gateway=network.gateway, oif=cidx)
    print(
        f"  wired {ep.container.name}: {ep.cont_if} {ep.ip}/{HOST_PREFIX} -> gw "
        f"{network.gateway} <-> {ep.router_if} (route {ep.ip}/{HOST_PREFIX} dev {ep.router_if})"
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
    return sysctls


def build_nft(network: Network) -> nft.Ruleset:
    """The `inet turnip` ruleset for one router netns: the forward flow matrix.

    flush-and-reload (Table.reload) so re-applying replaces the table atomically.
    Maps precede the rules that use them. The forward chain (policy drop) accepts:
    established/related (conntrack return path -- so flows are one-way in the maps);
    drops invalid; then for new conns -- ICMP only between gateway-authorized pairs,
    any-port gateway pairs (allowed_hosts), and service-scoped pairs (allowed_flows;
    `th dport` covers tcp AND udp). Else policy drop."""
    gw = network.gateway
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


def configure_dataplane(network: Network) -> None:
    """Write a router netns's sysctls + load its nft table, run from WITHIN podman's
    userns (phase 1) via a forked setns-by-path hop. This stays in phase 1 -- not the
    init-side parent -- because entering a netns (setns CLONE_NEWNET) needs CAP_SYS_ADMIN
    in the CALLER's own userns: the rootless user has it only inside podman's userns
    (owner-match grants caps in the TARGET userns, not the caller's). Both ops need the
    veths to already exist, so it runs after all wiring.

    (When uplink nft lands, the uplink-dependent rules -- which need the uplink veth,
    created host-side in phase 2 -- get re-applied by the ROOT parent over the fd; root
    *does* hold CAP_SYS_ADMIN in the init userns. That split is rootful-only.)"""
    sysctls = router_sysctls(network)
    rules = build_nft(network)

    def apply() -> str:
        write_sysctls(sysctls)
        nft.load(rules)
        return ""

    run_in_netns(network.netns_path, apply)
    print(
        f"  {network.name} dataplane: ip_forward + per-veth proxy_arp/rp_filter + "
        f"ipv6 off + nft '{NFT_TABLE}'"
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


def wire_in_podman(model: Model, runtime: ResolvedRuntime) -> dict[str, int]:
    """Phase 1, run in a forked child: become the rootless user, enter podman's ns,
    rebuild the netns clean-slate, wire each network (gateway + /32 routed veths + hosts
    files), and apply each router's dataplane (sysctls + nft) -- all the netns work,
    done from inside podman's userns where the rootless user holds CAP_SYS_ADMIN. Then
    open and RETURN a router-netns fd per network: the netns persist by bind-mount, so
    the fds (shipped to the init-side parent) stay valid after this child exits, for the
    parent's phase-2 host edge.

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
    # sockets closed; dataplane (sysctls + nft) per router, still inside podman's userns
    for network in model.networks:
        configure_dataplane(network)
    return {net.name: os.open(net.netns_path, os.O_RDONLY) for net in model.networks}


def teardown_host_edge(model: Model) -> None:
    """Remove host-side uplink state in the init netns -- the host veth end, which does
    NOT die with the router netns (its peer does, the host end persists). Runs in the
    privileged parent, before re-wiring (up = down + build) and on `down`. Idempotent:
    skips a veth that's already gone. A no-op for networks without an uplink, so the
    rootless path never touches the init netns here."""
    uplinks = [net.uplink for net in model.networks if net.uplink is not None]
    if not uplinks:
        return
    ipr = IPRoute()
    try:
        for up in uplinks:
            found = ipr.link_lookup(ifname=up.host_if)
            if found:
                ipr.link("del", index=found[0])
                print(f"  removed host uplink veth {up.host_if}")
    finally:
        ipr.close()


def host_edge_connect(network: Network, router_fd: int) -> None:
    """Wire a network's uplink veth across the init<->router boundary -- the host edge,
    run in the privileged init-side parent (phase 2). No-op without an uplink.

    The veth is born in the init netns; the router end is moved into the router netns by
    fd (IFLA_NET_NS_FD -- needs only CAP_NET_ADMIN, which root holds in init, flowing
    down to podman's userns). The host end is addressed on the /31 init-side; the router
    end + its default route via the host end are set inside the router netns, entered by
    fd (setns, which root may do from the init userns). No nft/masquerade yet."""
    up = network.uplink
    if up is None:
        return
    ipr = IPRoute()
    try:
        ipr.link("add", ifname=up.host_if, kind="veth", peer={"ifname": up.router_if})
        ipr.link("set", index=ipr.link_lookup(ifname=up.router_if)[0], net_ns_fd=router_fd)
        hidx = ipr.link_lookup(ifname=up.host_if)[0]
        ipr.addr("add", index=hidx, address=up.host_ip, prefixlen=LINK_PREFIX)
        ipr.link("set", index=hidx, state="up")
    finally:
        ipr.close()

    def configure_router_end() -> str:
        r = IPRoute()
        try:
            ridx = r.link_lookup(ifname=up.router_if)[0]
            r.addr("add", index=ridx, address=up.router_ip, prefixlen=LINK_PREFIX)
            r.link("set", index=ridx, state="up")
            r.route("add", dst="default", gateway=up.host_ip, oif=ridx)
        finally:
            r.close()
        return ""

    run_in_netns_fd(router_fd, configure_router_end)
    print(
        f"  {network.name} uplink: {up.host_if} {up.host_ip} <-> {up.router_if} "
        f"{up.router_ip}/{LINK_PREFIX} (router default via {up.host_ip})"
    )


def up(model: Model, runtime: ResolvedRuntime) -> None:
    """Two phases joined by the fd bridge. Phase 1 (a podman child, wire_in_podman) does
    all the netns work -- rebuild clean-slate, wire each network, apply the dataplane --
    inside podman's userns, and ships a router-netns fd per network. Phase 2 (this
    init-side parent) connects each network's host edge (uplink veth) over those fds; it
    needs only CAP_NET_ADMIN (the IFLA_NET_NS_FD move) which root holds in the init
    netns, and is a no-op for networks without an uplink. Clean slate spans both halves
    (up = down + build): the host-edge teardown here + the netns teardown in phase 1."""
    teardown_host_edge(model)  # parent: clear prior host state (no-op until uplinks)
    router_fds = collect_fds_from_child(lambda: wire_in_podman(model, runtime))
    try:
        for network in model.networks:
            host_edge_connect(network, router_fds[network.name])
    finally:
        for fd in router_fds.values():
            os.close(fd)


def down(model: Model, runtime: ResolvedRuntime) -> None:
    """Tear down both halves: host-edge state in the init parent, then the netns in a
    podman child (which reaps everything inside -- veths, routes, nft)."""
    teardown_host_edge(model)
    in_podman_context(runtime, model.teardown)


def main() -> None:
    cmd = sys.argv[1] if len(sys.argv) > 1 else "up"
    fn = {"up": up, "down": down}.get(cmd)
    if fn is None:
        sys.exit(f"usage: {sys.argv[0]} {{up|down}}")

    # Resolve everything env-dependent here in the PARENT (env + passwd db intact),
    # build the runtime model, then run the command -- which forks into podman for the
    # netns work and does the host edge here in the init netns.
    turnip = load_config()
    runtime = resolve_runtime(turnip.runtime)
    # The host edge (any uplink/links) needs the init netns -> privilege. For now that
    # means sudo; CAP_NET_ADMIN-as-user is deferred (todo.md).
    if turnip.requires_root and os.geteuid() != 0:
        sys.exit("config needs the host edge (uplink/links) -- run via sudo")
    model = build_model(turnip, runtime.state_dir)
    fn(model, runtime)


if __name__ == "__main__":
    main()
