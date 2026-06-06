# container-network

A persistent, rootless container network for podman: a **routed, Cilium-style L3
fabric** built out of network namespaces, veths, and nftables — no shared L2
bridge. Each container hangs off a central `router` netns by its own `/32` veth;
the router forwards between them by destination IP, and an nftables flow matrix
decides who may talk to whom.

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

The flow matrix is hub-and-spoke with `hass` as the hub: `zwave`↔`hass` and
`hass`↔`proxy` on tcp/443; `zwave`↔`proxy` is denied. Every container may reach
the gateway. Edit `FABRIC_FLOWS` / `HOSTS` in `main.py` to change it.

## Usage

Run as your **normal login user** — no `podman unshare` wrapper. `main.py` enters
podman's rootless user+mount namespaces in-process (see `in_podman_context`); you
just have to use the venv interpreter that has pyroute2:

```sh
./.venv/bin/python main.py up       # create + wire everything
./.venv/bin/python main.py verify   # report dataplane state
./.venv/bin/python main.py down     # tear it all down
```

Attach a container to one of the namespaces (joins it via `--network ns:<path>`):

```sh
./run-container.sh            # hass netns, netshoot shell
./run-container.sh zwave      # zwave netns
```

`down` removes the namespaces; since the `router` netns owns the gateway, all
veths, the routes, the sysctls, and the nft table, removing it is a complete
teardown — there are no per-element deletes.

## Files

| File            | Role |
|-----------------|------|
| `main.py`       | Orchestration + CLI: `create_gateway`, `connect`, `configure_dataplane`, and the `up`/`verify`/`down` dispatch. |
| `fabric.py`     | The model: `Host`, `HOSTS`, `FABRIC_FLOWS`, and the addressing constants (`ROUTER`/`GW_IP`/…). A leaf module so the others don't import-cycle through `main`. |
| `netns.py`      | The namespace layer: enter podman's namespaces (`in_podman_context`), netns lifecycle (`ensure_netns`/`remove_netns`), open sockets + ifindex lookups, and run-code/write-sysctls inside a netns (`run_in_netns`/`write_sysctls`). Plus the rootless / pyroute2 rationale. |
| `nftops.py`     | Builds the nftables flow matrix as libnftables JSON (`build_nft`) and locates `nft` (`find_nft`). |
| `verify.py`     | The `verify` command — read-only dataplane report. |
| `lan-attach.py` | **Stale, pending rework.** A rootful helper that wires a host veth into the fabric. Written for the old bridge model; will be reworked to drop a LAN veth into the `hass` netns (so `hass` is the sole LAN/mDNS/discovery-facing endpoint) once that feature lands. |
| `run-container.sh` | Launch a podman container attached to a fabric namespace. |
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
  `nft -j -f -` (built programmatically by `build_nft` in `nftops.py`, no
  hand-formatted text). See `netns.run_in_netns` / `main.configure_dataplane`.
- **The "virtual" gateway is made real.** A pure Cilium-style virtual gateway
  relies on a default route (via an uplink) for proxy_arp to answer. This fabric
  is self-contained (no host uplink — that needs root in the host netns), so
  `10.0.0.1` is assigned to a `dummy` (`fabric0`) and answered by the normal ARP
  responder. `proxy_arp` is kept on each veth — harmless now, correct once an
  uplink is added.

## Known gaps / next steps

- **No external egress.** The fabric is self-contained; nothing routes to the
  host LAN or the internet. The intended path is a rootful LAN attach into the
  `hass` netns (multi-homing `hass`), at which point its routes need fixing so
  the LAN becomes the default and the fabric stays a more-specific route. This is
  what `lan-attach.py` will be reworked into.
- **IPv4 only, by design.** IPv6 is disabled router-wide (no service needs it
  here, and it's one less thing to lock down). Adding it would mean a parallel v6
  dataplane.
