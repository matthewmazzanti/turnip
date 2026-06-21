# turnip

A persistent, rootless container network for podman: a **routed, Cilium-style L3
network** built out of network namespaces, veths, and nftables — no shared L2
bridge. Each container hangs off a central `router` netns by its own `/32` veth;
the router forwards between them by destination IP, and an nftables flow matrix
decides who may talk to whom.

The network's *model* (who exists, who may talk, what crosses the edge) is a
declarative `turnip.json`, loaded + validated by `config.py`; the mechanism
(`main.py` over `netns.py`/`nftlib.py`) builds the dataplane from it. The rootless
baseline (milestones 1–3) is done; the rootful host edge (uplinks, links) is next.
See `docs/CONFIG-SKETCH.md` for the config model and `docs/IMPLEMENTATION-PLAN.md`
for the milestone status and architecture.

## Why routed instead of a bridge

On a bridge, every container shares one L2 broadcast domain: they ARP for each
other, so a container can poison another's ARP cache or spoof a source MAC, and
you end up defending with MAC pins / ARP filtering / FDB hygiene in nftables.

Routed, there is no shared L2. A container's only neighbour is the router; the
router forwards by destination IP; and strict `rp_filter` on each fabric veth
drops any packet whose source IP doesn't reverse-route back out the veth it
arrived on. That single per-veth `/32` route does double duty: it's both *how to
reach* a container and *what source is legitimate* on its veth. MAC is irrelevant
at L3, so nothing derives or pins one — and nftables becomes pure L3/L4 flow
policy instead of identity hygiene.

## Topology

```
router netns:  fabric0  10.0.0.1/32     (dummy; the virtual gateway)
               |- vethR-zwave  route 10.0.0.11/32 dev vethR-zwave
               |- vethR-hass   route 10.0.0.12/32 dev vethR-hass
               |- vethR-proxy  route 10.0.0.13/32 dev vethR-proxy
               ip_forward=1 ; per-veth proxy_arp=1, rp_filter=1 (strict)
               ipv6 disabled ; nft table inet fabric (forward flow matrix)
zwave  netns:  eth0 10.0.0.11/32  default via 10.0.0.1
hass   netns:  eth0 10.0.0.12/32  default via 10.0.0.1
proxy  netns:  eth0 10.0.0.13/32  default via 10.0.0.1
```

The example flow matrix is hub-and-spoke with `hass` as the hub: `zwave`→`hass`
and `hass`→`proxy` on tcp/443 (flows are **directional** — `from` initiates to
`to`). Every container may reach the gateway. Edit `containers`/`networks`/`flows`
in `turnip.json` to change it (see `tests/turnip.example.json`). The `inet fabric` table
in the diagram is now `inet turnip`, one per router netns.

## Usage

Run as your **normal login user** — no `podman unshare` wrapper. `main.py` enters
podman's rootless user+mount namespaces in-process (see `in_podman_context`); you
just have to use the venv interpreter that has pyroute2. Config is discovered via
`$TURNIP_CONFIG`, else `./turnip.json`.

```sh
uv run turnip up        # create + wire everything, write hosts files
uv run turnip down      # tear it all down
```

A container attaches to a router netns by joining it (`podman run --network
ns:<state_dir>/containers/<name>/netns`) with its generated hosts file bind-mounted
to `/etc/hosts`; the integration suite drives this directly (`Probe.run_container`).

`down` removes every netns the config implies. Since each router netns owns its
gateway, veths, routes, sysctls, and nft table, removing it is a complete teardown
— no per-element deletes. (`up` is `down()` + build: clean-slate every time.)

## Files

All package modules live under `src/turnip/`.

| File            | Role |
|-----------------|------|
| `main.py`       | The imperative shell + CLI: config/env IO, `resolve_runtime`, `build_model` (config → the `Container`/`Network`/`Endpoint` runtime graph), the wiring (`create_gateway`/`connect`/`configure_dataplane`), the app policy (`build_nft`/`router_sysctls`), hosts-file generation (`container_peers`/`hosts_file`), and the `up`/`down` dispatch. |
| `config.py`     | The declarative model: pure pydantic `Turnip` (types + validation, no IO) for `turnip.json` — containers, networks, attachments, runtime. This *is* the model the mechanism consumes. |
| `netns.py`      | The namespace layer (pure mechanism, explicit args): enter podman's namespaces (`in_podman_context`), netns lifecycle (`create_netns`/`remove_netns`), ifindex lookups, run-code/write-sysctls inside a netns (`run_in_netns`/`write_sysctls`). Plus the rootless / pyroute2 rationale. |
| `nftlib.py`     | A use-case-agnostic, data-oriented DSL for libnftables JSON (`render` over frozen-dataclass sums) and the `nft` executor (`load`/`find_nft`). The app policy that uses it (`build_nft`) lives in `main.py`. |
| `*.py.bak`      | The old literal-driven `main.py`/`verify.py`, parked as reference for the remaining milestones (M4/M5); to be removed when those land. |
| `typings/`      | Local partial pyroute2 stubs (it ships none); scoped to the API surface we use. |

## Design notes

The two genuinely subtle pieces (both documented at length in the source):

- **Entering podman's namespaces in-process.** Everything must run inside
  podman's user+mount namespaces (the mount ns so the persistent `~/netns/*`
  bind-mounts are visible; the user ns so we hold `CAP_NET_ADMIN` over the
  namespaces podman owns). Instead of wrapping the script in `podman unshare`,
  `in_podman_context` reads the rootless pause pid, forks, and `setns`es into the
  pause process's user ns then mount ns — the login user is the *owner* of
  podman's userns, so it gains full caps on the join. Env stays intact, so PATH /
  `nft` / the venv resolve normally.
- **Why a forked `setns` child for sysctls + nftables.** pyroute2 drives
  links/addrs/routes over a netlink socket bound *into* a netns, but `/proc/sys`
  (sysctls) and the nft ruleset reflect the calling *process's* netns — they
  aren't reachable through that socket. So `run_in_netns` (in `netns.py`) forks a
  child (from within the podman context), `setns`es into the `router` netns,
  writes `/proc/sys` directly, and applies the ruleset as libnftables JSON via
  `nft -j -f -` (built programmatically by `build_nft` in `main.py` on the
  `nftlib.py` DSL, no hand-formatted text). See `netns.run_in_netns` /
  `main.configure_dataplane`.
- **The "virtual" gateway is made real.** A pure Cilium-style virtual gateway
  relies on a default route (via an uplink) for proxy_arp to answer. This fabric
  is self-contained (no host uplink — that needs root in the host netns), so
  `10.0.0.1` is assigned to a `dummy` (`fabric0`) and answered by the normal ARP
  responder. `proxy_arp` is kept on each veth — harmless now, correct once an
  uplink is added.

## Known gaps / next steps

- **No external egress.** The network is self-contained; nothing routes to the
  host LAN or the internet. The intended path is a rootful host `uplink` (NAT +
  DNAT) per network, plus per-container `links` for direct LAN membership — both
  specified in `docs/CONFIG-SKETCH.md` (the "uplink" and "links" sections).
- **IPv4 only, by design.** IPv6 is disabled router-wide (no service needs it
  here, and it's one less thing to lock down). Adding it would mean a parallel v6
  dataplane.
