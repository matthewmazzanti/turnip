# turnip

A persistent network for **rootless podman** containers: a **routed, Cilium-style L3
network** built out of network namespaces, veths, and nftables — no shared L2 bridge.
Each container hangs off a central `router` netns by its own `/32` veth; the router
forwards between them by destination IP, and an nftables flow matrix decides who may
talk to whom. The network's *model* (who exists, who may talk, what crosses the edge)
is a declarative `turnip.json`; the mechanism builds the dataplane from it.

> **Status: Go rewrite in progress (rootful).** The active implementation is
> [`cmd/turnip`](cmd/turnip) (Go). It runs **rootful** — the host edge (uplinks,
> container links) needs the init netns, so turnip runs as root and drops to the
> rootless-podman owner to enter podman's namespaces. The reference Python
> implementation it's based on is parked under [`old/`](old); the kernel-interface
> primitives (podman-userns bootstrap, netns fd collection + bind-mount persistence,
> sysctl/nft/netlink over an fd) are validated in
> [`spike/go-netns-bootstrap`](spike/go-netns-bootstrap). Design docs live in
> [`docs/`](docs).

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
               ipv6 disabled ; nft table inet turnip (forward flow matrix)
zwave  netns:  eth0 10.0.0.11/32  default via 10.0.0.1
hass   netns:  eth0 10.0.0.12/32  default via 10.0.0.1
proxy  netns:  eth0 10.0.0.13/32  default via 10.0.0.1
```

The example flow matrix is hub-and-spoke with `hass` as the hub: `zwave`→`hass`
and `hass`→`proxy` on tcp/443 (flows are **directional** — `from` initiates to
`to`). Every container may reach the gateway. Edit `containers`/`networks`/`flows`
in `turnip.json` to change it (example: `old/tests/turnip.example.json`).

## Layout

| Path | Role |
|------|------|
| `cmd/turnip/` | the CLI + orchestration (the imperative shell): config/env IO, the `buildModel` lowering (config → `Plan`, `model.go`), the `applyPlan` driver (`apply.go`), and `up`/`down` dispatch |
| `internal/` | `config` (the declarative model + validation), `netns` (podman bootstrap, netns lifecycle, the SCM_RIGHTS fd bridge), `dataplane` (gateway/veth/route wiring + the nft flow matrix) |
| `nix/` | the flake helpers (`nix/lib`) + the rootless-podman dev VM (`testvm.nix`, `turnip-host.nix`) |
| `spike/go-netns-bootstrap/` | the validated kernel-interface primitives the port builds on |
| `old/` | the reference Python implementation (`src/turnip/`, tests, the privilege probe) |
| `docs/` | design docs — `ARCHITECTURE.md` (the config/plan/apply layering), `CONFIG-SKETCH.md` (config model + deferred-feature specs) |
| `todo.md` | the open-work checklist |

## Usage

> The port is functional and VM-validated: `up`/`down` build and tear down the
> full routed fabric (gateway, /32 veths, sysctls, nft matrix, uplinks, links).
> The rootless-podman attach end-to-end is the remaining gate (see `todo.md`).

```sh
nix build .#turnip      # -> result/bin/turnip   (or: go build ./cmd/turnip)
sudo turnip up          # create + wire the namespaces the config implies
sudo turnip down        # tear them down
```

turnip runs rootful and resolves the rootless-podman owner from `$SUDO_USER`. A
container attaches to a router netns by joining it (`podman run --network
ns:<state_dir>/containers/<name>/netns`) with its generated hosts file bind-mounted
to `/etc/hosts`. Each router netns owns its gateway, veths, routes, sysctls, and nft
table, so removing it is a complete teardown — no per-element deletes; `up` is
`down` + build (clean slate every time).

## Mechanism

turnip reaches podman's user+mount namespaces through `podman unshare` as an **exec
boundary** — Go's multithreaded runtime can't `setns(CLONE_NEWUSER)` in-process the way
the Python tool did, so a fresh process is dropped inside podman's userns instead. A
phase-1 child creates each netns there, **pins it with a bind-mount** (so `podman run
--network ns:<path>` can attach later), and ships its fd back to the root parent over
SCM_RIGHTS. The parent then drives the whole dataplane against those fds: sysctls via a
`setns` episode, nft via the netns-bound netlink socket (`google/nftables` `WithNetNSFd`,
no `nft` subprocess), and links/addrs/routes via `vishvananda/netlink`. See
[`spike/go-netns-bootstrap`](spike/go-netns-bootstrap) for the validated walk-through and
the capability reasoning (init-root holds `CAP_NET_ADMIN` over the podman-userns-owned
netns).

- **The "virtual" gateway is made real.** A pure Cilium-style virtual gateway relies on
  a default route (via an uplink) for proxy_arp to answer. A network with no uplink is
  self-contained, so `10.0.0.1` is assigned to a `dummy` (`fabric0`) and answered by the
  normal ARP responder. `proxy_arp` is kept on each veth — harmless without an uplink,
  correct once one is added.

## Design decisions

- **Rootful.** The host edge (uplink NAT/DNAT, container `links` into the host LAN) needs
  the init netns, so turnip runs as root. The capability-based path (an unprivileged user
  with ambient `CAP_NET_ADMIN`) is out of scope for the rewrite.
- **IPv4 only.** IPv6 is disabled router-wide (no service needs it here, and it's one
  less thing to lock down). Adding it would mean a parallel v6 dataplane.
