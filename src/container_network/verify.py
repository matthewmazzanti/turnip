#!/usr/bin/env python3
"""
verify.py -- the `verify` command: report the routed fabric's dataplane state.

Read-only. For each present namespace it dumps the router's gateway + per-veth
state and the load-bearing /32 routes (over pyroute2), then the router's sysctls
and nft table (read from INSIDE the router via a forked setns hop), and each
container's eth0 address + default route. Pure reporting -- no mutation.
"""

import os
import socket
import subprocess

from pyroute2 import NetNS

from .fabric import ALL_NS, FABRIC_IF, GW_IP, HOST_PREFIX, HOSTS, ROUTER
from .netns import find_ifindex, open_namespaces, path_for, run_in_netns
from .nftlib import find_nft


def _has_route(ns: NetNS, dst: str, dst_len: int, oif: int) -> bool:
    """True if a route to dst/dst_len out `oif` exists in `ns` (AF_INET)."""
    for r in ns.get_routes(family=socket.AF_INET):
        attrs = dict(r["attrs"])
        if r["dst_len"] == dst_len and attrs.get("RTA_DST") == dst and attrs.get("RTA_OIF") == oif:
            return True
    return False


def _has_default_via(ns: NetNS, gw: str) -> bool:
    """True if `ns` has a default route (dst_len 0) via gateway `gw`."""
    for r in ns.get_routes(family=socket.AF_INET):
        if r["dst_len"] == 0 and dict(r["attrs"]).get("RTA_GATEWAY") == gw:
            return True
    return False


def _router_dataplane_report(nft: str) -> str:
    """Run inside router: read the sysctls and check the nft table. Returns a
    formatted block (piped back to the parent by run_in_netns)."""

    def read(key: str) -> str:
        try:
            with open("/proc/sys/" + key.replace(".", "/")) as f:
                return f.read().strip()
        except OSError:
            return "?"

    lines = [
        f"  ip_forward={read('net.ipv4.ip_forward')} "
        f"all.rp_filter={read('net.ipv4.conf.all.rp_filter')} "
        f"ipv6.disabled={read('net.ipv6.conf.all.disable_ipv6')}"
    ]
    for h in HOSTS:
        pa = read(f"net.ipv4.conf.{h.router_if}.proxy_arp")
        rp = read(f"net.ipv4.conf.{h.router_if}.rp_filter")
        lines.append(f"  {h.name}: proxy_arp={pa} rp_filter={rp}")
    chk = subprocess.run([nft, "list", "table", "inet", "fabric"], text=True, capture_output=True)
    if chk.returncode == 0:
        nrules = sum(chk.stdout.count(v) for v in ("accept", "drop"))
        lines.append(f"  nft table 'fabric': present (~{nrules} verdicts)")
    else:
        lines.append("  nft table 'fabric': MISSING")
    return "\n".join(lines) + "\n"


def verify() -> None:
    present = [n for n in ALL_NS if os.path.ismount(path_for(n))]
    for n in ALL_NS:
        if n not in present:
            print(f"{n}: MISSING ({path_for(n)})")
    if not present:
        return

    with open_namespaces(present) as ns:
        # router: gateway, per-veth state + the load-bearing /32 route, then the
        # sysctls + nft table (read from inside via a forked hop)
        if ROUTER in ns:
            r = ns[ROUTER]
            gidx = find_ifindex(r, FABRIC_IF)
            if gidx is None:
                print(f"router: {FABRIC_IF} MISSING (router up but no gateway)")
            else:
                attrs = dict(r.get_links(gidx)[0]["attrs"])
                addrs = [dict(a["attrs"]).get("IFA_ADDRESS") for a in r.get_addr(index=gidx)]
                print(f"router: {FABRIC_IF} oper={attrs.get('IFLA_OPERSTATE')} addrs={addrs}")

            for h in HOSTS:
                ridx = find_ifindex(r, h.router_if)
                if ridx is None:
                    print(f"  {h.name}: {h.router_if} MISSING (no fabric veth)")
                    continue
                attrs = dict(r.get_links(ridx)[0]["attrs"])
                route_ok = "ok" if _has_route(r, h.ip, HOST_PREFIX, ridx) else "MISSING"
                print(
                    f"  {h.name}: {h.router_if} "
                    f"oper={attrs.get('IFLA_OPERSTATE')} "
                    f"route({h.ip}/{HOST_PREFIX} dev {h.router_if})={route_ok}"
                )

            try:
                report = run_in_netns(
                    path_for(ROUTER), lambda: _router_dataplane_report(find_nft())
                )
                print(report, end="")
            except Exception as e:
                print(f"  dataplane report unavailable: {e}")

        # containers: eth0 state + address + default route via the gateway
        for h in HOSTS:
            if h.name not in ns:
                continue
            c = ns[h.name]
            cidx = find_ifindex(c, h.cont_if)
            if cidx is None:
                print(f"{h.name}: {h.cont_if} MISSING (netns up but no veth end)")
                continue
            attrs = dict(c.get_links(cidx)[0]["attrs"])
            addrs = [dict(a["attrs"]).get("IFA_ADDRESS") for a in c.get_addr(index=cidx)]
            gw_ok = "ok" if _has_default_via(c, GW_IP) else "MISSING"
            print(
                f"{h.name}: {h.cont_if} oper={attrs.get('IFLA_OPERSTATE')} "
                f"addrs={addrs} default-via-{GW_IP}={gw_ok}"
            )
