# turnip

A persistent network for **rootless podman** containers: a **routed, Cilium-style L3
network** built out of network namespaces, veths, and nftables — no shared L2 bridge.
Each container hangs off a central `router` netns by its own `/32` veth; the router
forwards between them by destination IP, and an nftables flow matrix decides who may
talk to whom. The network's *model* (who exists, who may talk, what crosses the edge)
is a declarative `turnip.json`; the mechanism builds the dataplane from it.

> **Status: rootful; functional and integration-validated.** The implementation is
> [`cmd/turnip`](cmd/turnip) (Go). It runs **rootful** — the host edge (uplinks,
> container links) needs the init netns, so turnip runs as root and drops to the
> rootless-podman owner to enter podman's namespaces. The kernel-interface
> primitives (podman-userns bootstrap, netns fd collection + bind-mount persistence,
> sysctl/nft/netlink over an fd) live in [`internal/netns`](internal/netns) +
> [`internal/dataplane`](internal/dataplane). The hermetic two-node integration check
> (`nix flake check`) exercises the full matrix — structure, internal flows, network
> isolation, egress/ingress, and packet-crafting bad-actor checks (source spoofing,
> out-of-state injection, ARP poisoning); see [`docs/TEST-PLAN.md`](docs/TEST-PLAN.md).
> A real `podman run --network ns:` attach is covered by `TestPodmanRun` (a fresh
> container joining a pinned netns, governed by the same flow matrix); the egress/ingress
> operator variants remain (see [`todo.md`](todo.md)). Design docs live in
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
               ip_forward=1 ; conf.default: proxy_arp=1, rp_filter=1 (strict),
                 redirects/source-route off — every veth born hardened
               ipv6 disabled ; nft table inet turnip (forward flow matrix)
zwave  netns:  eth0 10.0.0.11/32  default via 10.0.0.1
hass   netns:  eth0 10.0.0.12/32  default via 10.0.0.1
proxy  netns:  eth0 10.0.0.13/32  default via 10.0.0.1
```

The example flow matrix is hub-and-spoke with `hass` as the hub: `zwave`→`hass`
and `hass`→`proxy` on tcp/443 (flows are **directional** — `from` initiates to
`to`). Every container may reach the gateway. Edit `containers`/`networks`/`flows`
in `turnip.json` to change it.

## Layout

| Path | Role |
|------|------|
| `cmd/turnip/` | the CLI + orchestration (the imperative shell): config/env IO, the `buildPlan` lowering (config → `Plan`, `plan.go`), the `applyPlan` driver (`apply.go`), and `up`/`down` dispatch |
| `internal/` | `config` (the declarative model + validation), `netns` (podman bootstrap, netns lifecycle, the SCM_RIGHTS fd bridge), `dataplane` (gateway/veth/route wiring + the nft flow matrix) |
| `nix/` | the flake helpers (`nix/lib`) + every VM (`nix/vm/`): `default.nix` groups them as `interactive.{host,world}` (dev VMs) and `test.{host,world}` (check nodes), each role growing from one base (`host-base.nix` / `world-base.nix`) plus the interactive carve-outs (`interactive.nix`); the probe OCI image is defined in `host-base.nix` |
| `nix/lib/turnip.nix` | the layered "wrap turnip for Nix" helpers — `turnipWithConfigFile` (bake a config file), `turnipWithConfig` (a Nix attrset → `toJSON`), `turnipService` (a systemd up/down unit). Exposed as `lib.<system>` |
| `nix/demo/` | the single-file worked example (`homelab.nix`): a bootable VM that deploys a turnip fabric + three [quadlet-nix](https://github.com/SEIAROTg/quadlet-nix) containers and a `turnip-demo` guided tour. `nix run .#demo` |
| `test/integration/` | the hermetic two-node dataplane check (`checks.integration`) — L1–L4 + bad-actor scenarios |
| `docs/` | design docs — `ARCHITECTURE.md` (config/plan/apply layering), `CONFIG-SKETCH.md` (config model), `TEST-PLAN.md` (the integration matrix), `SYSCTLS.md` (sysctl-hardening verdicts) |
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

## From Nix — the layered helpers + the demo

`nix/lib/turnip.nix` wraps turnip for Nix in three composable layers (exposed as
`lib.<system>`):

```nix
turnipWithConfigFile { configFile = ./turnip.json; }   # wrap the binary around a config file
turnipWithConfig     { config = { runtime.user = "homelab"; /* … */ }; }  # a Nix attrset → toJSON
turnipService        { config = { /* … */ }; requiresUserSession = 1001; }  # a systemd up/down unit
```

Each builds on the one before; `turnipService` takes a prebuilt `package` *or* a
`config`/`configFile` and emits a `systemd.services.<name>` fragment (`Type=oneshot`,
`up` on start, `down` on stop). It bakes the binaries turnip execs — `nft` and
`podman` (both injectable; pass `config.virtualisation.podman.package` so the rootless
`newuidmap`/`newgidmap` wrappers line up) — but **not** `ip`: turnip does all
link/addr/route work over netlink syscalls.

`nix/demo/homelab.nix` is a single-file worked example — the README's hass homelab
(zwave → hass → proxy, hass on a host-LAN veth link) deployed as turnip + three
[quadlet-nix](https://github.com/SEIAROTg/quadlet-nix) containers, baked into a
bootable VM:

```sh
nix run .#demo        # boots the VM on the serial console (autologin: demo/demo)
turnip-demo           # the guided tour, once you're in
```

The tour pokes the live fabric (via `turnip probe <ctr> -- <cmd>`) to show the two
things plain podman can't express: **directional, default-deny flow control**
(zwave → hass is allowed, zwave → proxy is dropped) and a **real host-LAN link**
(hass holds a `192.168.1.x` address on the host bridge; zwave can't reach the LAN at
all).

## Mechanism

turnip reaches podman's user+mount namespaces through `podman unshare` as an **exec
boundary** — Go's multithreaded runtime can't `setns(CLONE_NEWUSER)` in-process the way
the Python tool did, so a fresh process is dropped inside podman's userns instead. A
phase-1 child creates each netns there, **pins it with a bind-mount** (so `podman run
--network ns:<path>` can attach later), and ships its fd back to the root parent over
SCM_RIGHTS. The parent then drives the whole dataplane against those fds: sysctls via a
`setns` episode, nft via a forked `nft -j -f -` child wrapped in that same `setns` episode
(so it inherits the router netns), and links/addrs/routes via `vishvananda/netlink`. The capability
reasoning: init-root holds `CAP_NET_ADMIN` over the podman-userns-owned netns, so the
parent can drive the dataplane against the collected fds.

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
