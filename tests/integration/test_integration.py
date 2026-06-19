"""Integration tests -- one explicit function per scenario, with the network config
written INLINE (turnip({...})) right next to its hand-authored expectations (asserts via
Probe; never derived from turnip's model -- see probe.py). Skipped unless
TURNIP_INTEGRATION (conftest). Everything runs on one host: the `world` fixture is an
in-host netns peer for the uplink + LAN-link scenarios.

Run them: `just itest` (dev VM, fast loop) or `nix build .#checks.<sys>.integration` (CI).
"""

from __future__ import annotations

import os
import subprocess

import pytest
from probe import Probe

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


def test_router(turnip) -> None:
    # routed /32s + default routes, then the directional flow matrix vs live listeners.
    with turnip(ROUTER):
        p = Probe()
        assert p.addrs("zwave", "eth0") == {"10.0.0.11/32"}
        assert p.addrs("hass", "eth0") == {"10.0.0.12/32"}
        assert p.addrs("proxy", "eth0") == {"10.0.0.13/32"}
        assert p.has_default_via("zwave", "10.0.0.1")
        with p.listener("hass", 443), p.listener("proxy", 443):
            assert p.connects("zwave", "10.0.0.12", 443), "zwave->hass:443 ALLOWED (the flow)"
            assert not p.connects("zwave", "10.0.0.13", 443), "zwave->proxy:443 DROPPED (no flow)"
            assert not p.connects("proxy", "10.0.0.12", 443), "proxy->hass:443 DROPPED (1-way)"


def test_links(turnip, anchors) -> None:
    # links are the L2 trust escape -- OUTSIDE every router's nft policy.
    anchors([("bridge", "br-lan"), ("dummy", "net-phys")])
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


def test_uplink_egress(turnip, world) -> None:
    # default-deny across the uplink: only an `egress` container reaches world.
    with turnip(UPLINK):
        p = Probe()
        assert p.connects("out", world.ip, 8888), "out has egress -> world reachable"
        assert not p.connects("quiet", world.ip, 8888), "quiet has no egress -> dropped"


def test_uplink_ingress(turnip, world) -> None:
    # world -> host:8080 -> DNAT -> svc:80 (world is the external client).
    with turnip(UPLINK), Probe().listener("svc", 80):
        assert world.connects(world.host_uplink_ip, 8080), "published port DNATs to svc"
        assert not world.connects(world.host_uplink_ip, 9999), "unpublished port refused"


def test_linklan(turnip, world) -> None:
    # macvlan / ipvlan children reach world directly over their parent LANs (bypass host).
    with turnip({
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


# configs whose `turnip up` must FAIL at validate_link_anchors, before building anything.
def _box(*links: dict[str, object]) -> dict[str, object]:
    return {"containers": {"box": {"links": list(links)}}, "networks": {}}


_REJECTED = {
    "badbridge": _box(
        {"type": "veth", "bridge": "does-not-exist", "name": "eth0", "address": "192.168.50.10/24"},
    ),
    "phys_primary": _box(
        {"type": "phys", "dev": "eth0", "name": "eth9", "address": "10.0.0.9/24"},
    ),
    "macvlan_ipvlan_share_parent": _box(
        {"type": "macvlan", "parent": "eth0", "name": "lan0",
         "address": "192.168.1.10/24", "default": True},
        {"type": "ipvlan", "parent": "eth0", "name": "lan1", "address": "192.168.1.11/24"},
    ),
}


@pytest.mark.parametrize("config", _REJECTED.values(), ids=_REJECTED.keys())
def test_config_rejected(config, turnip_attempt) -> None:
    assert turnip_attempt(config) != 0


def test_podman_attach(turnip) -> None:
    # a real container joins zwave's netns via run-container.sh and resolves hass BY NAME
    # through the generated /etc/hosts; the denied peer is still dropped. Needs the image.
    image = os.environ.get("TURNIP_TEST_IMAGE")
    if not image:
        pytest.skip("needs a loaded OCI image (set TURNIP_TEST_IMAGE)")
    tconnect = os.environ["TURNIP_TCONNECT"]
    runc = os.environ["TURNIP_RUNCONTAINER"]

    def run_container(*args: str) -> int:
        argv = ["bash", runc, *args]
        if os.geteuid() == 0:  # run-container.sh drives rootless podman -> run as the owner
            argv = ["sudo", "-u", "homelab", "env", "XDG_RUNTIME_DIR=/run/user/1001",
                    "HOME=/home/homelab", *argv]
        return subprocess.run(argv).returncode

    with turnip(ROUTER), Probe().listener("hass", 443), Probe().listener("proxy", 443):
        ok = run_container("zwave", image, "--", tconnect, "hass", "443")
        assert ok == 0, "real container should reach hass BY NAME (generated /etc/hosts)"
        denied = run_container("zwave", image, "--", tconnect, "10.0.0.13", "443")
        assert denied != 0, "denied peer (proxy) dropped even from a real container"
