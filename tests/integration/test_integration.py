"""Integration tests -- one pytest test per registered scenario (the registry in
scenarios.py is the single source). Gated by the `integration` marker (skipped unless
TURNIP_INTEGRATION; see conftest.py), so this collects but no-ops on a bare host.

Run them: `just itest` (dev VM) or `pytest -m integration` inside the NixOS nodes.
"""

from __future__ import annotations

import os
import subprocess
from collections.abc import Callable
from contextlib import AbstractContextManager

import pytest
from probe import Probe
from scenarios import NEGATIVES, SCENARIOS, Scenario

pytestmark = pytest.mark.integration

_SCENARIO_PARAMS = [pytest.param(s, id=s.name, marks=list(s.marks)) for s in SCENARIOS]


@pytest.mark.parametrize("scenario", _SCENARIO_PARAMS)
def test_scenario(
    scenario: Scenario,
    turnip: Callable[[str], AbstractContextManager[None]],
    ensure_anchors: Callable[[list[tuple[str, str]]], None],
) -> None:
    ensure_anchors(scenario.anchors)
    with turnip(scenario.config):
        scenario.check(Probe())


@pytest.mark.parametrize("config", [c for _, c in NEGATIVES], ids=[n for n, _ in NEGATIVES])
def test_config_rejected(config: str, turnip_attempt: Callable[[str], int]) -> None:
    # turnip up must fail fast (validate_link_anchors) before building any netns.
    assert turnip_attempt(config) != 0


@pytest.mark.needs_image
def test_podman_attach(turnip: Callable[[str], AbstractContextManager[None]]) -> None:
    """A real podman container joins a turnip netns via run-container.sh and resolves a
    peer BY NAME through the generated /etc/hosts; the denied peer is still dropped."""
    image = os.environ["TURNIP_TEST_IMAGE"]
    tconnect = os.environ["TURNIP_TCONNECT"]
    runc = os.environ["TURNIP_RUNCONTAINER"]
    probe = Probe()
    with turnip("router.json"), probe.listener("hass", 443), probe.listener("proxy", 443):
        allowed = subprocess.run(["bash", runc, "zwave", image, "--", tconnect, "hass", "443"])
        assert allowed.returncode == 0, "zwave container should reach hass BY NAME (hosts + flow)"
        denied = subprocess.run(["bash", runc, "zwave", image, "--", tconnect, "10.0.0.13", "443"])
        assert denied.returncode != 0, "denied peer (proxy) must be dropped from a real container"
