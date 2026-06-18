"""Environment smoke test: the package and its third-party deps import cleanly.

No namespaces, no privilege -- just proof the environment is wired (deps present,
the src/ layout importable), so a broken venv or a missing dependency fails here,
fast and legibly, before any feature test runs. Mirrors what the VM's pyEnv must
also satisfy to run `turnip`.
"""

from __future__ import annotations

import importlib

import pytest

# Third-party runtime deps: the config model + the netlink layer.
THIRD_PARTY = ["pydantic", "pyroute2"]

# Every turnip module, so an import-time error (bad syntax, missing symbol) in any
# of them surfaces here rather than deep inside a feature test.
TURNIP_MODULES = [
    "turnip",
    "turnip.config",
    "turnip.nftlib",
    "turnip.netns",
    "turnip.main",
]


@pytest.mark.parametrize("name", THIRD_PARTY)
def test_third_party_import(name: str) -> None:
    importlib.import_module(name)


@pytest.mark.parametrize("name", TURNIP_MODULES)
def test_turnip_import(name: str) -> None:
    importlib.import_module(name)
