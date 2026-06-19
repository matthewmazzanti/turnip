"""Black-box probes for turnip integration tests.

These observe the LIVE system the way an operator would -- `ip -j` for addresses
and routes, real TCP connects for reachability, `nft` for the policy table -- and
compare it against EXPLICIT, hand-authored expectations supplied by each scenario.

They deliberately do NOT re-derive expectations from turnip's own model. A check
that lowers from `build_model` the way `up` does is an identity: it can only prove
"up applied what the model produced", and a lowering bug corrupts both sides
identically, so it sees nothing. The scenarios instead state the expected network
as known external ground truth ("build this config; it must have these
properties") -- and reachability is the backbone, because reachability is
behaviour, not a restatement of config.

A container's netns is owned by podman's rootless user namespace, so entering one
means going through podman's mount ns -- we shell to `podman unshare nsenter` as
the run user, exactly the way run-container.sh joins a container to its netns.
"""

from __future__ import annotations

import json
import os
import subprocess
import sys
import time
from collections.abc import Iterator
from contextlib import contextmanager, suppress

# A socket server that accepts (and immediately drops) connections until a deadline,
# then exits -- a live listener so a DENIED probe proves the SYN was dropped at the
# router (timeout), not merely refused (no listener). Self-expiring so a missed kill
# can't leak it.
_LISTEN = """
import socket, sys, time
port = int(sys.argv[1]); deadline = time.time() + float(sys.argv[2])
s = socket.socket(); s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
s.bind(("0.0.0.0", port)); s.listen(); s.settimeout(0.3)
while time.time() < deadline:
    try:
        conn, _ = s.accept(); conn.close()
    except OSError:
        pass
"""

_CONNECT = """
import socket, sys
try:
    socket.create_connection((sys.argv[1], int(sys.argv[2])), timeout=float(sys.argv[3])).close()
except OSError:
    sys.exit(1)
"""


class Probe:
    """Live introspection + reachability inside turnip's container netns.

    `uid`/`user` identify the rootless-podman owner whose runtime dir holds the
    state (`/run/user/<uid>/turnip/...`); entering a netns is routed through that
    user's podman mount ns. Methods take explicit expected values from the caller.
    """

    def __init__(self, user: str = "homelab", uid: int = 1001) -> None:
        self.user = user
        self.uid = uid

    def _netns(self, container: str) -> str:
        return f"/run/user/{self.uid}/turnip/containers/{container}/netns"

    def _wrap(self, container: str, argv: list[str]) -> list[str]:
        """argv to run INSIDE `container`'s netns, via podman's mount+user ns. As root
        we sudo to the owner first (podman unshare must run as the rootless user)."""
        inner = ["podman", "unshare", "nsenter", f"--net={self._netns(container)}", *argv]
        if os.geteuid() == 0:
            return [
                "sudo", "-u", self.user,
                "env", f"XDG_RUNTIME_DIR=/run/user/{self.uid}", f"HOME=/home/{self.user}",
                *inner,
            ]
        return inner

    def _run(self, container: str, argv: list[str]) -> subprocess.CompletedProcess[str]:
        return subprocess.run(
            self._wrap(container, argv), cwd="/tmp", text=True, capture_output=True
        )

    # --- structural introspection (compared to literal, hand-authored values) ---

    def addrs(self, container: str, ifname: str) -> set[str]:
        """The set of `ip/prefixlen` IPv4 addresses on `ifname` in `container`."""
        cp = self._run(container, ["ip", "-j", "addr", "show", "dev", ifname])
        if cp.returncode != 0:
            return set()
        out: set[str] = set()
        for link in json.loads(cp.stdout):
            for a in link.get("addr_info", []):
                if a.get("family") == "inet":
                    out.add(f"{a['local']}/{a['prefixlen']}")
        return out

    def iface_exists(self, container: str, ifname: str) -> bool:
        return self._run(container, ["ip", "link", "show", "dev", ifname]).returncode == 0

    def has_default_via(self, container: str, gateway: str) -> bool:
        """True if `container` has a default route via `gateway`."""
        cp = self._run(container, ["ip", "-j", "route"])
        if cp.returncode != 0:
            return False
        routes = json.loads(cp.stdout)
        return any(r.get("dst") == "default" and r.get("gateway") == gateway for r in routes)

    # --- reachability (the external property a lowering bug cannot fake) ---

    @contextmanager
    def listener(self, container: str, port: int, seconds: float = 15) -> Iterator[None]:
        """Run a TCP listener on `port` inside `container` for the block's duration."""
        proc = subprocess.Popen(
            self._wrap(container, ["python3", "-c", _LISTEN, str(port), str(seconds)]),
            cwd="/tmp", start_new_session=True,
        )
        try:
            time.sleep(1.0)  # let the bind/listen settle before any connect
            yield
        finally:
            with suppress(ProcessLookupError):
                os.killpg(os.getpgid(proc.pid), 9)
            proc.wait()

    def connects(self, src: str, dst_ip: str, port: int, timeout: float = 2.0) -> bool:
        """True if a TCP connect from inside `src` to `dst_ip:port` succeeds."""
        cp = self._run(src, ["python3", "-c", _CONNECT, dst_ip, str(port), str(timeout)])
        return cp.returncode == 0


class Checks:
    """A tiny pass/fail accumulator -- prints each result, exits nonzero if any failed
    (so a scenario is a process the test driver runs and checks the exit code of)."""

    def __init__(self) -> None:
        self.failed: list[str] = []

    def ok(self, condition: bool, label: str) -> None:  # noqa: FBT001
        print(f"  [{'PASS' if condition else 'FAIL'}] {label}")
        if not condition:
            self.failed.append(label)

    def done(self) -> None:
        if self.failed:
            print(f"\n{len(self.failed)} check(s) FAILED:")
            for f in self.failed:
                print(f"  - {f}")
            sys.exit(1)
        print("\nall checks passed")
