"""Validation tests for the declarative Turnip model (turnip.config).

Covers the happy path (the shipped example + the deferred bridge/links shapes)
and the load-time rejections that enforce the default-deny "omission never
widens" rule and the cross-cutting container-global checks.
"""

from __future__ import annotations

import json
from pathlib import Path
from typing import Any

import pytest
from pydantic import ValidationError

from turnip.config import (
    LinkType,
    MacvlanLink,
    MacvlanMode,
    NetworkType,
    Proto,
    Turnip,
)

EXAMPLE = Path(__file__).resolve().parent / "turnip.example.json"


# --- helpers --------------------------------------------------------------


def load(path: Path) -> Turnip:
    """Read + validate a config file. Loading is IO and lives in `main` now, so
    the test owns its own one-liner (the model itself is what's under test)."""
    return Turnip.model_validate(json.loads(path.read_text()))


def net(**over: Any) -> dict[str, Any]:
    """A minimal valid router network with an uplink, plus overrides."""
    base: dict[str, Any] = {
        "gateway": "10.0.0.1",
        "gateway_if": "gw0",
        "uplink": {"host_if": "h", "router_if": "r", "link": "169.254.1.0"},
        "attach": {},
    }
    base.update(over)
    return base


def cfg(containers: dict[str, Any], networks: dict[str, Any]) -> dict[str, Any]:
    return {"containers": containers, "networks": networks}


def reject(data: dict[str, Any], match: str) -> str:
    with pytest.raises(ValidationError, match=match) as ei:
        Turnip.model_validate(data)
    return str(ei.value)


# --- happy path -----------------------------------------------------------


def test_example_loads() -> None:
    fab = load(EXAMPLE)
    lan = fab.networks["lan"]
    assert lan.type is NetworkType.ROUTER
    assert str(lan.gateway) == "10.0.0.1"
    assert str(lan.uplink.link) == "169.254.1.0"  # bare base, /31 implied
    # proto fans out and parses to enum members
    assert lan.attach["zwave"].egress[0].proto == [Proto.UDP, Proto.TCP]
    # example sets proxy ingress explicitly: host_port 8443 -> container port 443
    assert lan.attach["proxy"].ingress[0].host_port == 8443
    assert lan.attach["proxy"].ingress[0].port == 443
    assert lan.flows[0].from_ == "zwave" and lan.flows[0].to == "hass"
    assert fab.requires_root is True  # the lan uplink


def test_example_json_round_trips_with_alias() -> None:
    fab = load(EXAMPLE)
    dumped = json.loads(fab.model_dump_json(by_alias=True))
    flow = dumped["networks"]["lan"]["flows"][0]
    assert "from" in flow and "from_" not in flow
    assert dumped["networks"]["lan"]["attach"]["zwave"]["egress"][0]["proto"] == ["udp", "tcp"]


def test_bridge_network_deferred_shape() -> None:
    data = cfg(
        {"sensor": {}},
        {
            "iot": {
                "type": "bridge",
                "subnet": "10.2.0.0/24",
                "gateway": "10.2.0.1",
                "uplink": {"host_if": "vh", "router_if": "vr", "link": "169.254.2.0"},
                "attach": {"sensor": {"ip": "10.2.0.10", "interface": "eth0", "egress": True}},
            }
        },
    )
    fab = Turnip.model_validate(data)
    assert fab.networks["iot"].type is NetworkType.BRIDGE
    assert str(fab.networks["iot"].subnet) == "10.2.0.0/24"


def test_links_union_deferred_shape() -> None:
    data = cfg(
        {
            "box": {
                "links": [
                    {
                        "type": "macvlan",
                        "parent": "eth0",
                        "name": "lan0",
                        "address": "192.168.1.12/24",
                        "gateway": "192.168.1.1",
                    },
                    {
                        "type": "veth",
                        "bridge": "br-lan",
                        "name": "eth2",
                        "address": "192.168.1.13/24",
                    },
                    {
                        "type": "phys",
                        "dev": "enp3s0",
                        "name": "eth3",
                        "address": "192.168.1.20/24",
                        "default": True,
                    },
                ]
            }
        },
        {},
    )
    fab = Turnip.model_validate(data)
    link0 = fab.containers["box"].links[0]
    assert isinstance(link0, MacvlanLink)
    assert link0.type is LinkType.MACVLAN
    assert link0.mode is MacvlanMode.BRIDGE  # default
    assert fab.requires_root is True  # a link makes it rootful


def test_egress_any_token_accepted() -> None:
    data = cfg(
        {"a": {}},
        {
            "n": net(
                attach={
                    "a": {
                        "ip": "10.0.0.5",
                        "interface": "eth0",
                        "egress": [{"proto": "tcp", "port": "any"}],
                    }
                }
            )
        },
    )
    fab = Turnip.model_validate(data)
    assert fab.networks["n"].attach["a"].egress[0].port == "any"


def test_icmp_egress_is_portless() -> None:
    data = cfg(
        {"a": {}},
        {
            "n": net(
                attach={"a": {"ip": "10.0.0.5", "interface": "eth0", "egress": [{"proto": "icmp"}]}}
            )
        },
    )
    fab = Turnip.model_validate(data)
    assert fab.networks["n"].attach["a"].egress[0].port is None


# --- rejections: the fail-closed pins -------------------------------------


def test_scoped_egress_missing_port_rejected() -> None:
    data = cfg(
        {"a": {}},
        {
            "n": net(
                attach={"a": {"ip": "10.0.0.5", "interface": "eth0", "egress": [{"proto": "tcp"}]}}
            )
        },
    )
    assert "missing 'port'" in reject(data, "missing 'port'")


def test_egress_needs_uplink() -> None:
    n = {
        "gateway": "10.0.0.1",
        "gateway_if": "f0",
        "attach": {"a": {"ip": "10.0.0.5", "interface": "eth0", "egress": True}},
    }
    reject(cfg({"a": {}}, {"n": n}), "needs this network's uplink")


def test_port_bounds_enforced() -> None:
    data = cfg(
        {"a": {}},
        {
            "n": net(
                attach={
                    "a": {
                        "ip": "10.0.0.5",
                        "interface": "eth0",
                        "ingress": [{"proto": "tcp", "host_port": 99999}],
                    }
                }
            )
        },
    )
    reject(data, "less than or equal to 65535")


def test_icmp_ingress_rejected() -> None:
    data = cfg(
        {"a": {}},
        {
            "n": net(
                attach={
                    "a": {
                        "ip": "10.0.0.5",
                        "interface": "eth0",
                        "ingress": [{"proto": "icmp", "host_port": 1}],
                    }
                }
            )
        },
    )
    reject(data, "port-bearing proto")


# --- rejections: network type rules ---------------------------------------


def test_subnet_forbidden_on_router() -> None:
    reject(cfg({}, {"r": net(subnet="10.0.0.0/24")}), "subnet is forbidden on a router")


def test_flows_forbidden_on_bridge() -> None:
    data = cfg(
        {"a": {}, "b": {}},
        {
            "b": {
                "type": "bridge",
                "subnet": "10.2.0.0/24",
                "gateway": "10.2.0.1",
                "attach": {
                    "a": {"ip": "10.2.0.5", "interface": "eth0"},
                    "b": {"ip": "10.2.0.6", "interface": "eth0"},
                },
                "flows": [{"from": "a", "to": "b", "proto": "tcp", "port": 1}],
            }
        },
    )
    reject(data, "router-only")


def test_router_requires_gateway_if() -> None:
    reject(cfg({}, {"r": {"gateway": "10.0.0.1"}}), "requires 'gateway_if'")


# --- rejections: cross-cutting container-global checks ---------------------


def test_flow_endpoint_must_be_attached() -> None:
    data = cfg(
        {"a": {}},
        {
            "n": net(
                attach={"a": {"ip": "10.0.0.5", "interface": "eth0"}},
                flows=[{"from": "a", "to": "ghost", "proto": "tcp", "port": 1}],
            )
        },
    )
    reject(data, "not attached to this network")


def test_unknown_container_in_attach() -> None:
    reject(
        cfg({}, {"n": net(attach={"ghost": {"ip": "10.0.0.5", "interface": "eth0"}})}),
        "unknown container",
    )


def test_two_defaults_rejected() -> None:
    data = cfg(
        {"a": {}},
        {
            "n1": net(attach={"a": {"ip": "10.0.0.5", "interface": "eth0", "default": True}}),
            "n2": {
                "gateway": "10.1.0.1",
                "gateway_if": "f1",
                "attach": {"a": {"ip": "10.1.0.5", "interface": "eth1", "default": True}},
            },
        },
    )
    reject(data, "marked default; pick one")


def test_zero_default_multi_iface_rejected() -> None:
    data = cfg(
        {"a": {}},
        {
            "n1": net(attach={"a": {"ip": "10.0.0.5", "interface": "eth0"}}),
            "n2": {
                "gateway": "10.1.0.1",
                "gateway_if": "f1",
                "attach": {"a": {"ip": "10.1.0.5", "interface": "eth1"}},
            },
        },
    )
    reject(data, "none marked default")


def test_duplicate_interface_in_container() -> None:
    data = cfg(
        {"a": {"links": [{"type": "phys", "dev": "x", "name": "eth0", "address": "1.2.3.4/24"}]}},
        {"n": net(attach={"a": {"ip": "10.0.0.5", "interface": "eth0"}})},
    )
    reject(data, "duplicate interface")


def test_host_port_collision_across_networks() -> None:
    data = cfg(
        {"a": {}, "b": {}},
        {
            "n": net(
                attach={
                    "a": {
                        "ip": "10.0.0.5",
                        "interface": "eth0",
                        "ingress": [{"proto": "tcp", "host_port": 8443, "port": 443}],
                    },
                    "b": {
                        "ip": "10.0.0.6",
                        "interface": "eth0",
                        "ingress": [{"proto": "tcp", "host_port": 8443, "port": 444}],
                    },
                }
            )
        },
    )
    reject(data, "host_port collision")


# --- rejections: typing & enums -------------------------------------------


def test_uplink_link_must_be_even_base() -> None:
    n = net()
    n["uplink"]["link"] = "169.254.1.1"  # odd half of the /31 pair
    reject(cfg({}, {"n": n}), "even base of a /31")


def test_uplink_link_rejects_prefix() -> None:
    n = net()
    n["uplink"]["link"] = "169.254.1.0/31"  # prefix is locked, not configured
    reject(cfg({}, {"n": n}), "valid IPv4 address")


def test_ifname_length_capped() -> None:
    reject(cfg({}, {"n": net(gateway_if="this-name-is-too-long")}), "at most 15 characters")


def test_extra_key_forbidden() -> None:
    reject(cfg({}, {"n": net(egres=True)}), "Extra inputs are not permitted")


def test_bad_enum_value() -> None:
    reject(cfg({}, {"n": {"type": "switch", "gateway": "10.0.0.1"}}), "'router' or 'bridge'")
