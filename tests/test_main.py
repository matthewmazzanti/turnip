"""Tests for main.py's pure build step: config -> runtime model.

No namespaces and no IO here -- build_model() produces the Container/Network/
Endpoint objects (paths, names, ips) with handles unbound (.netns is None). The
live wiring is exercised by the integration smoke, not here.
"""

from __future__ import annotations

from pathlib import Path
from typing import Any

import pytest

from turnip import main
from turnip.config import Turnip

STATE = Path("/n")


def _turnip(containers: dict[str, Any], networks: dict[str, Any]) -> Turnip:
    return Turnip.model_validate({"containers": containers, "networks": networks})


def _model(containers: dict[str, Any], networks: dict[str, Any]) -> main.Model:
    return main.build_model(_turnip(containers, networks), STATE)


def test_build_model_state_layout() -> None:
    model = _model(
        {"zwave": {}, "hass": {}},
        {"lan": {"gateway": "10.0.0.1", "gateway_if": "gw0"}},
    )
    # container state dir holds {netns, hosts}; router is a bare netns
    zwave = model.containers[0]
    assert zwave.netns_path == "/n/containers/zwave/netns"
    assert zwave.hosts_path == "/n/containers/zwave/hosts"
    assert zwave.state_dir == "/n/containers/zwave"
    assert [n.netns_path for n in model.networks] == ["/n/routers/lan"]
    # nodes = containers first, then routers; all unbound at build time
    assert [node.name for node in model.nodes] == ["zwave", "hass", "lan"]
    assert all(node.netns is None for node in model.nodes)


def test_build_model_lowers_endpoints() -> None:
    model = _model(
        {"zwave": {}, "hass": {}},
        {
            "lan": {
                "gateway": "10.0.0.1",
                "gateway_if": "gw0",
                "attach": {
                    "zwave": {"ip": "10.0.0.11", "interface": "eth0"},
                    "hass": {"ip": "10.0.0.12", "interface": "eth0"},
                },
            }
        },
    )
    net = model.networks[0]
    assert net.gateway == "10.0.0.1" and net.gateway_if == "gw0"
    eps = {ep.container.name: ep for ep in net.endpoints}
    assert eps["zwave"].ip == "10.0.0.11"
    assert eps["zwave"].router_if == "vethR-zwave"
    assert eps["zwave"].cont_if == "eth0"
    # the endpoint references the shared Container object (one netns per container)
    assert eps["zwave"].container is next(c for c in model.containers if c.name == "zwave")


def test_links_only_container_still_gets_a_netns() -> None:
    # a container with no network attachment is still in the `containers` map, so it
    # still gets its netns (its links land there in milestone 5)
    model = _model(
        {"box": {"links": [{"type": "phys", "dev": "x", "name": "eth0", "address": "1.2.3.4/24"}]}},
        {},
    )
    assert [c.name for c in model.containers] == ["box"]
    assert model.containers[0].netns_path == "/n/containers/box/netns"
    assert model.networks == []


def test_handle_raises_when_unbound() -> None:
    model = _model({"box": {}}, {})
    with pytest.raises(RuntimeError, match="not bound"):
        _ = model.containers[0].handle


def _hub_spoke() -> main.Model:
    # zwave -> hass, hass -> proxy (directional). proxy initiates to nobody.
    return _model(
        {"zwave": {}, "hass": {}, "proxy": {}},
        {
            "lan": {
                "gateway": "10.0.0.1",
                "gateway_if": "gw0",
                "attach": {
                    "zwave": {"ip": "10.0.0.11", "interface": "eth0"},
                    "hass": {"ip": "10.0.0.12", "interface": "eth0"},
                    "proxy": {"ip": "10.0.0.13", "interface": "eth0"},
                },
                "flows": [
                    {"from": "zwave", "to": "hass", "proto": "tcp", "port": 443},
                    {"from": "hass", "to": "proxy", "proto": "tcp", "port": 443},
                ],
            }
        },
    )


def _by_name(model: main.Model, name: str) -> main.Container:
    return next(c for c in model.containers if c.name == name)


def test_container_peers_are_directional() -> None:
    model = _hub_spoke()
    # peers = outbound-flow targets only (the reverse view, directional)
    assert main.container_peers(model, _by_name(model, "zwave")) == {"hass": "10.0.0.12"}
    assert main.container_peers(model, _by_name(model, "hass")) == {"proxy": "10.0.0.13"}
    assert main.container_peers(model, _by_name(model, "proxy")) == {}  # initiates to nobody


def test_hosts_file_has_self_and_peers() -> None:
    model = _hub_spoke()
    hosts = main.hosts_file(model, _by_name(model, "zwave"))
    assert hosts.splitlines() == [
        "127.0.0.1 localhost",
        "10.0.0.11 zwave",  # self
        "10.0.0.12 hass",  # reachable peer
    ]
    # proxy reaches no one -> just localhost + self
    assert main.hosts_file(model, _by_name(model, "proxy")).splitlines() == [
        "127.0.0.1 localhost",
        "10.0.0.13 proxy",
    ]


def test_router_if_derives_from_container() -> None:
    assert main.router_if("zwave") == "vethR-zwave"


def test_router_if_rejects_overlong_name() -> None:
    # "vethR-" (6) + a 10+ char container overflows IFNAMSIZ (15)
    with pytest.raises(ValueError, match="IFNAMSIZ"):
        main.router_if("a-very-long-container-name")
