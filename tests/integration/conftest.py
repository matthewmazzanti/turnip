"""Integration-test fixtures + gating.

Integration tests shell the `turnip` CLI on PATH (black-box) and probe the live system,
so they only run in a rootful env with rootless podman. They are SKIPPED unless
TURNIP_INTEGRATION is set -- a bare host `uv run pytest` runs only the pure unit tests
and shows these (in tests/integration/) as skipped.

Everything runs on ONE host: the `world` fixture provisions a peer in its own netns
(not a second machine) for the uplink + LAN-link scenarios -- a netns peer exercises the
same kernel forwarding/NAT/bridge paths as a separate box.
"""

from __future__ import annotations

import json
import os
import subprocess
import tempfile
import time
from collections.abc import Callable, Generator
from contextlib import contextmanager
from pathlib import Path

import pytest

HERE = Path(__file__).parent

# every config runs as the rootless-podman owner; default it so the inline configs in
# the tests stay focused on the network, not boilerplate (a config may still override).
_DEFAULT_RUNTIME = {"user": "homelab"}


def pytest_collection_modifyitems(config: pytest.Config, items: list[pytest.Item]) -> None:
    """Skip everything under tests/integration/ unless TURNIP_INTEGRATION is set."""
    if os.environ.get("TURNIP_INTEGRATION"):
        return
    skip = pytest.mark.skip(reason="set TURNIP_INTEGRATION (live rootful env) to run")
    for item in items:
        if str(item.fspath).startswith(str(HERE)):
            item.add_marker(skip)


def _materialize(config: dict[str, object]) -> str:
    """Write a config dict to a temp JSON file and return its path (turnip's CLI reads a
    file via TURNIP_CONFIG). `runtime` defaults to the rootless owner unless overridden."""
    full = {"runtime": _DEFAULT_RUNTIME, **config}
    fd, path = tempfile.mkstemp(suffix=".json", prefix="turnip-test-")
    with os.fdopen(fd, "w") as f:
        json.dump(full, f)
    return path


def _turnip(action: str, path: str) -> subprocess.CompletedProcess[str]:
    env = {**os.environ, "TURNIP_CONFIG": path}
    return subprocess.run(["turnip", action], env=env, capture_output=True, text=True)


@pytest.fixture
def turnip() -> Callable[[dict[str, object]], object]:
    """`with turnip({...}):` materializes the config, brings the network up, and ALWAYS
    tears it down (a failing assertion in the body can't leak a live netns -- teardown is
    in a finally)."""

    @contextmanager
    def _up_down(config: dict[str, object]) -> Generator[None]:
        path = _materialize(config)
        try:
            up = _turnip("up", path)
            assert up.returncode == 0, f"turnip up failed:\n{up.stdout}\n{up.stderr}"
            try:
                yield
            finally:
                _turnip("down", path)
        finally:
            os.unlink(path)

    return _up_down


@pytest.fixture
def turnip_attempt() -> Callable[[dict[str, object]], int]:
    """`turnip up` for a config expected to FAIL, then always `down`; return the up return
    code (for the negative / validation-reject tests)."""

    def _attempt(config: dict[str, object]) -> int:
        path = _materialize(config)
        try:
            rc = _turnip("up", path).returncode
            _turnip("down", path)  # idempotent; a no-op when up failed before building
            return rc
        finally:
            os.unlink(path)

    return _attempt


@pytest.fixture
def anchors() -> Callable[[list[tuple[str, str]]], None]:
    """Create each borrowed link anchor if absent (idempotent) -- a host bridge / dummy
    NIC. Self-contained on any rootful host; reaped with the host on teardown."""

    def _ensure(specs: list[tuple[str, str]]) -> None:
        for kind, name in specs:
            subprocess.run(["ip", "link", "add", name, "type", kind], capture_output=True)
            subprocess.run(["ip", "link", "set", name, "up"], capture_output=True)

    return _ensure


# --- the `world` peer (an in-host netns, not a second machine) ---------------
# Reachable three ways, one veth per role: via the host uplink (w-up, an L3 subnet the
# host routes + masquerades to / DNATs from), and as the macvlan / ipvlan parents (mv-par
# / iv-par -- L2 only, the child talks straight to world over the veth).

_SEGMENTS = [
    # (host_veth, world_veth, world_cidr, host_cidr | None)
    ("w-up", "w-up-p", "198.51.100.2/24", "198.51.100.1/24"),  # uplink egress/ingress
    ("mv-par", "mv-par-p", "192.168.1.2/24", None),  # macvlan parent LAN
    ("iv-par", "iv-par-p", "192.168.2.2/24", None),  # ipvlan parent LAN
]


class World:
    """Handle to the peer netns. `ip` is its uplink-facing address (the egress target);
    `connects()` originates a TCP connect FROM world (the external client for ingress)."""

    ip = "198.51.100.2"
    host_uplink_ip = "198.51.100.1"

    def connects(self, dst_ip: str, port: int, timeout: float = 3.0) -> bool:
        cp = subprocess.run(
            ["ip", "netns", "exec", "world", "python3", str(HERE / "_connect.py"),
             dst_ip, str(port), str(timeout)],
            capture_output=True,
        )
        return cp.returncode == 0


@pytest.fixture
def world() -> Generator[World]:
    subprocess.run(["ip", "netns", "del", "world"], capture_output=True)  # clear any stale peer
    subprocess.run(["ip", "netns", "add", "world"], check=True)
    listener = None
    try:
        for host_v, world_v, world_cidr, host_cidr in _SEGMENTS:
            subprocess.run(
                ["ip", "link", "add", host_v, "type", "veth", "peer", "name", world_v,
                 "netns", "world"], check=True,
            )
            subprocess.run(["ip", "link", "set", host_v, "up"], check=True)
            subprocess.run(["ip", "-n", "world", "link", "set", world_v, "up"], check=True)
            subprocess.run(
                ["ip", "-n", "world", "addr", "add", world_cidr, "dev", world_v], check=True
            )
            if host_cidr:
                subprocess.run(["ip", "addr", "add", host_cidr, "dev", host_v], check=True)
        subprocess.run(["ip", "-n", "world", "link", "set", "lo", "up"], check=True)
        # a listener on :8888 (all of world's addresses) -- the egress + LAN-link target.
        listener = subprocess.Popen(
            ["ip", "netns", "exec", "world", "python3", str(HERE / "_serve.py"), "8888"]
        )
        time.sleep(1)
        yield World()
    finally:
        if listener is not None:
            listener.kill()
        subprocess.run(["ip", "netns", "del", "world"], capture_output=True)  # reaps the veths
