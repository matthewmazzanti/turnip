# Implementation plan — config → mechanism, by milestone

The model is declarative (`config.py` → `Turnip`). This is the plan to drive the
mechanism from it **bottom-up**: a minimal working vertical pass per milestone,
then refactor to grow the abstractions out of code that already runs — rather than
designing an IR first and discovering its shape is wrong (it already was; see
"Compass").

## Current status (handoff)

- **Done & committed (M1–M3):** the rootless baseline is fully config-driven —
  `turnip up`/`down` create netns, wire the /32 routed veths + gateway, apply the
  `inet turnip` flow matrix, and generate per-container hosts files. `fabric.py` is
  deleted; the old literal-driven `main.py`/`verify.py` are parked as `*.py.bak`.
  47 tests green (ruff/pyright clean). Commits `46fd274`, `fbc1e61`, `e734b05`.
- **Architecture as built:** `main.py` is the imperative shell (config + env IO,
  runtime resolution, `build_model`); `config.py`/`netns.py`/`nftlib.py` are pure.
  The runtime model is a **stateful object graph** (`Container`/`Network`/
  `Endpoint`), not a pure IR. `Model` owns the netns lifetime (`create`/`teardown`/
  `bound()`). `up = down() + build` (clean-slate). Flows are **directional**. State
  lives under `runtime.state_dir` (default `$XDG_RUNTIME_DIR/turnip`).
- **Done & committed (M4 — uplinks):** the rootful host edge — two-phase fork
  (podman child ships router-netns fds → init-side root parent), host masquerade +
  DNAT, router egress/ingress allow rules, default-deny INPUT. `resolve_runtime` is
  privilege-aware (euid/SUDO_USER, reject root-as-target). Commits `f619819`…`94ba0f6`.
- **Done: M5 (links) — all three slices, VM-validated** (slice 1 committed
  `b609257`; slices 2–3 follow). Container-scoped host-netdev holes that bypass the
  routers/nft: `veth→bridge`, `veth→host`, `macvlan`, `ipvlan`, `phys`. `Container`
  grows lowered `links` (`HostLink` = config spec + derived effective-default +
  host-veth name); `Endpoint` grows `default`; the default route is gated on
  effective ownership (configured, or sole interface). The fd-bridge ships
  `container:<name>` fds (linked containers only) alongside `router:<net>`;
  `link_connect` (phase-2 init parent) births virtual devices into the container
  netns via `net_ns_fd` (veth/macvlan/ipvlan) or moves a `phys` device in;
  `validate_link_anchors` fails fast (anchor exists/kind, macvlan-wireless, phys
  primary-NIC, macvlan⊕ipvlan-per-parent). The whole config surface is now wired —
  rootless baseline + rootful uplink + rootful links.
- **Open TODOs:** the running-container teardown guard (`# TODO` in `up`); veth
  name truncation for multi-network; the deferred `bridge` *network* type /
  multi-homing (CONFIG-SKETCH).

## Approach

- **Minimal pass, then refactor.** Each milestone first lands the smallest thing
  that works end-to-end from `turnip.json`, then a refactor pass extracts/grows
  the shared abstraction it revealed. Working code at every step; abstractions
  earn their place.
- **Pure modules, imperative shell.** The modules (`config` = model + validation,
  `netns`/`nftlib` = mechanism) hold **no env/IO reads and no mutable state**:
  they take explicit values. `main` is the shell — it reads the config file +
  environment, resolves the runtime (user/uid/state_dir), derives names, and
  passes concrete values in. So there's no module-level config or hidden global to
  reason about; everything env-dependent is resolved in one place.
- **Derivations as pure helpers from day one — in `main` while iterating.** The
  guardrail against the scatter an up-front IR was meant to prevent: name/path
  derivations (`router_netns_path`, `container_netns_path`, `netns_paths`, later
  `veth_name`, …) are small pure functions, living in `main` for now (no separate
  module until a refactor earns one). That *is* the proto-IR; it crystallizes into
  types/its own module only when a milestone's refactor calls for it.
- **Rootless first, rootful last.** Milestones 1–3 are the self-contained,
  no-privilege baseline (= today's behaviour, config-driven). 4–5 add the host
  edge, where the sudo/permissioning lives — kept out of the core path until then.
- **`fabric.py` dies incrementally**, its last import going at milestone 3.
  `verify.py` is retired, not rewired (see "What `verify` becomes").

## Compass — where the abstraction was heading (largely realized)

> **Update (M1–M3 done).** The three-entity shape below held, but it crystallized
> as a **stateful object graph** (`Container`/`Network`/`Endpoint` dataclasses in
> `main.py`, built by `build_model`), **not** a pure-IR `Layout` module — we don't
> reslice/serialize, so the graph with direct refs won out. Read `ContainerLayout`
> → `Container`, `NetworkLayout` → `Network` below; `links` is the one field still
> to land (M5). See "Runtime model" under Decisions for the rationale.

So refactors aim somewhere consistent, the eventual lowered IR mirrors config's
**three entities**, unified — not the network-centric shape first sketched, which
had no home for container-scoped `links` and fumbled the shared container netns:

```
Layout
  containers: list[ContainerLayout]   # netns, links, resolved default-route owner
  networks:   list[NetworkLayout]     # router netns, gateway, uplink, endpoints
Endpoint   # a lowered attachment, points back at a ContainerLayout
  container_ref, router_if, cont_if, ip, routes, egress, ingress
```

`container` is its own scope (it owns `links`, and one container multi-homed onto
N networks is *one* netns with N endpoints) — `config.py` confirms it: a top-level
`containers` map, and `_cross_cutting` gathers ifaces/defaults across `links` *and*
every `attach`. We don't build this graph up front; we let each milestone grow the
part it needs (M2 → `Endpoint`, M3 → `NetworkLayout`, M4 → uplink/egress, M5 →
`ContainerLayout`/`links`).

## Decisions already made

- **State layout:** `runtime.state_dir` (default `$XDG_RUNTIME_DIR/turnip`,
  fallback `/run/user/<uid>/turnip` — the user's runtime tmpfs, where rootless
  runtime state belongs; the netns can't outlive a reboot anyway) holds
  `routers/<network>` (router netns) + `containers/<container>/{netns,hosts}` — a
  per-container dir with its netns and generated hosts file. Symmetric,
  collision-free. `run-container.sh` joins `containers/<name>/netns` and bind-mounts
  `containers/<name>/hosts` → `/etc/hosts`. Teardown removes the netns + hosts but
  LEAVES the dir: rmdir hits EBUSY while the netns lazily unmounts (`MNT_DETACH`),
  and `create()`'s `makedirs(exist_ok)` reuses it.
- **nft table:** constant `table inet turnip` per router netns.
- **`resolve_runtime` rootless default:** fall back to the current login user, so
  the no-sudo path needs no explicit `runtime.user`; require an explicit user only
  when privileged (milestone 4).
- **Clean-slate `up = down() + build` (no granular idempotency):** `up` calls
  `down` first, then builds fresh — rather than reconciling in place. Skip-if-exists
  silently failed to apply config changes (change an IP → the existing veth is
  skipped → change lost); the network can't stay live across a rewire anyway (a
  container holds its netns by path, so a change needs it down); and it matches
  nft's flush-and-reload. Routing clean-slate *through* `down` (not a per-netns
  clobber) keeps **one teardown path**: when `down` grows host-side rootful state
  in M4 (host veth, masquerade, DNAT — state that doesn't die with the router
  netns), `up`'s clean-slate clears it for free, in `down`'s order, with no drift.
  *TODO:* refuse when a running container is attached to a target netns (teardown
  would orphan it) — for now the systemd unit orders containers down first.
- **Runtime model (a stateful object graph, not a pure IR):** `build_model` lowers
  the config into `Container`/`Network`/`Endpoint` dataclasses; the wiring is free
  functions over them. `Endpoint.container` is a **direct ref** to the shared
  `Container` (object graph), not a name to resolve — we don't reslice/serialize, so
  normalization would only add lookups + `model`-threading + lost type safety. Each
  netns-owning node carries its `netns_path` and, *while bound*, a live `.netns`
  handle (`.handle` raises if used unbound); ifindexes are never stashed (they go
  stale). `Model` **owns the netns lifetime** as methods: `create()` / `teardown()`
  for the persistent netns, and `bound()` (a generator-CM method, sibling to those)
  for the with-scoped sockets — `__exit__` must never remove the persistent netns.
- **Per-container hosts files (a projection, not stored back-refs):** `up` generates
  each container's `/etc/hosts` (localhost + self + the peers it may *initiate* to,
  per its directional outbound flows) via `container_peers`, a reverse traversal
  (container → endpoints → networks → flows → peer IP) computed over the forward
  graph. Promote to stored back-refs only if a second reverse consumer appears.

## Milestones

Each: a minimal pass (what lands), the refactor it motivates (what abstraction
grows), and how it's checked.

### 1. netns setup — DONE
- **Pass:** `up` creates `routers/<net>` + `containers/<container>` from `Turnip`;
  `down` removes them. `main()` loads config → resolves runtime → runs inside
  `in_podman_context`. Drags in: `state_dir` from `runtime.state_dir` (threaded as
  an arg, not a global); the current-user `resolve_runtime` fallback;
  `run-container.sh` → `containers/` path.
- **Refactor (done):** netns naming/paths are pure helpers in `main`
  (`router_netns_path`/`container_netns_path`/`netns_paths`); the two-scope shape
  (a `containers` loop + a `networks` loop) fell out naturally — the proto-IR, no
  types yet. Also moved all env/IO out of the modules into `main`: `config` is now
  pure model + validation (no `load`/`resolve_runtime`), `netns` takes explicit
  paths (no `_netns_dir` global). `verify.py` parked as `.bak` (it coupled to the
  old `path_for`/`fabric`).
- **Check (done):** live smoke — `up` creates the three netns as mounts (confirmed
  inside podman's mount ns), re-`up` rebuilds clean-slate (clobber, not skip),
  `down` removes clean.

### 2. netns linking — DONE
- **Pass (done):** `create_gateway` (the dummy gateway per network) + `connect`
  (the /32 routed veth pairs, container link-scope + default routes, the
  load-bearing router-side /32 route) driven by config `attach` entries. The
  netns/veth name derivations are pure helpers in `main` keyed by relative name
  (`router_netns`/`container_netns`/`router_if`).
  Non-router networks raise NotImplementedError (bridge is post-baseline).
  *(Note: the `open_namespaces` mapping introduced here was later folded into
  `Model.bound()` by the post-M3 runtime-model refactor.)*
- **Refactor (deferred at the time, now done):** at M2 the per-attachment facts
  were passed as `(network, container, att)` to `connect`; they since crystallized
  into the `Endpoint`/`Network` runtime objects (the post-M3 refactor — see
  "Runtime model" under Decisions).
- **Check (done):** live smoke — gateway `gw0` at `<gw>/32`, both `vethR-*` up with
  their /32 device routes, each container addressed with link-scope-gw + default
  routes; **container → gateway ping succeeds** (weak-host ARP answers, no
  `proxy_arp` needed yet). Inter-container forwarding is correctly *not* yet up:
  it needs `ip_forward`, which lands with the sysctls + nft policy in M3.

### 3. nft application — DONE
- **Pass (done):** `build_nft(network)` + `router_sysctls(network)` take a config
  `Network` and are applied in each router netns via the `run_in_netns` hop
  (`configure_dataplane`), after all wiring (per-veth sysctls + rp_filter need the
  veths to exist). Flows are directional (`from`→`to`), container↔gateway host
  pairs, icmp-to-gw; table `inet turnip`. `up` is now the full rootless baseline.
- **Refactor (done, post-M3):** the runtime object graph
  (`Container`/`Network`/`Endpoint` + `build_model`) landed as a follow-up — the
  derivations + parallel path/handle maps were the friction that earned it. The
  wiring/dataplane are now free functions over those objects; `build_nft` reads
  `network.gateway`/`endpoints`/`flows`. (Not a pure IR — see "Runtime model"
  under Decisions.)
- **Deleted:** `fabric.py` (last consumer gone). `verify.py` stays parked as
  `.bak` (reference for the future integration tests).
- **Check (done):** the **parity** test passes — config-driven `build_nft` for a
  config mirroring `fabric.py` reproduces the old literal golden byte-for-byte,
  modulo the table rename `fabric`→`turnip`. Live smoke: sysctls correct
  (ip_forward, per-veth rp_filter/proxy_arp, ipv6 off), `inet turnip` table loaded
  with the full matrix, gateway ping works, an **allowed** flow (zwave→hass:443)
  connects, and a **denied** one (zwave→proxy:443) is *dropped* — a timeout despite
  a live listener on proxy, i.e. the SYN was dropped at the router, not refused.
  End state: rootless baseline = today's behaviour, fully config-driven.
- *Note:* `port="any"`/icmp-in-flows raise NotImplementedError (need a second map
  shape) — deferred; the baseline carries concrete ports.

### 4. uplinks — DONE  *(rootful — host edge)*
First time turnip runs privileged. **The privilege plumbing is the hard part, not
the uplink** — see CONFIG-SKETCH "The uplink — and the rootful half it implies"
for the full model. Summary:
- **Two privilege sources** (decided this session): `sudo` (real root) **or** run
  as the user with `CAP_NET_ADMIN` (ambient cap — the systemd `User=` +
  `AmbientCapabilities` service path, no root). `requires_root` (any uplink/links)
  is the gate.
- **Forked into two branches** (the fork is forced by the userns split, not
  privilege): a host-edge branch in the init netns (host veth end, masquerade/DNAT
  in host nft, host route, `ip_forward`) and a netns branch in podman's ns. The
  parent keeps its privilege; the child becomes the user (**drop if root**, else
  no-op); they pass the router-netns fd via `SCM_RIGHTS`.
- **First step (do before any uplink logic):** harden `resolve_runtime`
  (privilege detection via euid/CapEff, require-explicit-user-under-root, uid-based
  dir resolution) and build + standalone-test the privileged-exec primitive
  (generalizes `in_podman_context`). Prove fork + host-netns op + netns op + fd
  pass in isolation first.
- **Then the uplink itself:** add `uplink` to the `Network` object and
  `egress`/`ingress` to `Endpoint` (more fields, additive — `build_model` lowers
  them); router-side allow rules in the existing nft table; host-side
  masquerade/DNAT in a host nft zone. `down` grows host-side teardown (flush the
  host nft zone; the veth + its routes mostly auto-die with the router netns) —
  one zone per network. `up = down() + build` clears it for free.
- **Check:** integration test — a permitted egress reaches out, an ingress DNAT
  lands; default-deny holds otherwise; re-`up` doesn't stack host nft rules.

### 5. links  *(rootful — container edge)*  — all slices DONE & VM-validated
Sliced for incremental landing (config modeled all four types already):
**slice 1 = the model refactor + the two `veth` types; slice 2 = `macvlan`/`ipvlan`;
slice 3 = `phys`.** All landed; slice 1 committed (`b609257`), slices 2–3 follow.

- **Pass:** container-scoped host-netdev holes (`veth`/`macvlan`/`ipvlan`/`phys`),
  own-vs-borrow ownership implied by `type`, teardown by name **and** kind.
- **Refactor (slice 1, done):** the `Container` object grows lowered `links`
  (`HostLink` = config spec + the two derived facts: effective default-route
  ownership and, for veth, the host-side veth name). The one-default-route
  invariant reads across attachments *and* links in one place — `build_model`
  resolves the *effective* default (configured, OR the container's sole interface)
  once, stored on `Endpoint.default` / `HostLink.default`, so `connect()` /
  `link_connect()` stay dumb. The fd-bridge keys split `router:<net>` /
  `container:<name>` (the latter only for linked containers).
- **Slice 1 mechanism (done):** `link_connect` (phase-2 init parent) mirrors
  `host_edge_connect` — births the container end via `net_ns_fd`, enslaves the
  host end to the bridge (`veth→bridge`) or leaves it bare (`veth→host`; turnip
  adds no host route — the deliberate point-to-point escape hatch). In-container
  config (addr/mac/mtu/routes/default) entered by fd. `validate_link_anchors`
  fails fast (init netns, before any build) on a missing/non-bridge anchor.
  `teardown_host_edge` deletes link host veths idempotently.
- **Check (slice 1, done — VM-validated):** link iface appears in the container
  with its static address + correct (gated) default route, **outside every
  router's nft policy** (zero nft refs); `veth→bridge` host end enslaved to the
  bridge; re-`up` is clean-slate (no stacked host veths); `down` leaves the bridge
  anchor untouched. **Empirical findings (kernel 6.18):** the cross-userns
  `net_ns_fd` birth into a *container* netns works (same cap-flow-down as the
  uplink); and a veth host end is **reaped with the container netns** on teardown
  (so the explicit host-veth delete is belt-and-suspenders, kept idempotent for
  kernels where it survives).
- **Slices 2–3 (done — VM-validated):** `macvlan`/`ipvlan` are born directly into
  the container netns off the host `parent` (`link=` IFLA_LINK resolved in init +
  `net_ns_fd` placing the device — the born-into-netns idiom works for both, no
  create-then-move needed). `phys` MOVES the existing device in (set `net_ns_fd`),
  renames it to the configured `name` in-netns, and is **borrowed** — relies on the
  kernel returning it to init on netns destroy (decided: no `return_phys`).
  `validate_link_anchors` grew: parent-exists, macvlan-wireless reject, phys
  primary/default-route-NIC reject, and a macvlan⊕ipvlan-share-a-parent reject.
- **Empirical findings (kernel 6.18, slices 2–3):** macvlan accepts a mode **name**
  via pyroute2 (`macvlan_mode="bridge"`) but ipvlan does **not** — `IFLA_IPVLAN_MODE`
  needs the numeric value (`_IPVLAN_MODE`: l2=0/l3=1/l3s=2; confirmed l3s round-trips).
  macvlan and ipvlan **cannot share a parent** (kernel EBUSY — a device is a macvlan
  master XOR ipvlan master), hence the new validation. phys validated with a dummy
  stand-in (the move/rename/borrow code path is identical; real-NIC auto-return is
  relied-upon kernel behavior, not turnip code).

## What `verify` becomes

Against a config-derived layout, structural `verify` is near-tautological: `up`
and any check lower from the *same* source, and `config.py` already rejects
malformed config at load — so it can only confirm `up` applied the config, never
that the config means what you wanted. With `verify` no longer a multi-consumer,
its two real jobs split and leave the CLI:

- **Convergence / drift** → **project integration tests** (env-gated: `up` →
  assert the dataplane → `down`). The per-milestone "Check (integration)" lines
  above. They need the live rootless podman context, so they're gated and kept
  separate from the pure helper/golden tests.
- **Effective-policy projection (intent)** → **deferred personal script.** Re-project
  policy into a view *orthogonal* to authoring — the per-container "what can this
  reach" grid, optionally probe-backed (zwave→hass:443 accepted, zwave→proxy
  dropped). Tied to a deployed config, not the core tool. Deferred.

Net: the rewire drops the `verify` command; `up`/`down` remain.

## Testing strategy

- **Helper/unit tests** (pure, no namespaces): grow with the derivation helpers —
  netns/veth naming + length rejection, container-route derivation, flow both-ways
  expansion. Fast, ungated.
- **nftlib golden** (`tests/test_nftlib.py`): regenerated at milestone 3 against
  the example config; the diff is the review artifact. Plus the **parity** golden.
- **Integration tests** (env-gated — require live rootless podman): the inheritor
  of structural `verify`, one per milestone from 2 on. Skipped where podman/caps
  are absent, so they never block the unit + golden tests.

## Still open

1. **Veth truncation/hash** — out of scope while N=1 (length check only); design
   the `(network, container) → ≤15` scheme when multi-network lands.
2. **Config discovery** — keep `$TURNIP_CONFIG` → `./turnip.json`, or add
   `--config`? (Carried from CONFIG-SKETCH open questions.)
