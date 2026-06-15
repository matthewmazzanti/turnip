#!/usr/bin/env python3
"""
nftlib.py -- a small, use-case-agnostic DSL for libnftables JSON, data-driven.

The operators are pure frozen dataclasses -- plain data, no methods. A single
`render()` function pattern-matches over the sum types and produces the
libnftables JSON; nested nodes render recursively. Because render() matches an
exhaustive union (closed with `assert_never`), adding a node type is a type error
until every interpreter handles it. Fields are CONSTRAINED: Literals for the
closed sets nft defines (meta keys, match ops, chain hooks/types/policies,
verdict kinds), so a typo is a type error, not a dict that silently builds bad
JSON. A second interpreter (nft text, a differ) is just another match over the
same sums.

Three tiers, each a sum:
  Expr    -- expressions: payload/meta/ct/concat, match/vmap/verdict
  Object  -- table-level objects: table/chain/verdict_map/rule
  Command -- verbs: add/delete
The Table context stamps family/table (and chain) onto the objects it builds.
Lowercase factories are the ergonomic constructors. Shapes verified against
`nft -j list`.
"""

from __future__ import annotations

import json
import os
import shutil
import subprocess
import sys
from collections.abc import Iterable
from dataclasses import dataclass
from typing import Any, Literal, assert_never

# Closed sets nft defines (from libnftables-json(5)); constraining to these turns
# typos into type errors.
MetaKey = Literal[
    "length",
    "protocol",
    "priority",
    "random",
    "mark",
    "iif",
    "iifname",
    "iiftype",
    "oif",
    "oifname",
    "oiftype",
    "skuid",
    "skgid",
    "nftrace",
    "rtclassid",
    "ibriport",
    "obriport",
    "ibridgename",
    "obridgename",
    "pkttype",
    "cpu",
    "iifgroup",
    "oifgroup",
    "cgroup",
    "nfproto",
    "l4proto",
    "secpath",
]
Family = Literal["ip", "ip6", "inet", "arp", "bridge", "netdev"]
MatchOp = Literal["==", "!=", "<", ">", "<=", ">=", "in"]
ChainType = Literal["filter", "nat", "route"]
Hook = Literal[
    "prerouting", "input", "forward", "output", "postrouting", "ingress", "egress"
]
Policy = Literal["accept", "drop"]
VerdictKind = Literal["accept", "drop", "continue", "return", "jump", "goto"]

# An immediate literal scalar.
Lit = str | int | bool


# --- expression types: operands --------------------------------------------


@dataclass(frozen=True)
class Payload:
    """A header-field reference -- `ip saddr`, `th dport`."""

    protocol: str
    field: str


@dataclass(frozen=True)
class Meta:
    """A meta selector -- `meta l4proto`. key is constrained to nft's META_KEY set."""

    key: MetaKey


@dataclass(frozen=True)
class Ct:
    """A conntrack selector -- `ct state`."""

    key: str


@dataclass(frozen=True)
class Concat:
    """A concatenation -- `a . b . c`. Parts are operands or immediates."""

    parts: tuple[Value, ...]

    def __post_init__(self) -> None:
        if len(self.parts) < 2:
            raise ValueError("concat needs at least two parts")


# --- expression types: statements -------------------------------------------


@dataclass(frozen=True)
class Match:
    """A match -- `<left> <op> <right>`. op is constrained to the relational set;
    right is an operand, immediate, or a set."""

    left: Expr
    right: Value
    op: MatchOp = "=="


@dataclass(frozen=True)
class Vmap:
    """A verdict-map lookup -- `<key> vmap @<setname>`."""

    key: Expr
    setname: str


@dataclass(frozen=True)
class Verdict:
    """A verdict -- accept/drop/continue/return, or jump/goto with a target chain.

    __post_init__ enforces the cross-field rule the JSON shape can't: jump/goto
    require a target, the rest forbid one.
    """

    kind: VerdictKind
    target: str | None = None

    def __post_init__(self) -> None:
        needs_target = self.kind in ("jump", "goto")
        if needs_target and self.target is None:
            raise ValueError(f"{self.kind} verdict requires a target chain")
        if not needs_target and self.target is not None:
            raise ValueError(f"{self.kind} verdict takes no target")


# --- object types -----------------------------------------------------------


@dataclass(frozen=True)
class Chain:
    """A chain. type+hook+prio (+policy) make it a base chain; omit them for a
    regular (jump-target) chain."""

    family: Family
    table: str
    name: str
    type: ChainType | None = None
    hook: Hook | None = None
    prio: int | None = None
    policy: Policy | None = None


@dataclass(frozen=True)
class VerdictMap:
    """A named verdict map -- `map <name> { type <key_type> : verdict; ... }`.
    elements are (key, verdict) node pairs."""

    family: Family
    table: str
    name: str
    key_type: tuple[str, ...]
    elements: tuple[tuple[Expr, Expr], ...]


@dataclass(frozen=True)
class Rule:
    """A rule in `chain`, with a list of statement nodes."""

    family: Family
    table: str
    chain: str
    exprs: tuple[Expr, ...]


# --- command types ----------------------------------------------------------


@dataclass(frozen=True)
class Add:
    obj: Object


@dataclass(frozen=True)
class Delete:
    obj: Object


# --- ruleset (top level) ----------------------------------------------------


@dataclass(frozen=True)
class Ruleset:
    """The top-level command batch -- renders to `{"nftables": [<commands>]}`."""

    commands: tuple[Command, ...]


# --- context ----------------------------------------------------------------


@dataclass(frozen=True)
class Table:
    """A bound (family, name) table -- both an object (the `{table: ...}` body)
    and the context that stamps family/table (and chain, for rules) onto the
    objects its factory methods build. Those methods return Add commands;
    reload() returns the flush-and-reload triple. The caller assembles the
    commands in order -- maps must precede the rules using them.
    """

    family: Family
    name: str

    def reload(self) -> list[Command]:
        """The flush-and-reload triple -- add-empty / delete / re-add -- so a
        re-run replaces the whole table atomically and idempotently."""
        return [Add(self), Delete(self), Add(self)]

    def chain(
        self,
        name: str,
        *,
        type: ChainType | None = None,
        hook: Hook | None = None,
        prio: int | None = None,
        policy: Policy | None = None,
    ) -> Add:
        return Add(Chain(self.family, self.name, name, type, hook, prio, policy))

    def verdict_map(
        self, name: str, key_type: list[str], elements: list[tuple[Expr, Expr]]
    ) -> Add:
        return Add(
            VerdictMap(self.family, self.name, name, tuple(key_type), tuple(elements))
        )

    def rule(self, chain: str, *exprs: Expr) -> Add:
        return Add(Rule(self.family, self.name, chain, exprs))


# --- sums -------------------------------------------------------------------

# Expressions; what an op accepts as an operand (Value); table-level objects;
# command verbs. render() is total over Value | Object | Command.
Expr = Payload | Meta | Ct | Concat | Match | Vmap | Verdict
Value = Expr | Lit | list[str | int]
Object = Table | Chain | VerdictMap | Rule
Command = Add | Delete


# --- render -----------------------------------------------------------------


def render(x: Value | Object | Command | Ruleset) -> Any:
    """Render any node -- expression, immediate, set, object, command, or the
    top-level ruleset -- to its libnftables JSON. One match over the closed sums;
    `assert_never` makes an unhandled variant a type error."""
    match x:
        case Payload():
            return {"payload": {"protocol": x.protocol, "field": x.field}}
        case Meta():
            return {"meta": {"key": x.key}}
        case Ct():
            return {"ct": {"key": x.key}}
        case Concat():
            return {"concat": [render(p) for p in x.parts]}
        case Match():
            return {
                "match": {"op": x.op, "left": render(x.left), "right": render(x.right)}
            }
        case Vmap():
            return {"vmap": {"key": render(x.key), "data": f"@{x.setname}"}}
        case Verdict():
            return {x.kind: {"target": x.target} if x.target is not None else None}
        case Table():
            return {"table": {"family": x.family, "name": x.name}}
        case Chain():
            spec: dict[str, Any] = {
                "family": x.family,
                "table": x.table,
                "name": x.name,
            }
            if x.type is not None:
                spec["type"] = x.type
            if x.hook is not None:
                spec["hook"] = x.hook
            if x.prio is not None:
                spec["prio"] = x.prio
            if x.policy is not None:
                spec["policy"] = x.policy
            return {"chain": spec}
        case VerdictMap():
            return {
                "map": {
                    "family": x.family,
                    "table": x.table,
                    "name": x.name,
                    "map": "verdict",
                    "type": list(x.key_type),
                    "elem": [[render(k), render(v)] for k, v in x.elements],
                }
            }
        case Rule():
            return {
                "rule": {
                    "family": x.family,
                    "table": x.table,
                    "chain": x.chain,
                    "expr": [render(e) for e in x.exprs],
                }
            }
        case Add():
            return {"add": render(x.obj)}
        case Delete():
            return {"delete": render(x.obj)}
        case Ruleset():
            return {"nftables": [render(c) for c in x.commands]}
        case list():
            return [render(i) for i in x]
        case str() | int():  # also catches bool (a subclass of int)
            return x
        case _:
            assert_never(x)


# --- constructors -----------------------------------------------------------
# Ergonomic factories for the Node tier (operands + statements). The
# object/command tiers are built via the Table context above.


def payload(protocol: str, field: str) -> Payload:
    return Payload(protocol, field)


def meta(key: MetaKey) -> Meta:
    return Meta(key)


def ct(key: str) -> Ct:
    return Ct(key)


def concat(*parts: Value) -> Concat:
    return Concat(parts)


def match(left: Expr, right: Value, op: MatchOp = "==") -> Match:
    return Match(left, right, op)


def vmap(key: Expr, setname: str) -> Vmap:
    return Vmap(key, setname)


def ct_state(*states: str) -> Match:
    """Match `ct state` against one or more states (op "in", as nft requires).

    A single state renders as a scalar, multiple as a list -- matching nft's own
    JSON, e.g. ct_state("new") vs ct_state("established", "related").
    """
    if not states:
        raise ValueError("ct_state needs at least one state")
    right: Value = states[0] if len(states) == 1 else list[str | int](states)
    return Match(Ct("state"), right, "in")


def accept() -> Verdict:
    return Verdict("accept")


def drop() -> Verdict:
    return Verdict("drop")


def jump(target: str) -> Verdict:
    return Verdict("jump", target)


def goto(target: str) -> Verdict:
    return Verdict("goto", target)


def ruleset(commands: Iterable[Command]) -> Ruleset:
    """The top-level batch -- `render(ruleset(cmds))` gives `{"nftables": [...]}`."""
    return Ruleset(tuple(commands))


# --- nft execution ----------------------------------------------------------


def find_nft() -> str:
    """Absolute path to `nft`. With the in-process model the environment is
    intact (no `podman unshare` reset), so shutil.which normally finds it; the
    fallbacks (incl. NixOS's current-system path) are belt-and-suspenders."""
    found = shutil.which("nft")
    if found:
        return found
    for cand in (
        "/run/current-system/sw/bin/nft",
        "/usr/sbin/nft",
        "/usr/bin/nft",
        "/sbin/nft",
    ):
        if os.path.exists(cand):
            return cand
    raise FileNotFoundError(
        "nft binary not found (looked in PATH and common locations)"
    )


def load(rs: Ruleset) -> None:
    """Render `rs` and load it via `nft -j -f -`. Runs nft in the CURRENT netns,
    so call it from inside the run_in_netns hop to land in the right namespace."""
    nft_bin = find_nft()
    proc = subprocess.run(
        [nft_bin, "-j", "-f", "-"],
        input=json.dumps(render(rs)),
        text=True,
        capture_output=True,
    )
    if proc.returncode != 0:
        # surface nft's diagnostic from inside the child before it exits nonzero
        sys.stderr.write(proc.stderr)
        raise RuntimeError(f"nft load failed (rc={proc.returncode})")
