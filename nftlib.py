#!/usr/bin/env python3
"""
nftlib.py -- a small, use-case-agnostic DSL for libnftables JSON, as a typed AST.

Each operator is a frozen dataclass node with a render() that produces its
libnftables JSON; nested nodes render recursively. Sub-expressions are typed
`Node`; operand inputs are a typed union `Value` (a node, an immediate, or a set
of immediates), so render() dispatches over a known domain (a `match`, no
`Any`/cast escape hatch). Fields are CONSTRAINED: Literals for the closed sets
nft defines (meta keys, match ops, chain hooks/types/policies, verdict kinds), so
a typo is a type error, not a dict that silently builds bad JSON (which is all an
`= dict[str, Any]` alias ever caught: nothing).

The file is laid out types-first then constructors: the dataclasses/ABCs grouped
by tier -- expressions (Node: operands -> statements), objects (Obj: table /
chain / map / rule), commands (Command: add / delete), and the Table context --
followed by the lowercase factory functions (the ergonomic constructors for the
Node tier) collected in one group at the end. Shapes verified against
`nft -j list`.
"""

import abc
from dataclasses import dataclass
from typing import Any, Literal

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
MatchOp = Literal["==", "!=", "<", ">", "<=", ">=", "in"]
ChainType = Literal["filter", "nat", "route"]
Hook = Literal[
    "prerouting", "input", "forward", "output", "postrouting", "ingress", "egress"
]
Policy = Literal["accept", "drop"]
VerdictKind = Literal["accept", "drop", "continue", "return", "jump", "goto"]


class Node(abc.ABC):
    """A libnftables AST node. render() returns its JSON value."""

    @abc.abstractmethod
    def render(self) -> Any: ...


# What an op accepts as an operand: a node, an immediate scalar, or a set of
# immediates (a JSON array, e.g. a match right of `{established, related}`).
Value = Node | str | int | bool | list[str | int]


def render(value: Value) -> Any:
    """Render a node, an immediate, or a set (list of immediates) to its JSON.

    The param is the typed operand union, so the list branch's elements are known
    to be immediates -- no cast needed.
    """
    match value:
        case Node():
            return value.render()
        case list():
            return [render(v) for v in value]
        case _:
            return value


# --- expression types: operands --------------------------------------------


@dataclass(frozen=True)
class Payload(Node):
    """A header-field reference -- `ip saddr`, `th dport`."""

    protocol: str
    field: str

    def render(self) -> Any:
        return {"payload": {"protocol": self.protocol, "field": self.field}}


@dataclass(frozen=True)
class Meta(Node):
    """A meta selector -- `meta l4proto`. key is constrained to nft's META_KEY set."""

    key: MetaKey

    def render(self) -> Any:
        return {"meta": {"key": self.key}}


@dataclass(frozen=True)
class Ct(Node):
    """A conntrack selector -- `ct state`."""

    key: str

    def render(self) -> Any:
        return {"ct": {"key": self.key}}


@dataclass(frozen=True)
class Concat(Node):
    """A concatenation -- `a . b . c`. Parts are operands or immediates."""

    parts: tuple[Value, ...]

    def __post_init__(self) -> None:
        if len(self.parts) < 2:
            raise ValueError("concat needs at least two parts")

    def render(self) -> Any:
        return {"concat": [render(p) for p in self.parts]}


# --- expression types: statements -------------------------------------------


@dataclass(frozen=True)
class Match(Node):
    """A match -- `<left> <op> <right>`. op is constrained to the relational set;
    right is an operand, immediate, or a set."""

    left: Node
    right: Value
    op: MatchOp = "=="

    def render(self) -> Any:
        return {
            "match": {
                "op": self.op,
                "left": render(self.left),
                "right": render(self.right),
            }
        }


@dataclass(frozen=True)
class Vmap(Node):
    """A verdict-map lookup -- `<key> vmap @<setname>`."""

    key: Node
    setname: str

    def render(self) -> Any:
        return {"vmap": {"key": render(self.key), "data": f"@{self.setname}"}}


@dataclass(frozen=True)
class Verdict(Node):
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

    def render(self) -> Any:
        return {self.kind: {"target": self.target} if self.target is not None else None}


# --- object types -----------------------------------------------------------


class Obj(abc.ABC):
    """A table-level object (table / chain / map / rule). render() returns its
    `{kind: {...}}` JSON -- the body wrapped by an Add/Delete command."""

    @abc.abstractmethod
    def render(self) -> dict[str, Any]: ...


@dataclass(frozen=True)
class Chain(Obj):
    """A chain. type+hook+prio (+policy) make it a base chain; omit them for a
    regular (jump-target) chain."""

    family: str
    table: str
    name: str
    type: ChainType | None = None
    hook: Hook | None = None
    prio: int | None = None
    policy: Policy | None = None

    def render(self) -> dict[str, Any]:
        spec: dict[str, Any] = {
            "family": self.family,
            "table": self.table,
            "name": self.name,
        }
        if self.type is not None:
            spec["type"] = self.type
        if self.hook is not None:
            spec["hook"] = self.hook
        if self.prio is not None:
            spec["prio"] = self.prio
        if self.policy is not None:
            spec["policy"] = self.policy
        return {"chain": spec}


@dataclass(frozen=True)
class VerdictMap(Obj):
    """A named verdict map -- `map <name> { type <key_type> : verdict; ... }`.
    elements are (key, verdict) node pairs."""

    family: str
    table: str
    name: str
    key_type: tuple[str, ...]
    elements: tuple[tuple[Node, Node], ...]

    def render(self) -> dict[str, Any]:
        return {
            "map": {
                "family": self.family,
                "table": self.table,
                "name": self.name,
                "map": "verdict",
                "type": list(self.key_type),
                "elem": [[render(k), render(v)] for k, v in self.elements],
            }
        }


@dataclass(frozen=True)
class Rule(Obj):
    """A rule in `chain`, with a list of statement nodes."""

    family: str
    table: str
    chain: str
    exprs: tuple[Node, ...]

    def render(self) -> dict[str, Any]:
        return {
            "rule": {
                "family": self.family,
                "table": self.table,
                "chain": self.chain,
                "expr": [render(e) for e in self.exprs],
            }
        }


# --- command types ----------------------------------------------------------


class Command(abc.ABC):
    """A ruleset command: a verb applied to an object. render() -> `{verb: {...}}`."""

    @abc.abstractmethod
    def render(self) -> dict[str, Any]: ...


@dataclass(frozen=True)
class Add(Command):
    obj: Obj

    def render(self) -> dict[str, Any]:
        return {"add": self.obj.render()}


@dataclass(frozen=True)
class Delete(Command):
    obj: Obj

    def render(self) -> dict[str, Any]:
        return {"delete": self.obj.render()}


# --- context ----------------------------------------------------------------


@dataclass(frozen=True)
class Table(Obj):
    """A bound (family, name) table -- both the table object (render() -> the
    `{table: ...}` body) and the context that stamps family/table (and chain,
    for rules) onto the objects its factory methods build. Those methods return
    Add commands; reload() returns the flush-and-reload triple. The caller
    assembles the commands in order -- maps must precede the rules using them.
    """

    family: str
    name: str

    def render(self) -> dict[str, Any]:
        return {"table": {"family": self.family, "name": self.name}}

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
        self, name: str, key_type: list[str], elements: list[tuple[Node, Node]]
    ) -> Add:
        return Add(
            VerdictMap(self.family, self.name, name, tuple(key_type), tuple(elements))
        )

    def rule(self, chain: str, *exprs: Node) -> Add:
        return Add(Rule(self.family, self.name, chain, exprs))


# --- constructors -----------------------------------------------------------
# Ergonomic lowercase factories for the Node tier (operands + statements). The
# object/command tiers are built via the Table context above.


def payload(protocol: str, field: str) -> Payload:
    return Payload(protocol, field)


def meta(key: MetaKey) -> Meta:
    return Meta(key)


def ct(key: str) -> Ct:
    return Ct(key)


def concat(*parts: Value) -> Concat:
    return Concat(parts)


def match(left: Node, right: Value, op: MatchOp = "==") -> Match:
    return Match(left, right, op)


def vmap(key: Node, setname: str) -> Vmap:
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
