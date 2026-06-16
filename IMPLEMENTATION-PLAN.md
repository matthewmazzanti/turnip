# Implementation plan — config → mechanism, by milestone

The model is declarative (`config.py` → `Turnip`). The mechanism
(`main.py`/`nftlib.py`/`netns.py`) still runs on the hardcoded `fabric.py`
literals. This is the plan to close that gap **bottom-up**: take a minimal,
working vertical pass at each milestone, then refactor to grow the abstractions
(the netns/veth/addressing derivations, and eventually a lowered IR) out of code
that already runs — rather than designing the IR first and discovering its shape
is wrong (it already was; see "Compass" below).

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

## Compass — where the abstraction is heading (not a spec)

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
  (`router_netns`/`container_netns`/`router_if`); `open_namespaces` finalized to a
  `{name: path}` mapping, shaped here by the wiring loop (its first consumer).
  Non-router networks raise NotImplementedError (bridge is post-baseline).
- **Refactor (deferred):** the per-attachment derived facts (`router_if`,
  `cont_if`, `ip`, gateway) are still passed as `(network, container, att)` to
  `connect` — readable enough that a typed `Endpoint` hasn't earned itself yet. It
  likely crystallizes at M3, where the nft builder also needs the resolved per-
  container IPs; let it emerge there rather than forcing it now.
- **Check (done):** live smoke — gateway `gw0` at `<gw>/32`, both `vethR-*` up with
  their /32 device routes, each container addressed with link-scope-gw + default
  routes; **container → gateway ping succeeds** (weak-host ARP answers, no
  `proxy_arp` needed yet). Inter-container forwarding is correctly *not* yet up:
  it needs `ip_forward`, which lands with the sysctls + nft policy in M3.

### 3. nft application — DONE
- **Pass (done):** `build_nft(network)` + `router_sysctls(network)` take a config
  `Network` and are applied in each router netns via the `run_in_netns` hop
  (`configure_dataplane`), after all wiring (per-veth sysctls + rp_filter need the
  veths to exist). Flows expand both-ways, container↔gateway host pairs, icmp-to-gw;
  table `inet turnip`. `up` is now the full rootless baseline.
- **Refactor (deferred again):** `build_nft`/`router_sysctls` read
  `network.gateway`/`attach`/`flows` directly — clean enough that the lowered
  `NetworkLayout`/`Endpoint` still hasn't earned itself. Same call as M2; let it
  crystallize only when a consumer makes it pay (e.g. M4's edge, or multi-network).
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

### 4. uplinks  *(rootful — host edge)*
- **Pass:** per-network `uplink` veth (router netns ↔ host netns) + `egress`/
  `ingress` (router-side allow rules; host-side masquerade/DNAT/route), on the
  sudo / fork-drop / `SCM_RIGHTS` primitive (CONFIG-SKETCH "the rootful half").
  Brings in the explicit-user requirement and the `requires_root` gate.
- **Refactor:** `uplink` on `NetworkLayout`, `egress`/`ingress` on `Endpoint`; the
  privileged-execution primitive abstracted as the reusable `wire_into(netns)`.
- **Check:** integration test — a permitted egress reaches out, an ingress DNAT
  lands; default-deny holds otherwise.

### 5. links  *(rootful — container edge)*
- **Pass:** container-scoped host-netdev holes (`veth`/`macvlan`/`ipvlan`/`phys`),
  own-vs-borrow ownership implied by `type`, teardown by name **and** kind.
- **Refactor:** `ContainerLayout` grows `links` — the unified container scope
  finally crystallizes (the thing the network-centric sketch couldn't hold). The
  one-default-route invariant now reads across attachments *and* links in one place.
- **Check:** integration test — a link iface appears in the container with its
  static address, outside every router's nft policy; `down` returns `phys`, reaps
  virtual, never touches anchors.

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
