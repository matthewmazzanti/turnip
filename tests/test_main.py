"""Tests for main.py's pure config→name derivations (milestones 1-2).

No namespaces and no IO here -- just that the right netns names + veth names fall
out of a parsed config. Grows as later milestones add more derivations.
"""

from __future__ import annotations

from typing import Any

import pytest

from turnip import main
from turnip.config import Turnip


def _turnip(containers: dict[str, Any], networks: dict[str, Any]) -> Turnip:
    return Turnip.model_validate({"containers": containers, "networks": networks})


def test_netns_name_helpers_use_the_two_subdirs() -> None:
    assert main.router_netns("lan") == "routers/lan"
    assert main.container_netns("zwave") == "containers/zwave"


def test_netns_names_covers_both_scopes_in_order() -> None:
    t = _turnip(
        {"zwave": {}, "hass": {}},
        {"lan": {"gateway": "10.0.0.1", "gateway_if": "gw0"}},
    )
    # every declared container, then every network's router
    assert main.netns_names(t) == ["containers/zwave", "containers/hass", "routers/lan"]


def test_links_only_container_still_gets_a_netns() -> None:
    # a container with no network attachment is still in the `containers` map, so
    # it still needs its own netns (its links land there in milestone 5)
    t = _turnip(
        {"box": {"links": [{"type": "phys", "dev": "x", "name": "eth0", "address": "1.2.3.4/24"}]}},
        {},
    )
    assert main.netns_names(t) == ["containers/box"]


def test_router_if_derives_from_container() -> None:
    assert main.router_if("zwave") == "vethR-zwave"


def test_router_if_rejects_overlong_name() -> None:
    # "vethR-" (6) + a 10+ char container overflows IFNAMSIZ (15)
    with pytest.raises(ValueError, match="IFNAMSIZ"):
        main.router_if("a-very-long-container-name")
