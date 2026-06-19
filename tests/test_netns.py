"""Tests for the host<->podman fd bridge -- pure fork + SCM_RIGHTS, no namespaces.

The podman/netns integration (a child entering podman's ns and creating router netns)
is exercised by the VM probe; here we test only the transport: a child produces fds,
the parent collects them by name, and they stay valid after the child exits.
"""

from __future__ import annotations

import os

import pytest

from turnip import netns


def test_collect_fds_round_trips_and_outlives_child() -> None:
    def produce() -> dict[str, int]:
        # distinct fds opened in the child; the parent should receive usable dups
        return {"a": os.open(os.devnull, os.O_RDONLY), "b": os.open(os.devnull, os.O_RDONLY)}

    fds = netns.collect_fds_from_child(produce)
    try:
        assert set(fds) == {"a", "b"}
        assert fds["a"] != fds["b"]
        for fd in fds.values():
            os.fstat(fd)  # valid in the parent after the child exited (else OSError)
    finally:
        for fd in fds.values():
            os.close(fd)


def test_collect_fds_preserves_order_and_names() -> None:
    names = ["lan", "dmz", "mgmt"]

    def produce() -> dict[str, int]:
        return {n: os.open(os.devnull, os.O_RDONLY) for n in names}

    fds = netns.collect_fds_from_child(produce)
    try:
        assert list(fds) == names  # names round-trip, each paired with its own fd
    finally:
        for fd in fds.values():
            os.close(fd)


def test_collect_fds_propagates_child_failure() -> None:
    def boom() -> dict[str, int]:
        raise RuntimeError("produce blew up")

    with pytest.raises(RuntimeError, match="child failed"):
        netns.collect_fds_from_child(boom)
