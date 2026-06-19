"""Integration-test gating + fixtures, shared by the dev-VM `just itest` loop and the
hermetic NixOS checks.

Integration tests shell the `turnip` CLI on PATH (black-box) and probe the live system,
so they only run in a rootful env with rootless podman. They are SKIPPED unless
TURNIP_INTEGRATION is set -- a bare host `uv run pytest` runs only the pure unit tests
and shows these as skipped. Scenarios that need extra resources self-skip too:
TURNIP_WORLD (a peer node) and TURNIP_TEST_IMAGE (a loaded OCI image).
"""

from __future__ import annotations

import os
import subprocess
from collections.abc import Callable, Generator
from contextlib import contextmanager
from pathlib import Path

import pytest

CONFIGS = Path(__file__).parent / "configs"

# marker -> (env var that enables it, human description)
_GATES = {
    "integration": ("TURNIP_INTEGRATION", "a live rootful env"),
    "needs_world": ("TURNIP_WORLD", "a peer `world` node"),
    "needs_image": ("TURNIP_TEST_IMAGE", "a loaded OCI image"),
}


def pytest_configure(config: pytest.Config) -> None:
    # registered here (not pyproject) so `pytest <dir>` works standalone in a node.
    for mark, (_env, desc) in _GATES.items():
        config.addinivalue_line("markers", f"{mark}: requires {desc}")


def pytest_collection_modifyitems(config: pytest.Config, items: list[pytest.Item]) -> None:
    for item in items:
        for mark, (env, desc) in _GATES.items():
            if mark in item.keywords and not os.environ.get(env):
                item.add_marker(pytest.mark.skip(reason=f"set {env} ({desc}) to run"))


def _turnip(action: str, config: str) -> subprocess.CompletedProcess[str]:
    env = {**os.environ, "TURNIP_CONFIG": str(CONFIGS / config)}
    return subprocess.run(["turnip", action], env=env, capture_output=True, text=True)


@pytest.fixture
def turnip() -> Callable[[str], object]:
    """Factory: `with turnip("net.json"):` brings the network up and ALWAYS tears it down
    (teardown in a finally -- a failing assertion can't leak a live netns)."""

    @contextmanager
    def _up_down(config: str) -> Generator[None]:
        up = _turnip("up", config)
        assert up.returncode == 0, f"turnip up failed:\n{up.stdout}\n{up.stderr}"
        try:
            yield
        finally:
            _turnip("down", config)

    return _up_down


@pytest.fixture
def turnip_attempt() -> Callable[[str], int]:
    """Run `turnip up` for a config expected to FAIL, then always `down`; return the up
    return code. For the negative (validation-reject) scenarios."""

    def _attempt(config: str) -> int:
        rc = _turnip("up", config).returncode
        _turnip("down", config)  # idempotent; a no-op when up failed before building
        return rc

    return _attempt


@pytest.fixture
def ensure_anchors() -> Callable[[list[tuple[str, str]]], None]:
    """Create each borrowed link anchor if absent (idempotent) -- a host bridge / a dummy
    NIC. Self-contained on the dev VM; a no-op on a NixOS node that already has them."""

    def _ensure(anchors: list[tuple[str, str]]) -> None:
        for kind, name in anchors:
            subprocess.run(["ip", "link", "add", name, "type", kind], capture_output=True)
            subprocess.run(["ip", "link", "set", name, "up"], capture_output=True)

    return _ensure
