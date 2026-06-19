"""The integration scenario registry -- the single source of truth.

Each Scenario pairs a config (the network to build) with HAND-AUTHORED expectations
(plain asserts via Probe -- never derived from turnip's model; see probe.py). The pytest
parametrization (test_integration.py) runs them; the NixOS nodes select by env/marker
(needs_world for the two-node uplink test, needs_image for podman). Adding a Scenario
here makes it run everywhere with no .nix edit.
"""

from __future__ import annotations

from collections.abc import Callable
from dataclasses import dataclass, field

import pytest
from probe import Probe

Anchor = tuple[str, str]  # (kind, name) -- e.g. ("bridge", "br-lan"), ("dummy", "net-phys")


@dataclass(frozen=True)
class Scenario:
    name: str
    config: str
    check: Callable[[Probe], None]
    anchors: list[Anchor] = field(default_factory=list)
    marks: tuple[pytest.MarkDecorator, ...] = ()


# --- single-node scenarios --------------------------------------------------


def _router(p: Probe) -> None:
    # routed /32s + default routes, then the directional flow matrix vs live listeners.
    assert p.addrs("zwave", "eth0") == {"10.0.0.11/32"}
    assert p.addrs("hass", "eth0") == {"10.0.0.12/32"}
    assert p.addrs("proxy", "eth0") == {"10.0.0.13/32"}
    assert p.has_default_via("zwave", "10.0.0.1")
    assert p.has_default_via("proxy", "10.0.0.1")
    with p.listener("hass", 443), p.listener("proxy", 443):
        assert p.connects("zwave", "10.0.0.12", 443), "zwave->hass:443 ALLOWED (the flow)"
        assert not p.connects("zwave", "10.0.0.13", 443), "zwave->proxy:443 DROPPED (no flow)"
        assert not p.connects("proxy", "10.0.0.12", 443), "proxy->hass:443 DROPPED (directional)"


def _links(p: Probe) -> None:
    # veth->bridge: two containers reach each other over br-lan with NO flow (the bypass).
    assert p.addrs("br1", "eth0") == {"192.168.50.10/24"}
    assert p.addrs("br2", "eth0") == {"192.168.50.11/24"}
    with p.listener("br2", 9000):
        assert p.connects("br1", "192.168.50.11", 9000), "br1->br2 over br-lan (no flow needed)"
    # veth->host: turnip leaves the host end bare in init (the operator routes to it).
    assert p.addrs("p2p", "eth0") == {"10.9.0.2/30"}
    assert p.init_iface_exists("vethL-p2p-eth0"), "p2p host end present in init"
    # phys: moved in, renamed to the configured name, borrowed (gone from init).
    assert p.addrs("ph", "eth9") == {"192.168.9.10/24"}
    assert not p.init_iface_exists("net-phys"), "net-phys should be gone from init (moved into ph)"


# --- two-node scenarios (need the `world` peer; run by tests/nixos/uplink.nix) -------


def _uplink_egress(p: Probe) -> None:
    # default-deny across the uplink: only an `egress` container reaches world (:8888).
    assert p.has_default_via("out", "10.0.0.1")
    assert p.connects("out", "192.168.1.2", 8888), "out has egress -> world reachable"
    assert not p.connects("quiet", "192.168.1.2", 8888), "quiet has no egress -> dropped"


def _linklan(p: Probe) -> None:
    # macvlan / ipvlan children reach world directly over their LANs (bypassing host).
    assert p.addrs("mv", "lan0") == {"192.168.1.50/24"}
    assert p.link_kind("mv", "lan0") == "macvlan"
    assert p.connects("mv", "192.168.1.2", 8888), "macvlan child -> world over the LAN"
    assert p.addrs("iv", "lan1") == {"192.168.2.50/24"}
    assert p.link_kind("iv", "lan1") == "ipvlan"
    assert p.connects("iv", "192.168.2.2", 8888), "ipvlan child -> world over the LAN"


SCENARIOS: list[Scenario] = [
    Scenario("router", "router.json", _router),
    Scenario("links", "links.json", _links, anchors=[("bridge", "br-lan"), ("dummy", "net-phys")]),
    Scenario("uplink_egress", "uplink.json", _uplink_egress, marks=(pytest.mark.needs_world,)),
    Scenario("linklan", "linklan.json", _linklan, marks=(pytest.mark.needs_world,)),
]

# configs whose `turnip up` must FAIL at validate_link_anchors (before building anything).
NEGATIVES: list[tuple[str, str]] = [
    ("badbridge", "neg_badbridge.json"),
    ("physprimary", "neg_physprimary.json"),
    ("coexist", "neg_coexist.json"),
]
