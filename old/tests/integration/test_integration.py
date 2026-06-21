"""Integration tests -- one explicit function per scenario, with the network config
written INLINE (turnip({...})) right next to its hand-authored expectations (asserts via
Probe; never derived from turnip's model -- see harness.py). Skipped unless
TURNIP_INTEGRATION (conftest). The helpers (turnip/world/ensure_anchors/Probe) are
imported from harness -- explicit, no fixtures. Everything runs on one host: `world()` is
an in-host netns peer for the uplink + LAN-link scenarios.

Run them: `just itest` (dev VM, fast loop) or `nix build .#checks.<sys>.integration` (CI).
"""

from __future__ import annotations

import os

import pytest
from harness import Probe, Seg, connect_argv, ensure_anchors, turnip, turnip_attempt, world

# A DROPPED SYN never gets a reply, so a negative `connects` blocks for its WHOLE timeout
# (vs an allowed one that returns in ~ms). Everything here is a single-host netns where the
# RTT is microseconds, so this short deadline is still ~1000x the round trip -- ample to tell
# DROPPED from ALLOWED without paying the 2s default on every deny assertion. Positive
# connects keep the generous default (headroom against a false negative on a loaded box).
DENY_TIMEOUT = 0.5

# the world peer the two uplink tests reach over the host edge (routed, so host_cidr set).
WORLD_UPLINK = Seg("w-up", "198.51.100.2/24", "198.51.100.1/24")

# shared by more than one test, so lifted to a constant; single-use configs stay inline.
ROUTER = {
    "containers": {"zwave": {}, "hass": {}, "proxy": {}},
    "networks": {
        "lan": {
            "gateway": "10.0.0.1",
            "gateway_if": "gw0",
            "attach": {
                "zwave": {"ip": "10.0.0.11", "interface": "eth0"},
                "hass": {"ip": "10.0.0.12", "interface": "eth0"},
                "proxy": {"ip": "10.0.0.13", "interface": "eth0"},
            },
            "flows": [{"from": "zwave", "to": "hass", "proto": "tcp", "port": 443}],
        }
    },
}

UPLINK = {
    "containers": {"out": {}, "quiet": {}, "svc": {}},
    "networks": {
        "lan": {
            "gateway": "10.0.0.1",
            "gateway_if": "gw0",
            "uplink": {
                "host_if": "vethW", "router_if": "vethRup", "link": "169.254.1.0", "nat": True,
            },
            "attach": {
                "out": {"ip": "10.0.0.11", "interface": "eth0", "egress": True},
                "quiet": {"ip": "10.0.0.12", "interface": "eth0"},
                "svc": {
                    "ip": "10.0.0.13",
                    "interface": "eth0",
                    "ingress": [{"proto": "tcp", "host_port": 8080, "port": 80}],
                },
            },
        }
    },
}


def test_router() -> None:
    # routed /32s + default routes, then the directional flow matrix vs live listeners.
    with turnip(ROUTER):
        p = Probe()
        assert p.addrs("zwave", "eth0") == {"10.0.0.11/32"}
        assert p.addrs("hass", "eth0") == {"10.0.0.12/32"}
        assert p.addrs("proxy", "eth0") == {"10.0.0.13/32"}
        assert p.has_default_via("zwave", "10.0.0.1")
        with p.listener("hass", 443), p.listener("proxy", 443):
            assert p.connects("zwave", "10.0.0.12", 443), "zwave->hass:443 ALLOWED (the flow)"
            assert not p.connects("zwave", "10.0.0.13", 443, DENY_TIMEOUT), \
                "zwave->proxy:443 DROPPED (no flow)"
            assert not p.connects("proxy", "10.0.0.12", 443, DENY_TIMEOUT), \
                "proxy->hass:443 DROPPED (1-way)"


def test_links() -> None:
    # links are the L2 trust escape -- OUTSIDE every router's nft policy.
    ensure_anchors([("bridge", "br-lan"), ("dummy", "net-phys")])
    with turnip({
        "containers": {
            "br1": {"links": [{"type": "veth", "bridge": "br-lan", "name": "eth0",
                               "address": "192.168.50.10/24"}]},
            "br2": {"links": [{"type": "veth", "bridge": "br-lan", "name": "eth0",
                               "address": "192.168.50.11/24"}]},
            "p2p": {"links": [{"type": "veth", "peer": "host", "name": "eth0",
                               "address": "10.9.0.2/30"}]},
            "ph": {"links": [{"type": "phys", "dev": "net-phys", "name": "eth9",
                              "address": "192.168.9.10/24"}]},
        },
        "networks": {},
    }):
        p = Probe()
        # veth->bridge: two containers reach each other over br-lan with NO flow entry.
        assert p.addrs("br1", "eth0") == {"192.168.50.10/24"}
        assert p.addrs("br2", "eth0") == {"192.168.50.11/24"}
        with p.listener("br2", 9000):
            assert p.connects("br1", "192.168.50.11", 9000), "br1->br2 over br-lan (no flow)"
        # veth->host: host end left bare in init (turnip adds no host route).
        assert p.addrs("p2p", "eth0") == {"10.9.0.2/30"}
        assert p.init_iface_exists("vethL-p2p-eth0")
        # phys: moved in, renamed, borrowed (gone from init while the container holds it).
        assert p.addrs("ph", "eth9") == {"192.168.9.10/24"}
        assert not p.init_iface_exists("net-phys"), "net-phys moved into ph"


def test_uplink_egress() -> None:
    # default-deny across the uplink: only an `egress` container reaches world.
    with world(WORLD_UPLINK), turnip(UPLINK):
        p = Probe()
        assert p.connects("out", "198.51.100.2", 8888), "out has egress -> world reachable"
        assert not p.connects("quiet", "198.51.100.2", 8888, DENY_TIMEOUT), \
            "quiet has no egress -> dropped"


def test_uplink_ingress() -> None:
    # world -> host:8080 -> DNAT -> svc:80 (world is the external client).
    with world(WORLD_UPLINK) as w, turnip(UPLINK), Probe().listener("svc", 80):
        assert w.connects("198.51.100.1", 8080), "published port DNATs to svc"
        assert not w.connects("198.51.100.1", 9999, DENY_TIMEOUT), "unpublished port refused"


def test_linklan() -> None:
    # macvlan / ipvlan children reach world directly over their parent LANs (bypass host).
    with world(Seg("mv-par", "192.168.1.2/24"), Seg("iv-par", "192.168.2.2/24")), turnip({
        "containers": {
            "mv": {"links": [{"type": "macvlan", "parent": "mv-par", "name": "lan0",
                              "address": "192.168.1.50/24"}]},
            "iv": {"links": [{"type": "ipvlan", "parent": "iv-par", "name": "lan1",
                              "address": "192.168.2.50/24"}]},
        },
        "networks": {},
    }):
        p = Probe()
        assert p.addrs("mv", "lan0") == {"192.168.1.50/24"}
        assert p.link_kind("mv", "lan0") == "macvlan"
        assert p.connects("mv", "192.168.1.2", 8888), "macvlan child -> world over the LAN"
        assert p.addrs("iv", "lan1") == {"192.168.2.50/24"}
        assert p.link_kind("iv", "lan1") == "ipvlan"
        assert p.connects("iv", "192.168.2.2", 8888), "ipvlan child -> world over the LAN"


# each of these must FAIL at validate_link_anchors, before any netns is built. _box wraps
# the bad link(s) in a one-container config so each test shows just what's wrong.
def _box(*links: dict[str, object]) -> dict[str, object]:
    return {"containers": {"box": {"links": list(links)}}, "networks": {}}


def test_reject_missing_bridge() -> None:
    # veth->bridge onto an anchor that doesn't exist.
    assert turnip_attempt(_box(
        {"type": "veth", "bridge": "does-not-exist", "name": "eth0", "address": "192.168.50.10/24"},
    )) != 0


def test_reject_phys_on_primary_nic() -> None:
    # phys on the host's default-route NIC -- would sever the host.
    assert turnip_attempt(_box(
        {"type": "phys", "dev": "eth0", "name": "eth9", "address": "10.0.0.9/24"},
    )) != 0


def test_reject_macvlan_ipvlan_share_parent() -> None:
    # a device is a macvlan master XOR an ipvlan master -- can't be both.
    assert turnip_attempt(_box(
        {"type": "macvlan", "parent": "eth0", "name": "lan0",
         "address": "192.168.1.10/24", "default": True},
        {"type": "ipvlan", "parent": "eth0", "name": "lan1", "address": "192.168.1.11/24"},
    )) != 0


def test_podman_attach() -> None:
    # a real container joins zwave's netns and resolves hass BY NAME through the generated
    # /etc/hosts; the denied peer is still dropped. Needs the image (just python3 -- the
    # connect snippet is supplied here as `python3 -c ...`, not baked into the image).
    image = os.environ.get("TURNIP_TEST_IMAGE")
    if not image:
        pytest.skip("needs a loaded OCI image (set TURNIP_TEST_IMAGE)")
    probe = Probe()

    with turnip(ROUTER), Probe().listener("hass", 443), Probe().listener("proxy", 443):
        assert probe.run_container("zwave", image, connect_argv("hass", 443, 3)) == 0, \
            "real container reaches hass BY NAME (generated /etc/hosts)"
        assert probe.run_container("zwave", image, connect_argv("10.0.0.13", 443, 3)) != 0, \
            "denied peer (proxy) dropped even from a real container"
