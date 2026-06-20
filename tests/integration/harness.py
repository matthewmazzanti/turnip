"""Importable integration harness: the `Probe` (black-box observation) plus the drivers
the tests call directly -- `turnip`/`turnip_attempt` (run the CLI on a config dict),
`ensure_anchors` (borrowed link anchors), and `world` (an in-host peer netns). Plain
functions / contextmanagers, no pytest -- so they're explicit at the call site and
usable in a REPL or scratch script too. The only pytest-specific bit (skip the live
tests unless TURNIP_INTEGRATION) stays in conftest.py.

Black-box rule: expectations are HAND-AUTHORED by each test, never derived from turnip's
`build_model` (that would be an identity check -- a lowering bug would corrupt both sides
identically). Reachability (real TCP connects vs live listeners) is the backbone.
"""

from __future__ import annotations

import json
import os
import subprocess
import tempfile
import time
from collections.abc import Generator, Mapping
from contextlib import contextmanager, suppress
from typing import NamedTuple

# Two tiny programs kept as SOURCE so they run identically whether handed to `python3 -c`
# inside a container netns (podman unshare nsenter) or a world netns (ip netns exec) --
# no file path to resolve. A live listener makes a DENIED probe a timeout (SYN dropped at
# the router), not a refusal (no listener); it self-expires so a missed kill can't leak.
_LISTEN = """
import socket, sys, time
port = int(sys.argv[1])
deadline = time.time() + (float(sys.argv[2]) if len(sys.argv) > 2 else 1e9)
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


def _listen_argv(port: int, seconds: float | None = None) -> list[str]:
    extra = [str(seconds)] if seconds is not None else []
    return ["python3", "-c", _LISTEN, str(port), *extra]


def connect_argv(dst_ip: str, port: int, timeout: float) -> list[str]:
    return ["python3", "-c", _CONNECT, str(dst_ip), str(port), str(timeout)]


# --- driving the turnip CLI on an in-test config dict ------------------------


def _materialize(config: Mapping[str, object]) -> str:
    """Write a config dict to a temp JSON file -> its path (turnip's CLI reads a file via
    TURNIP_CONFIG). `runtime` defaults to the rootless owner unless the config overrides."""
    full = {"runtime": {"user": "homelab"}, **config}
    fd, path = tempfile.mkstemp(suffix=".json", prefix="turnip-test-")
    with os.fdopen(fd, "w") as f:
        json.dump(full, f)
    return path


def _turnip(action: str, path: str) -> subprocess.CompletedProcess[str]:
    env = {**os.environ, "TURNIP_CONFIG": path}
    return subprocess.run(["turnip", action], env=env, capture_output=True, text=True)


@contextmanager
def turnip(config: Mapping[str, object]) -> Generator[None]:
    """`with turnip({...}):` materializes the config, brings the network up, and ALWAYS
    tears it down (a failing assertion in the body can't leak a live netns)."""
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


def turnip_attempt(config: Mapping[str, object]) -> int:
    """`turnip up` for a config expected to FAIL, then always `down`; return the up return
    code (for the negative / validation-reject tests)."""
    path = _materialize(config)
    try:
        rc = _turnip("up", path).returncode
        _turnip("down", path)  # idempotent; a no-op when up failed before building
        return rc
    finally:
        os.unlink(path)


def ensure_anchors(specs: list[tuple[str, str]]) -> None:
    """Create each borrowed link anchor if absent (idempotent) -- a host bridge / dummy
    NIC. Self-contained on any rootful host; reaped with the host on teardown."""
    for kind, name in specs:
        subprocess.run(["ip", "link", "add", name, "type", kind], capture_output=True)
        subprocess.run(["ip", "link", "set", name, "up"], capture_output=True)


# --- black-box observation ---------------------------------------------------


class Probe:
    """Live introspection + reachability inside turnip's container netns. `uid`/`user`
    identify the rootless-podman owner; entering a netns is routed through that user's
    podman mount ns. Methods take explicit expected values from the caller."""

    def __init__(self, user: str = "homelab", uid: int = 1001) -> None:
        self.user = user
        self.uid = uid

    def _netns(self, container: str) -> str:
        return f"/run/user/{self.uid}/turnip/containers/{container}/netns"

    def _wrap(self, container: str, argv: list[str]) -> list[str]:
        """argv to run INSIDE `container`'s netns, via podman's mount+user ns. As root we
        sudo to the owner first (podman unshare must run as the rootless user)."""
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

    @staticmethod
    def _addrs(stdout: str) -> set[str]:
        """Parse `ip -j addr` JSON -> the set of `ip/prefixlen` IPv4 addresses."""
        out: set[str] = set()
        for link in json.loads(stdout or "[]"):
            for a in link.get("addr_info", []):
                if a.get("family") == "inet":
                    out.add(f"{a['local']}/{a['prefixlen']}")
        return out

    # structural introspection inside a container (vs literal expectations)

    def addrs(self, container: str, ifname: str) -> set[str]:
        cp = self._run(container, ["ip", "-j", "addr", "show", "dev", ifname])
        return self._addrs(cp.stdout) if cp.returncode == 0 else set()

    def iface_exists(self, container: str, ifname: str) -> bool:
        return self._run(container, ["ip", "link", "show", "dev", ifname]).returncode == 0

    def link_kind(self, container: str, ifname: str) -> str | None:
        """IFLA_INFO_KIND of `ifname` ('macvlan'/'ipvlan'/...) -- assert a link's type."""
        cp = self._run(container, ["ip", "-d", "-j", "link", "show", "dev", ifname])
        if cp.returncode != 0:
            return None
        return json.loads(cp.stdout)[0].get("linkinfo", {}).get("info_kind")

    def has_default_via(self, container: str, gateway: str) -> bool:
        cp = self._run(container, ["ip", "-j", "route"])
        if cp.returncode != 0:
            return False
        routes = json.loads(cp.stdout)
        return any(r.get("dst") == "default" and r.get("gateway") == gateway for r in routes)

    # the host INIT netns (no podman-unshare): a phys device moved out of init, a
    # veth->host host end, the uplink host end.

    def init_iface_exists(self, ifname: str) -> bool:
        cp = subprocess.run(["ip", "link", "show", "dev", ifname], capture_output=True)
        return cp.returncode == 0

    def init_addrs(self, ifname: str) -> set[str]:
        cp = subprocess.run(
            ["ip", "-j", "addr", "show", "dev", ifname], text=True, capture_output=True
        )
        return self._addrs(cp.stdout) if cp.returncode == 0 else set()

    # reachability (the external property a lowering bug cannot fake)

    @contextmanager
    def listener(self, container: str, port: int, seconds: float = 15) -> Generator[None]:
        """Run a TCP listener on `port` inside `container` for the block's duration."""
        proc = subprocess.Popen(
            self._wrap(container, _listen_argv(port, seconds)), cwd="/tmp", start_new_session=True
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
        return self._run(src, connect_argv(dst_ip, port, timeout)).returncode == 0


# --- the `world` peer (an in-host netns, not a second machine) ---------------
# A netns peer exercises the same kernel forwarding/NAT/bridge paths as a separate
# machine. Each test declares the segments it needs (no shared global topology).


class Seg(NamedTuple):
    """One interface on the `world` peer: a host-side veth `host_if`, the peer's address
    `world_cidr`, and an optional `host_cidr`. Set host_cidr for a ROUTED segment the host
    routes/NATs to (the uplink peer); leave it None for an L2-only macvlan/ipvlan parent
    (the child talks straight to world over the veth)."""

    host_if: str
    world_cidr: str
    host_cidr: str | None = None


class WorldHandle:
    """`connects()` originates a TCP connect FROM the world peer -- the external client for
    the ingress-DNAT test."""

    def connects(self, dst_ip: str, port: int, timeout: float = 3.0) -> bool:
        argv = ["ip", "netns", "exec", "world", *connect_argv(dst_ip, port, timeout)]
        return subprocess.run(argv, capture_output=True).returncode == 0


@contextmanager
def world(*segments: Seg) -> Generator[WorldHandle]:
    """`with world(Seg(...), ...) as w:` provisions the peer netns with the given segments
    (+ a listener on :8888) and tears it all down. Use it OUTSIDE turnip():
    `with world(...) as w, turnip(cfg):` -- world up first, turnip down first."""
    subprocess.run(["ip", "netns", "del", "world"], capture_output=True)  # clear any stale peer
    subprocess.run(["ip", "netns", "add", "world"], check=True)
    listener = None
    try:
        for seg in segments:
            peer = f"{seg.host_if}-p"
            subprocess.run(
                ["ip", "link", "add", seg.host_if, "type", "veth", "peer", "name", peer,
                 "netns", "world"], check=True,
            )
            subprocess.run(["ip", "link", "set", seg.host_if, "up"], check=True)
            subprocess.run(["ip", "-n", "world", "link", "set", peer, "up"], check=True)
            subprocess.run(
                ["ip", "-n", "world", "addr", "add", seg.world_cidr, "dev", peer], check=True
            )
            if seg.host_cidr:
                subprocess.run(["ip", "addr", "add", seg.host_cidr, "dev", seg.host_if], check=True)
        subprocess.run(["ip", "-n", "world", "link", "set", "lo", "up"], check=True)
        listener = subprocess.Popen(["ip", "netns", "exec", "world", *_listen_argv(8888)])
        time.sleep(1)
        yield WorldHandle()
    finally:
        if listener is not None:
            listener.kill()
        subprocess.run(["ip", "netns", "del", "world"], capture_output=True)  # reaps the veths
