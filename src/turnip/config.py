#!/usr/bin/env python3
"""
config.py -- the declarative Turnip model, in pydantic.

A typed loader + validator for `turnip.json` (see CONFIG-SKETCH.md). This is the
model only: WHO exists, WHO may talk, WHAT crosses the edge -- never the mechanism
that secures it. `main.py`/`nftlib.py`/`verify.py` are unchanged here; rewiring
them to consume `Turnip` is a separate step (per the sketch's build order).

Three entities, three scopes:
  - container   (containers.<name>)               identity + router-independent `links`
  - network     (networks.<name>)                 a router/bridge netns: gateway, uplink, flows
  - attachment  (networks.<name>.attach.<name>)   container x network: ip, interface, egress/ingress

Governing rule (default-deny): **omission must never widen.** Breadth is opt-in
and visible -- an explicit `proto` list, `port = "any"`, the deliberate
`egress = true` -- never the result of a dropped field. A missing `proto`/`port`
on a scoped rule is a load error, not a wildcard.

The security invariants (rp_filter-strict, ipv6-disabled, the implicit gateway +
icmp allows) stay as code in the mechanism, never as config -- a config that could
flip them is one that could silently defeat the anti-spoof pin.
"""

from __future__ import annotations

import enum
import ipaddress
from pathlib import Path
from typing import Annotated, Literal

from pydantic import (
    BaseModel,
    ConfigDict,
    Field,
    StringConstraints,
    field_validator,
    model_validator,
)

# --- scalar aliases & enums -----------------------------------------------

# IFNAMSIZ is 15: any kernel-facing interface name must fit, or the device can't
# be created. The loader rejects over-long names rather than letting the kernel
# truncate them into a silent collision.
IfName = Annotated[str, StringConstraints(min_length=1, max_length=15)]

# Closed sets of named alternatives are enums (StrEnum: members ARE their wire
# string, so `Proto.TCP == "tcp"` and f-strings render "tcp" -- handy for nft
# element generation downstream, and JSON round-trips by value).


class Proto(enum.StrEnum):
    """Layer-4 protocols expressible as nft elements. ICMP is portless (see the
    egress/ingress rule validators)."""

    TCP = "tcp"
    UDP = "udp"
    ICMP = "icmp"


class NetworkType(enum.StrEnum):
    ROUTER = "router"  # routed /32, default-deny (secure-by-default)
    BRIDGE = "bridge"  # shared-L2 trust group (DEFERRED)


class LinkType(enum.StrEnum):
    VETH = "veth"
    MACVLAN = "macvlan"
    IPVLAN = "ipvlan"
    PHYS = "phys"


class MacvlanMode(enum.StrEnum):
    BRIDGE = "bridge"
    PRIVATE = "private"
    VEPA = "vepa"
    PASSTHRU = "passthru"


class IpvlanMode(enum.StrEnum):
    L2 = "l2"
    L3 = "l3"  # kills bcast/mcast -> mDNS/discovery break (soft-warn at mechanism)
    L3S = "l3s"


# A port, 1..65535 -- the constraint lives in one reusable Annotated type
# instead of repeated inline `Field(ge=..., le=...)`.
Port = Annotated[int, Field(ge=1, le=65535)]

# A scoped port pattern: a concrete port, or the explicit wide token "any". The
# token stays a Literal -- a single sentinel, not a closed set of named
# alternatives, so an enum would be ceremony. There is no implicit wildcard.
PortPattern = Port | Literal["any"]

HOST_PREFIX = 32  # locked by topology (the routed /32 model); not configurable
LINK_PREFIX = 31  # uplink veth is a point-to-point /31 (RFC 3021); locked, like HOST_PREFIX

IPv4 = ipaddress.IPv4Address
IPv4Net = ipaddress.IPv4Network
IPv4If = ipaddress.IPv4Interface


class _Model(BaseModel):
    # extra="forbid" turns every typo into a load error instead of a silently
    # ignored key -- essential for a default-deny config where a misspelled
    # `egress` must not quietly mean "no egress".
    model_config = ConfigDict(extra="forbid")


# --- runtime --------------------------------------------------------------


class Runtime(_Model):
    """Execution environment -- separate from the model (who/what), this is the
    *where* (which user, dirs, binaries). All optional; the environment-dependent
    defaults (current user, $XDG_RUNTIME_DIR/turnip) are filled in by the caller,
    which owns the env + passwd-db reads (see `main.resolve_runtime`). Pure data."""

    user: str | None = None
    state_dir: Path | None = None  # holds the netns + generated hosts files
    nft: Path | None = None
    podman: Path | None = None


class ResolvedRuntime(_Model):
    """`Runtime` with the environment-dependent defaults filled in (by
    `main.resolve_runtime` -- this module stays free of env/IO)."""

    user: str
    uid: int
    gid: int
    state_dir: Path
    nft: Path | None = None
    podman: Path | None = None


# --- the edges: egress / ingress (on the attachment) ----------------------


class EgressRule(_Model):
    """One scoped outbound allowance. `proto` and `port` are both required (a
    dropped field is a load error, never a wildcard) -- except ICMP, which is a
    portless protocol. `proto` may be a list and fans out to one nft element per
    proto."""

    proto: list[Proto]
    port: PortPattern | None = None

    @field_validator("proto", mode="before")
    @classmethod
    def _listify(cls, v: object) -> object:
        # accept the scalar `"tcp"` sugar as well as `["udp", "tcp"]`
        return [v] if isinstance(v, str) else v

    @model_validator(mode="after")
    def _port_pin(self) -> EgressRule:
        port_bearing = [p for p in self.proto if p is not Proto.ICMP]
        if port_bearing and self.port is None:
            raise ValueError(
                f"egress rule for {port_bearing} missing 'port' (fail-closed); "
                f'use an explicit port or "any"'
            )
        if self.port is not None and not port_bearing:
            raise ValueError("icmp egress carries no port; drop the 'port' field")
        return self


class IngressRule(_Model):
    """One host->container DNAT mapping. Carries *two* ports because it does DNAT:
    `host_port` is published on the host edge, `port` is the container port.
    `port` defaults to `host_port` -- the one widening-*safe* default."""

    proto: Proto
    host_port: Port
    port: Port | None = None
    listen: IPv4 = IPv4("0.0.0.0")  # host address the DNAT listens on

    @field_validator("proto")
    @classmethod
    def _dnat_proto(cls, v: Proto) -> Proto:
        # DNAT rewrites a port, so the proto must carry one -- icmp can't.
        if v is Proto.ICMP:
            raise ValueError("ingress (DNAT) needs a port-bearing proto: tcp or udp")
        return v

    @model_validator(mode="after")
    def _default_port(self) -> IngressRule:
        if self.port is None:
            self.port = self.host_port
        return self


# `egress` on an attachment: a deliberate `true` (any external dest/proto/port),
# a list of scoped rules, or `false`/absent (default-deny).
Egress = bool | list[EgressRule]


# --- intra-network policy: flows (router-only) ----------------------------


class Flow(_Model):
    """A who-may-initiate-to-whom edge in a router network's forward chain.
    Endpoints are container names attached to *this* network. **Directional**:
    `from` may initiate to `to` on (proto, port), and only that -- the return path
    rides conntrack (ct established/related), so there is no reverse entry. Allowing
    the other direction means a second, explicit flow."""

    model_config = ConfigDict(extra="forbid", populate_by_name=True)

    from_: str = Field(alias="from")
    to: str
    proto: Proto
    port: PortPattern


# --- the attachment (container x network) ---------------------------------


class Attachment(_Model):
    """A container's membership in one network. Keyed by container under the
    network, so the (network, container) pair is unique by construction."""

    ip: IPv4
    interface: IfName
    default: bool = False  # owns the container's 0.0.0.0/0 route (one per container)
    egress: Egress = False
    ingress: list[IngressRule] = []

    @field_validator("egress", mode="before")
    @classmethod
    def _egress(cls, v: object) -> object:
        # Dispatch by shape so a malformed *list* surfaces the EgressRule error
        # (the fail-closed "missing 'port'"), not the bool branch's noise from
        # the bool|list union. A bare bool passes through untouched.
        if isinstance(v, bool):
            return v
        if isinstance(v, list):
            return [EgressRule.model_validate(r) for r in v]  # type: ignore[reportUnknownArgumentType]
        raise ValueError("egress must be a bool or a list of {proto, port} rules")

    @property
    def wants_edge(self) -> bool:
        """True if this attachment needs its network's uplink (any egress/ingress)."""
        return bool(self.egress) or bool(self.ingress)


# --- container links (holes into host networking) -- DEFERRED -------------
#
# A `link` moves a host netdev into a container, bypassing every router and its
# nft policy -- a deliberate trust escape. Modeled here (the sketch fully
# specifies it) though the mechanism is post-baseline. Ownership (own vs borrow)
# is implied by `type`, never a flag: veth/macvlan/ipvlan are virtual => owned;
# `phys` is physical => borrowed.


class _LinkBase(_Model):
    name: IfName  # container-side iface; unique within the container
    address: IPv4If  # a static "<CIDR>"; DHCP is deferred
    gateway: IPv4 | None = None
    routes: list[IPv4Net] = []
    mac: str | None = None
    mtu: int | None = Field(default=None, ge=68, le=65535)
    default: bool = False  # owns the container default route (one per container)


class MacvlanLink(_LinkBase):
    """Own MAC/IP on the parent's LAN; mDNS works; host<->child isolated."""

    type: Literal[LinkType.MACVLAN]
    parent: IfName
    mode: MacvlanMode = MacvlanMode.BRIDGE


class IpvlanLink(_LinkBase):
    """Single MAC (works on WiFi). `l3` kills bcast/mcast -> discovery breaks."""

    type: Literal[LinkType.IPVLAN]
    parent: IfName
    mode: IpvlanMode = IpvlanMode.L2


class VethLink(_LinkBase):
    """A veth into a host bridge (`bridge=`) or the root netns (`peer="host"`)."""

    type: Literal[LinkType.VETH]
    bridge: IfName | None = None
    peer: Literal["host"] | None = None  # single sentinel -> stays a Literal

    @model_validator(mode="after")
    def _one_anchor(self) -> VethLink:
        if (self.bridge is None) == (self.peer is None):
            raise ValueError('veth link needs exactly one of `bridge` or `peer="host"`')
        return self


class PhysLink(_LinkBase):
    """A BORROWED NIC/VF: moved in, returned to root on down, never deleted."""

    type: Literal[LinkType.PHYS]
    dev: IfName


Link = Annotated[
    MacvlanLink | IpvlanLink | VethLink | PhysLink,
    Field(discriminator="type"),
]


class Container(_Model):
    """A container identity. `links` are router-independent host-network holes."""

    links: list[Link] = []


# --- the network (a router or bridge netns) -------------------------------


class Uplink(_Model):
    """A veth between this network's router netns and the host netns -- what
    makes `egress`/`ingress` possible, and what makes the run rootful."""

    host_if: IfName
    router_if: IfName
    link: IPv4  # base of the point-to-point /31; the two ends are `link` and `link + 1`
    nat: bool = True  # masquerade (home default); false = routed (needs a LAN route)

    @field_validator("link")
    @classmethod
    def _even_base(cls, v: IPv4) -> IPv4:
        # the /31 prefix is locked (LINK_PREFIX), so only the base is configured;
        # require it even so {base, base+1} is a well-formed /31 pair with one
        # canonical spelling (rejects e.g. .1, the odd half of the same pair).
        if int(v) & 1:
            raise ValueError(
                f"uplink.link {v} must be the even base of a /31 (ends are base, base+1)"
            )
        return v


class Network(_Model):
    """A router netns (default) or a bridge netns. Everything stateful is
    network-scoped: the nft table, the gateway, the uplink/DNAT all live here, so
    `flows` and `attach` are per-network."""

    type: NetworkType = NetworkType.ROUTER  # secure-by-default
    gateway: IPv4
    gateway_if: IfName | None = None  # router: the dummy gateway iface
    subnet: IPv4Net | None = None  # bridge-only, required there; forbidden on router
    uplink: Uplink | None = None
    attach: dict[str, Attachment] = {}
    flows: list[Flow] = []  # router-only

    @model_validator(mode="after")
    def _network_rules(self) -> Network:
        if self.type is NetworkType.ROUTER:
            if self.subnet is not None:
                raise ValueError("subnet is forbidden on a router network (/32 everywhere)")
            if self.gateway_if is None:
                raise ValueError("router network requires 'gateway_if' (the gateway iface)")
        else:  # bridge
            if self.subnet is None:
                raise ValueError("bridge network requires 'subnet'")
            if self.flows:
                raise ValueError("flows is router-only; a bridge segment has no L3 forward hop")
            if self.gateway not in self.subnet:
                raise ValueError(f"bridge gateway {self.gateway} not within subnet {self.subnet}")

        for cname, a in self.attach.items():
            # the edge rides the uplink: no uplink, no path out -> the rule is meaningless
            if a.wants_edge and self.uplink is None:
                raise ValueError(f"{cname}: egress/ingress needs this network's uplink")
            # on a bridge, attach ip is a real subnet address (not a /32)
            if self.subnet is not None and a.ip not in self.subnet:
                raise ValueError(f"{cname}: ip {a.ip} not within subnet {self.subnet}")

        # flow endpoints must be attached to THIS network (reads locally)
        for fl in self.flows:
            for end in (fl.from_, fl.to):
                if end not in self.attach:
                    raise ValueError(f"flow endpoint {end!r} is not attached to this network")
        return self


# --- the whole network -----------------------------------------------------


class Turnip(_Model):
    """The parsed `turnip.json`: containers, networks, attachments + runtime.

    Holds the cross-cutting, container-global checks (the price of the
    network-centric layout) that no single network can see on its own.
    """

    runtime: Runtime = Field(default_factory=Runtime)
    containers: dict[str, Container] = {}
    networks: dict[str, Network]

    @property
    def requires_root(self) -> bool:
        """sudo is needed only when the host edge is in play: some network has an
        `uplink` or some container has `links`. A pure routed network with neither
        is the self-contained rootless tool of today."""
        return any(n.uplink for n in self.networks.values()) or any(
            c.links for c in self.containers.values()
        )

    @model_validator(mode="after")
    def _cross_cutting(self) -> Turnip:
        # (name, is_default) per container, gathered from links AND every attach
        ifaces: dict[str, list[tuple[str, bool]]] = {c: [] for c in self.containers}
        for cname, c in self.containers.items():
            for ln in c.links:
                ifaces[cname].append((ln.name, ln.default))

        # host_port is a single host-wide resource: collision check spans every
        # network's uplink, keyed by (listen, proto) after (trivial) proto fan-out.
        ports: dict[tuple[str, str, int], str] = {}
        for net, n in self.networks.items():
            for cname, a in n.attach.items():
                if cname not in self.containers:  # may only attach declared containers
                    raise ValueError(f"network {net!r}: attaches unknown container {cname!r}")
                ifaces[cname].append((a.interface, a.default))
                for ing in a.ingress:
                    key = (str(ing.listen), ing.proto, ing.host_port)
                    if key in ports:
                        raise ValueError(f"host_port collision {key}: {cname} vs {ports[key]}")
                    ports[key] = cname

        for cname, lst in ifaces.items():
            names = [nm for nm, _ in lst]
            dupes = sorted({nm for nm in names if names.count(nm) > 1})
            if dupes:
                raise ValueError(f"{cname}: duplicate interface name(s) {dupes}")
            ndefault = sum(1 for _, d in lst if d)
            if ndefault > 1:
                raise ValueError(f"{cname}: {ndefault} interfaces marked default; pick one")
            if ndefault == 0 and len(names) > 1:
                raise ValueError(
                    f"{cname}: {len(names)} interfaces and none marked default; pick one"
                )
        return self


# Loading is IO and lives in the caller (`main`): it reads $TURNIP_CONFIG / the
# file and calls `Turnip.model_validate(...)`. This module stays pure model +
# validation so it has no env/filesystem reads to reason about.
