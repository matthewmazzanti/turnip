"""Integration-test gating. The harness (Probe + the turnip/world/anchors drivers) is
importable from harness.py; the only pytest-specific concern lives here: integration
tests shell the `turnip` CLI and probe the live system, so they are SKIPPED unless
TURNIP_INTEGRATION is set -- a bare host `uv run pytest` runs only the pure unit tests.
"""

from __future__ import annotations

import os
from pathlib import Path

import pytest

HERE = Path(__file__).parent


def pytest_collection_modifyitems(config: pytest.Config, items: list[pytest.Item]) -> None:
    """Skip everything under tests/integration/ unless TURNIP_INTEGRATION is set."""
    if os.environ.get("TURNIP_INTEGRATION"):
        return
    skip = pytest.mark.skip(reason="set TURNIP_INTEGRATION (live rootful env) to run")
    for item in items:
        if str(item.fspath).startswith(str(HERE)):
            item.add_marker(skip)
