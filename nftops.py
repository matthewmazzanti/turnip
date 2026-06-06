#!/usr/bin/env python3
"""
nftops.py -- build the routed fabric's nftables ruleset as libnftables JSON.

The fabric use-case, expressed entirely on the nftlib DSL (operands, statements,
and the Table context) -- no raw libnftables dict literals. build_nft() returns
the command list fed, as JSON, to `nft -j -f -`; find_nft() locates the binary.
The actual apply (the setns hop + `nft -j -f -`) is orchestration; see
main.configure_dataplane.
"""

import os
import shutil
from typing import Any

from fabric import FABRIC_FLOWS, GW_IP, HOSTS
from nftlib import (
    Command,
    Node,
    Table,
    accept,
    concat,
    ct_state,
    drop,
    match,
    meta,
    payload,
    vmap,
)

# Map key types (the `type ...` of each verdict map).
FLOW_KEY = ["ipv4_addr", "ipv4_addr", "inet_proto", "inet_service"]
HOST_KEY = ["ipv4_addr", "ipv4_addr"]


def build_nft() -> dict[str, Any]:
    """Build the `inet fabric` ruleset as a libnftables JSON command list.

    Loaded flush-and-reload (Table.reload) so re-running `up` replaces the table
    atomically and idempotently. Maps are declared before the rules that
    reference them (@allowed_*). The forward chain (policy drop) accepts, in
    order: established/related (the conntrack return path -- so flows are one-way
    in the maps); drops invalid; then for new conns -- ICMP only between
    gateway-authorized pairs (PMTU etc.; ICMP has no port and would otherwise die
    in the L4 map), any-port gateway pairs (allowed_hosts), and service-scoped
    pairs (allowed_flows; `th dport` covers tcp AND udp). Anything else hits
    policy drop. No MAC checks: rp_filter strict on each veth is the spoof pin,
    and since these rules match only v4 (`ip`) and v6 is disabled router-wide,
    any stray v6 forward also hits policy drop.
    """
    ip = {h.name: h.ip for h in HOSTS}

    # allowed_flows elements: each FABRIC_FLOWS tuple, both directions.
    flow_elem: list[tuple[Node, Node]] = []
    for a, b, proto, port in FABRIC_FLOWS:
        for s, d in ((a, b), (b, a)):
            flow_elem.append((concat(ip[s], ip[d], proto, port), accept()))

    # allowed_hosts elements: every container <-> the gateway, both directions.
    host_elem: list[tuple[Node, Node]] = []
    for h in HOSTS:
        host_elem.append((concat(h.ip, GW_IP), accept()))
        host_elem.append((concat(GW_IP, h.ip), accept()))

    # saddr . daddr key, shared by the icmp and any-port host-pair rules.
    sd = [payload("ip", "saddr"), payload("ip", "daddr")]

    t = Table("inet", "fabric")
    cmds: list[Command] = [
        *t.reload(),
        t.chain("forward", type="filter", hook="forward", prio=0, policy="drop"),
        t.verdict_map("allowed_flows", FLOW_KEY, flow_elem),
        t.verdict_map("allowed_hosts", HOST_KEY, host_elem),
        # ct state established,related accept
        t.rule("forward", ct_state("established", "related"), accept()),
        # ct state invalid drop
        t.rule("forward", ct_state("invalid"), drop()),
        # ct state new meta l4proto icmp ip saddr . ip daddr vmap @allowed_hosts
        t.rule(
            "forward",
            ct_state("new"),
            match(meta("l4proto"), "icmp"),
            vmap(concat(*sd), "allowed_hosts"),
        ),
        # ct state new ip saddr . ip daddr vmap @allowed_hosts
        t.rule("forward", ct_state("new"), vmap(concat(*sd), "allowed_hosts")),
        # ct state new ip saddr . ip daddr . meta l4proto . th dport
        #     vmap @allowed_flows
        t.rule(
            "forward",
            ct_state("new"),
            vmap(
                concat(*sd, meta("l4proto"), payload("th", "dport")),
                "allowed_flows",
            ),
        ),
    ]
    return {"nftables": [c.render() for c in cmds]}


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
