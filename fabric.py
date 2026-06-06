#!/usr/bin/env python3
"""
fabric.py -- the routed fabric's model: addressing, hosts, and the flow matrix.

The single source of truth for WHAT the fabric is -- which containers, which IPs,
who may talk to whom. Imported by main.py (orchestration), nftops.py (ruleset),
and verify.py (checks); kept in its own leaf module so those three don't form an
import cycle through main (and so running main.py as a script doesn't drag the
model through a second import). No mechanism here -- just the data and the Host
type.
"""

# Owned, persistent router netns: hosts the gateway, every router-side veth end,
# the forwarding routes, and the nft table.
ROUTER = "router"

# The virtual gateway, made real on a dummy so it answers ARP with no uplink
# (see main.py). /32 everywhere: no on-link subnet exists.
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
        return f"vethR-{self.name}"  # router side; route + rp_filter anchor

    @property
    def cont_if(self) -> str:
        return "eth0"  # container side (one neighbour: the gw)


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
