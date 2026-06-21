"""Tests for the nftlib DSL and the `inet turnip` flow matrix it builds.

Two layers:
  - render() unit tests -- each node type renders to the libnftables JSON shape,
    and the cross-field validators (Verdict target, Concat arity) fire.
  - a golden snapshot of build_nft()'s rendered output for a representative network
    (the committed `golden/build_nft.json`), plus a check that flows are
    directional (one map entry per flow, from -> to only).
"""

from __future__ import annotations

import json
from pathlib import Path

import pytest

from turnip import nftlib as nft
from turnip.config import Turnip
from turnip.main import Network, build_model, build_nft

GOLDEN = Path(__file__).resolve().parent / "golden" / "build_nft.json"
# Regenerate (only when build_nft is *intended* to change): render
# build_nft(_sample_network()) and write it to golden/build_nft.json -- e.g. run
# test_build_nft_matches_golden's body with `GOLDEN.write_text(...)` substituted.


def _sample_network() -> Network:
    """A representative router network (the runtime model object): zwave/.11,
    hass/.12, proxy/.13, gw 10.0.0.1, hub-and-spoke flows (zwave -> hass,
    hass -> proxy on tcp/443)."""
    turnip = Turnip.model_validate(
        {
            "containers": {"zwave": {}, "hass": {}, "proxy": {}},
            "networks": {
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
        }
    )
    return build_model(turnip, Path("/n")).networks[0]


# --- render(): expression nodes -------------------------------------------


def test_payload_meta_ct_render() -> None:
    assert nft.render(nft.payload("ip", "saddr")) == {
        "payload": {"protocol": "ip", "field": "saddr"}
    }
    assert nft.render(nft.meta("l4proto")) == {"meta": {"key": "l4proto"}}
    assert nft.render(nft.ct("state")) == {"ct": {"key": "state"}}


def test_concat_renders_parts_in_order() -> None:
    c = nft.concat(nft.payload("ip", "saddr"), "tcp", 443)
    assert nft.render(c) == {
        "concat": [{"payload": {"protocol": "ip", "field": "saddr"}}, "tcp", 443]
    }


def test_concat_needs_two_parts() -> None:
    with pytest.raises(ValueError, match="at least two parts"):
        nft.concat(nft.meta("l4proto"))


def test_match_default_and_explicit_op() -> None:
    assert nft.render(nft.match(nft.meta("l4proto"), "icmp")) == {
        "match": {"op": "==", "left": {"meta": {"key": "l4proto"}}, "right": "icmp"}
    }
    assert nft.render(nft.match(nft.meta("mark"), 1, "!="))["match"]["op"] == "!="


def test_ct_state_scalar_vs_list() -> None:
    # one state -> scalar right; many -> list (mirrors nft's own JSON)
    assert nft.render(nft.ct_state("new"))["match"]["right"] == "new"
    assert nft.render(nft.ct_state("established", "related"))["match"] == {
        "op": "in",
        "left": {"ct": {"key": "state"}},
        "right": ["established", "related"],
    }


def test_ct_state_needs_a_state() -> None:
    with pytest.raises(ValueError, match="at least one state"):
        nft.ct_state()


def test_vmap_renders_set_reference() -> None:
    sd = nft.concat(nft.payload("ip", "saddr"), nft.payload("ip", "daddr"))
    assert nft.render(nft.vmap(sd, "allowed_hosts"))["vmap"]["data"] == "@allowed_hosts"


def test_verdict_map_omits_elem_when_empty() -> None:
    # nft rejects `"elem": []` ("Invalid set elem expression"); an empty map must
    # render with no `elem` key (a flow-less network -- uplink egress/ingress only --
    # produces exactly this empty allowed_flows map). A non-empty map keeps `elem`.
    table = nft.Table("inet", "turnip")
    empty = nft.render(table.verdict_map("allowed_flows", ["ipv4_addr"], []))
    assert "elem" not in empty["add"]["map"]
    elems: list[tuple[nft.Expr, nft.Expr]] = [(nft.concat("1.2.3.4", "5.6.7.8"), nft.accept())]
    full = nft.render(table.verdict_map("m", ["ipv4_addr", "ipv4_addr"], elems))
    assert "elem" in full["add"]["map"]


def test_verdicts_render() -> None:
    assert nft.render(nft.accept()) == {"accept": None}
    assert nft.render(nft.drop()) == {"drop": None}
    assert nft.render(nft.jump("forward")) == {"jump": {"target": "forward"}}


def test_verdict_target_cross_field_rules() -> None:
    with pytest.raises(ValueError, match="requires a target"):
        nft.Verdict("jump")  # target omitted
    with pytest.raises(ValueError, match="takes no target"):
        nft.Verdict("accept", "forward")  # target on a non-jump


# --- render(): objects + commands via the Table context -------------------


def test_table_reload_is_add_delete_add() -> None:
    t = nft.Table("inet", "fabric")
    cmds = t.reload()
    assert [type(c).__name__ for c in cmds] == ["Add", "Delete", "Add"]
    assert nft.render(cmds[1]) == {"delete": {"table": {"family": "inet", "name": "fabric"}}}


def test_chain_omits_unset_base_fields() -> None:
    # the Table factories return Add commands; render .obj to inspect the object
    t = nft.Table("inet", "fabric")
    regular = nft.render(t.chain("sub").obj)["chain"]
    assert set(regular) == {"family", "table", "name"}  # no type/hook/prio/policy
    base = nft.render(t.chain("forward", type="filter", hook="forward", prio=0, policy="drop").obj)
    assert base["chain"]["policy"] == "drop"


def test_rule_and_verdict_map_stamp_family_table() -> None:
    t = nft.Table("inet", "fabric")
    r = nft.render(t.rule("forward", nft.ct_state("new"), nft.accept()).obj)["rule"]
    assert r["family"] == "inet" and r["table"] == "fabric" and r["chain"] == "forward"
    m = nft.render(t.verdict_map("m", ["ipv4_addr"], []).obj)["map"]
    assert m["map"] == "verdict" and m["type"] == ["ipv4_addr"]


def test_ruleset_wraps_in_nftables_key() -> None:
    rs = nft.ruleset(nft.Table("inet", "fabric").reload())
    assert list(nft.render(rs)) == ["nftables"]


# --- build_nft: golden snapshot + directional flows ------------------------


def test_build_nft_matches_golden() -> None:
    rendered = nft.render(build_nft(_sample_network()))
    assert rendered == json.loads(GOLDEN.read_text())


def _allowed_flows(network: Network) -> list[list[object]]:
    rendered = nft.render(build_nft(network))
    flows = next(
        command["add"]["map"]
        for command in rendered["nftables"]
        if "add" in command and command["add"].get("map", {}).get("name") == "allowed_flows"
    )
    return [key["concat"] for key, _verdict in flows["elem"]]


def test_build_nft_flows_are_directional() -> None:
    # one map entry per flow, from -> to only -- NO reverse entry (the return path
    # rides ct established/related). 2 flows -> 2 elements.
    elems = _allowed_flows(_sample_network())
    assert elems == [
        ["10.0.0.11", "10.0.0.12", "tcp", 443],  # zwave -> hass
        ["10.0.0.12", "10.0.0.13", "tcp", 443],  # hass -> proxy
    ]
    # the reverses are absent
    assert ["10.0.0.12", "10.0.0.11", "tcp", 443] not in elems  # hass -> zwave
    assert ["10.0.0.13", "10.0.0.12", "tcp", 443] not in elems  # proxy -> hass
