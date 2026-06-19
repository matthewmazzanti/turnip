"""Tests for main.py's pure build step: config -> runtime model.

No namespaces and no IO here -- build_model() produces the Container/Network/
Endpoint objects (paths, names, ips) with handles unbound (.netns is None). The
live wiring is exercised by the integration smoke, not here.
"""

from __future__ import annotations

import pwd
from pathlib import Path
from typing import Any

import pytest

from turnip import main
from turnip.config import Runtime, Turnip

STATE = Path("/n")


def _turnip(containers: dict[str, Any], networks: dict[str, Any]) -> Turnip:
    return Turnip.model_validate({"containers": containers, "networks": networks})


def _model(containers: dict[str, Any], networks: dict[str, Any]) -> main.Model:
    return main.build_model(_turnip(containers, networks), STATE)


def test_build_model_state_layout() -> None:
    model = _model(
        {"zwave": {}, "hass": {}},
        {"lan": {"gateway": "10.0.0.1", "gateway_if": "gw0"}},
    )
    # container state dir holds {netns, hosts}; router is a bare netns
    zwave = model.containers[0]
    assert zwave.netns_path == "/n/containers/zwave/netns"
    assert zwave.hosts_path == "/n/containers/zwave/hosts"
    assert zwave.state_dir == "/n/containers/zwave"
    assert [n.netns_path for n in model.networks] == ["/n/routers/lan"]
    # nodes = containers first, then routers; all unbound at build time
    assert [node.name for node in model.nodes] == ["zwave", "hass", "lan"]
    assert all(node.netns is None for node in model.nodes)


def test_build_model_lowers_endpoints() -> None:
    model = _model(
        {"zwave": {}, "hass": {}},
        {
            "lan": {
                "gateway": "10.0.0.1",
                "gateway_if": "gw0",
                "attach": {
                    "zwave": {"ip": "10.0.0.11", "interface": "eth0"},
                    "hass": {"ip": "10.0.0.12", "interface": "eth0"},
                },
            }
        },
    )
    net = model.networks[0]
    assert net.gateway == "10.0.0.1" and net.gateway_if == "gw0"
    eps = {ep.container.name: ep for ep in net.endpoints}
    assert eps["zwave"].ip == "10.0.0.11"
    assert eps["zwave"].router_if == "vethR-zwave"
    assert eps["zwave"].cont_if == "eth0"
    # the endpoint references the shared Container object (one netns per container)
    assert eps["zwave"].container is next(c for c in model.containers if c.name == "zwave")


def test_links_only_container_still_gets_a_netns() -> None:
    # a container with no network attachment is still in the `containers` map, so it
    # still gets its netns (its links land there in milestone 5)
    model = _model(
        {"box": {"links": [{"type": "phys", "dev": "x", "name": "eth0", "address": "1.2.3.4/24"}]}},
        {},
    )
    assert [c.name for c in model.containers] == ["box"]
    assert model.containers[0].netns_path == "/n/containers/box/netns"
    assert model.networks == []


def test_handle_raises_when_unbound() -> None:
    model = _model({"box": {}}, {})
    with pytest.raises(RuntimeError, match="not bound"):
        _ = model.containers[0].handle


def _hub_spoke() -> main.Model:
    # zwave -> hass, hass -> proxy (directional). proxy initiates to nobody.
    return _model(
        {"zwave": {}, "hass": {}, "proxy": {}},
        {
            "lan": {
                "gateway": "10.0.0.1",
                "gateway_if": "gw0",
                "attach": {
                    "zwave": {"ip": "10.0.0.11", "interface": "eth0"},
                    "hass": {"ip": "10.0.0.12", "interface": "eth0"},
                    "proxy": {"ip": "10.0.0.13", "interface": "eth0"},
                },
                "flows": [
                    {"from": "zwave", "to": "hass", "proto": "tcp", "port": 443},
                    {"from": "hass", "to": "proxy", "proto": "tcp", "port": 443},
                ],
            }
        },
    )


def _by_name(model: main.Model, name: str) -> main.Container:
    return next(c for c in model.containers if c.name == name)


def test_container_peers_are_directional() -> None:
    model = _hub_spoke()
    # peers = outbound-flow targets only (the reverse view, directional)
    assert main.container_peers(model, _by_name(model, "zwave")) == {"hass": "10.0.0.12"}
    assert main.container_peers(model, _by_name(model, "hass")) == {"proxy": "10.0.0.13"}
    assert main.container_peers(model, _by_name(model, "proxy")) == {}  # initiates to nobody


def test_hosts_file_has_self_and_peers() -> None:
    model = _hub_spoke()
    hosts = main.hosts_file(model, _by_name(model, "zwave"))
    assert hosts.splitlines() == [
        "127.0.0.1 localhost",
        "10.0.0.11 zwave",  # self
        "10.0.0.12 hass",  # reachable peer
    ]
    # proxy reaches no one -> just localhost + self
    assert main.hosts_file(model, _by_name(model, "proxy")).splitlines() == [
        "127.0.0.1 localhost",
        "10.0.0.13 proxy",
    ]


def test_router_if_derives_from_container() -> None:
    assert main.router_if("zwave") == "vethR-zwave"


def test_router_if_rejects_overlong_name() -> None:
    # "vethR-" (6) + a 10+ char container overflows IFNAMSIZ (15)
    with pytest.raises(ValueError, match="IFNAMSIZ"):
        main.router_if("a-very-long-container-name")


# --- resolve_runtime: user/uid/dir resolution (sudo-aware) -----------------

_USERS = {  # name -> (uid, gid, home)
    "alice": (1000, 1000, "/home/alice"),
    "homelab": (1001, 100, "/home/homelab"),
    "root": (0, 0, "/root"),
}


def _pw(name: str) -> pwd.struct_passwd:
    uid, gid, home = _USERS[name]
    return pwd.struct_passwd((name, "x", uid, gid, "", home, "/bin/sh"))


def _resolve(
    monkeypatch: pytest.MonkeyPatch,
    *,
    euid: int,
    login_uid: int = 1000,
    sudo_user: str | None = None,
    **rt: Any,
) -> main.ResolvedRuntime:
    def _getpwnam(name: str) -> pwd.struct_passwd:
        return _pw(name)

    def _getpwuid(uid: int) -> pwd.struct_passwd:
        return next(_pw(n) for n, v in _USERS.items() if v[0] == uid)

    monkeypatch.setattr(main.os, "geteuid", lambda: euid)
    monkeypatch.setattr(main.os, "getuid", lambda: login_uid)
    monkeypatch.setattr(main.pwd, "getpwnam", _getpwnam)
    monkeypatch.setattr(main.pwd, "getpwuid", _getpwuid)
    if sudo_user is None:
        monkeypatch.delenv("SUDO_USER", raising=False)
    else:
        monkeypatch.setenv("SUDO_USER", sudo_user)
    return main.resolve_runtime(Runtime(**rt))


def test_resolve_rootless_falls_back_to_login_user(monkeypatch: pytest.MonkeyPatch) -> None:
    rr = _resolve(monkeypatch, euid=1000)  # not privileged -> current login user (alice)
    assert (rr.user, rr.uid, rr.gid) == ("alice", 1000, 1000)
    assert rr.state_dir == Path("/run/user/1000/turnip")


def test_resolve_explicit_user_wins(monkeypatch: pytest.MonkeyPatch) -> None:
    rr = _resolve(monkeypatch, euid=1000, user="homelab")
    assert rr.user == "homelab" and rr.uid == 1001
    assert rr.state_dir == Path("/run/user/1001/turnip")


def test_resolve_root_requires_explicit_user(monkeypatch: pytest.MonkeyPatch) -> None:
    # euid 0, no runtime.user, no $SUDO_USER -> refuse to fall back to root
    with pytest.raises(ValueError, match="set runtime.user"):
        _resolve(monkeypatch, euid=0)


def test_resolve_root_uses_config_user(monkeypatch: pytest.MonkeyPatch) -> None:
    rr = _resolve(monkeypatch, euid=0, user="homelab")
    assert rr.user == "homelab" and rr.uid == 1001


def test_resolve_root_uses_sudo_user(monkeypatch: pytest.MonkeyPatch) -> None:
    rr = _resolve(monkeypatch, euid=0, sudo_user="homelab")
    assert rr.user == "homelab" and rr.uid == 1001


def test_resolve_rejects_root_target(monkeypatch: pytest.MonkeyPatch) -> None:
    with pytest.raises(ValueError, match="must be the unprivileged owner"):
        _resolve(monkeypatch, euid=0, user="root")


def test_resolve_dirs_follow_target_uid_not_env(monkeypatch: pytest.MonkeyPatch) -> None:
    # even as root with XDG_RUNTIME_DIR pointing elsewhere, dirs follow the target uid
    monkeypatch.setenv("XDG_RUNTIME_DIR", "/run/user/0")  # root's -- must be ignored
    rr = _resolve(monkeypatch, euid=0, user="homelab")
    assert rr.state_dir == Path("/run/user/1001/turnip")
    # an explicit state_dir override is still honored
    rr2 = _resolve(monkeypatch, euid=0, user="homelab", state_dir=Path("/custom"))
    assert rr2.state_dir == Path("/custom")
