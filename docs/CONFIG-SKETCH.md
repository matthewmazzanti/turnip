# Declarative config — design & deferred-features spec

turnip's *model* is a declarative `turnip.json` — `runtime` + `containers` +
`networks` (with their `attach` rosters and `flows`) — parsed and validated by
`internal/config`. Everything else (`internal/dataplane`, `internal/netns`, the
`cmd/turnip` shell) is mechanism that consumes the parsed model; see
[ARCHITECTURE](ARCHITECTURE.md) for the layering. With multiple networks the model
is three entities — **containers**, **networks**, and the **attachments** that join
them.

This doc is the schema's design rationale and the spec for the **deferred** features
not yet built — the `bridge` network type, multi-homing, inter-network routing, the
`host`/`peer` flow types — each tagged **DEFERRED** where it appears. A
zero-construct counterpoint (the bare-substrate experiment) lives in
[CONFIG-SKETCH-substrate.md](CONFIG-SKETCH-substrate.md); it is explored, **not** the
target.

The security invariants stay as **code, not config**: rp_filter-strict,
ipv6-disabled, the implicit gateway + icmp allows, masquerade-only-at-the-WAN-boundary.
They *are* the routed model's guarantees; a config that could flip them is one that
can silently defeat the anti-spoof pin. The file describes the *model* (who exists,
who may talk, what crosses the edge), never the mechanism that secures it.

## The one idea — everything is a flow

All policy is a **flow**: a directed (proto, port) permission, discriminated by a
`type` that says what kind of edge it is. There are no `egress`/`ingress` markers on
attachments and no separate primitives — intra-network reachability, leaving for the
internet, and inbound port-forwards are all rows in one `flows` list per network.
`from`/`to` **always name containers**; the non-container end (the internet, the
host, a peer network) is implied by the flow's `type`, so a magic `"wan"` endpoint
that could collide with a container name never appears.

| `type` | direction | endpoints | extra | status |
|---|---|---|---|---|
| `internal` | `from` → `to` | two containers, this network | — | **baseline** |
| `egress` | `from` → internet | one container + the uplink | wide form `proto:"any"` | **baseline** |
| `ingress` | internet → `to` | one container + the uplink | `host_port` (DNAT), `listen` | **baseline** |
| `host` | `from` → the host | one container + the host addr | needs host input-chain | **DEFERRED** |
| `peer` | `from` → `net:to` | containers on peered networks | needs transit (see below) | **DEFERRED** |

The baseline three are a *reshape* of what turnip already does (the v1
`flows`+`egress`+`ingress`), into one relation — no new mechanism. `host` and `peer`
add new mechanism and are deferred.

## Scope — baseline vs deferred

The design here is the full target. The **baseline** (built first) is a deliberate
subset; the rest is sound but each piece drags in a whole subsystem, so it's
deferred — the design notes stay (their sections tagged **DEFERRED**) as the spec
for when each lands.

**Baseline**
- The routed model lifted to config: `runtime` + `containers` + a **single**
  routed `networks.<n>` with its `attach` roster and a `flows` list of
  `internal`/`egress`/`ingress` flows.
- The host edge: one `uplink` carrying egress (NAT/masquerade) + ingress (host
  DNAT), built on the rootful fork-drop/`SCM_RIGHTS` primitive.
- Container `links` — **`veth` only** (into a host bridge, or to the root netns).

**Deferred** (useful, but each adds disproportionate surface)
- **Multiple networks / multi-homing** — the per-network loop, the `default`-route
  flag, IFNAMSIZ veth naming. (The structure is already multi-network-ready;
  baseline just exercises N=1.)
- **`host` flow type** — host-local services (e.g. a host DNS resolver); needs a
  host *input*-chain allow, not just forward/nat. Closes the DNS open question.
- **`peer` flow type + network→network transit** — `net:container` reachability
  across a router↔router link; keep the "phases stay global, validate acyclicity"
  invariant in mind so it stays additive (see "Inter-network routing & NAT").
- **Non-veth `links`** (`macvlan`/`ipvlan`/`phys`) — dropped from the target;
  veth (bridge / root-netns) covers the current use case. (Trimmed from the code —
  see "Implementation delta.")
- **`bridge` network type** — a second security model + L2 enforcement.

## The model in one file — `turnip.json` (baseline)

Baseline: a single routed network with a host uplink, all policy in one `flows`
list. Deferred features appear in their (DEFERRED) sections below, not here. JSON has
no comments; with Nix as the authoring layer, annotations live in the Nix module
(see "Why JSON"). This is the on-disk artifact `turnip.example.json`.

```json
{
  "runtime": {
    "user": "homelab"
  },
  "containers": { "zwave": {}, "hass": {}, "proxy": {} },
  "networks": {
    "lan": {
      "gateway": "10.0.0.1",
      "gateway_if": "fabric0",
      "uplink": { "host_if": "veth-lan-host", "router_if": "vethR-lan-up", "link": "169.254.1.0" },
      "attach": {
        "zwave": { "ip": "10.0.0.11", "interface": "eth0" },
        "hass":  { "ip": "10.0.0.12", "interface": "eth0" },
        "proxy": { "ip": "10.0.0.13", "interface": "eth0" }
      },
      "flows": [
        { "type": "internal", "from": "zwave", "to": "hass", "proto": "tcp", "port": 443 },
        { "type": "egress",   "from": "zwave", "proto": ["udp","tcp"], "port": 53 },
        { "type": "egress",   "from": "hass",  "proto": "any" },
        { "type": "ingress",  "to": "proxy",   "proto": "tcp", "host_port": 8443, "port": 443 }
      ]
    }
  }
}
```

The attachment is now pure *placement* (`ip`, `interface`, `default`); every
who-may-talk decision is a flow. Opening `networks.lan` shows its whole population,
its internal policy, and its entire edge in one block.

## Three entities, three scopes

The old single "host" conflated a container with its one network presence. With N
networks it's a many-to-many, so it splits:

| entity | where | carries |
|---|---|---|
| **container** | `containers.<name>` | identity; `links` (router-independent veth holes); the one-default-route invariant |
| **network** (= a router netns) | `networks.<name>` | `gateway`, `gateway_if`, `uplink`, the `flows` list, and its `attach` roster |
| **attachment** (container × network) | `networks.<name>.attach.<container>` | `ip`, `interface`, `default` (keyed by container ⇒ unique per network, structurally) |

Everything stateful is **network-scoped** — the nft table, the gateway, the
host-facing uplink/DNAT all live *in* a router netns, so "two networks" replicates
each. `flows` is therefore per-network (a relation over members of the *same*
router's forward chain, plus that router's uplink). `links` bypass routers entirely,
so they live on the **container**.

**Why network-centric.** Putting the roster + `flows` + `uplink` under the network
gives the best *security-audit* view: open `networks.dmz` and see its whole
population, internal policy, and egress path in one block. The trade is that a few
**container-global** concerns (the single default route, interface-name uniqueness)
get written network-side and must be validated non-locally (see below) — and "what
does container X touch?" is spread across networks, recoverable by a reverse
traversal (the generated `/etc/hosts` already does this).

**Naming split.** Two different "attach" ideas collide, so they get distinct names:
**`links`** = an L2 veth hole into host networking (on the container); **`attach`** =
a container's membership in a network (under the network).

## Flows — the unified policy relation (router-only)

A flow is a directed (proto, port) permission. `from`/`to` name containers attached
to *this* network; `type` selects the edge and supplies the non-container end.
Directional — `from` may initiate to its target on (proto, port), and only that; the
return path rides conntrack, so the other direction is a second explicit flow.

**Shared fields.**
- **`proto`** — scalar (`"tcp"`) or list (`["udp","tcp"]`); fans out to one nft
  element per proto. `icmp` is portless. The explicit wildcard **`"any"`** means all
  L4 protocols and **carries no port** (`internal`/`egress` only).
- **`port`** — a concrete port (1-65535) or the explicit wide token `"any"`.
  Required whenever the proto is port-bearing; a dropped `port` is a **load error**,
  never a wildcard (the fail-closed pin).

**Per-type.**
- **`internal`** — `from` + `to`, both containers here. Enforced saddr.daddr at the
  forward chain.
- **`egress`** — `from` only; the target is the internet via the uplink. The wide
  "this container gets the internet" form is `{ "type":"egress","from":c,"proto":"any" }`
  — broad, but **deliberate and explicit**, unreachable by forgetting a field.
- **`ingress`** — `to` only; the source is the internet via the uplink. Carries
  **two** ports because it does DNAT: `host_port` is published on the host edge,
  `port` is the container port (defaults to `host_port` — the one widening-safe
  default). `proto` must be port-bearing (tcp/udp; no icmp, no `"any"`). Optional
  `listen` is the host address the DNAT binds (default `0.0.0.0`). Matched on
  **dest, not source** (client IP is a wildcard after DNAT); no SNAT — the return
  rides the host's conntrack DNAT reversal.

`egress`/`ingress` flows require the network to have an `uplink` (no path out
otherwise → the flow is meaningless → load error).

### Why flow-only (vs egress/ingress markers)

1. **One relation, one mental model.** Intra, out, and in are the same shape; the
   audit view is a single uniform table per network.
2. **No magic endpoints.** A type discriminator keeps `from`/`to` always meaning
   containers; there is no reserved `"wan"` string to collide with a container named
   `wan`, and the parser dispatches on `type` like every other union in the model.
3. **Locality is preserved by *which* list.** A flow rides the uplink of the network
   whose `flows` list it sits in — so `hass`'s egress on `lan` vs on `dmz` are simply
   two flows in two networks. The per-(container, network) distinction the old
   attachment-scoped egress carried is kept structurally, without a per-attachment
   field.
4. **Fail-closed survives.** Breadth is still opt-in and visible (`proto:"any"`,
   `port:"any"`); omission never widens.

## Network type — `router` (default) vs `bridge`

> **DEFERRED (post-baseline).** Baseline is `router`-only. A `bridge` type is a
> second security model (shared-L2 trust group + L2 enforcement); spec preserved.

A network carries a `type`. The default, `router`, is everything above: the routed,
/32, default-deny model. `bridge` is a *different trust primitive* — a shared L2
segment — not a variant of the same one.

| aspect | `router` (default) | `bridge` |
|---|---|---|
| topology | L3; /32 per container; router forwards by dest IP | L2; shared subnet; bridge switches by MAC |
| intra-network policy | default-deny **`flows`** matrix | **open trust domain** — no `flows` |
| anti-spoof | /32 route + strict rp_filter | **none by default** (shared L2 ⇒ ARP/MAC exposure is *accepted within the group*) |
| L2 services (bcast/mcast, mDNS/discovery) | none | **yes** — the reason to choose it |
| IPv6 | disabled | may be enabled |
| addressing | /32, no `subnet` | real `subnet`/`prefix` |
| edge (`egress`/`ingress` flows / `uplink`) | same | same |

The pivotal difference is **intra-network policy**: a router polices who-talks-to-whom
with the L3 `flows` matrix; a bridge is L2-switched, so its members **trust each
other by construction**, like a VLAN. If you need default-deny, use a router network.

Config/validation deltas: `type` defaults to `router`; `internal` flows are
**router-only** (a `bridge` can't enforce them — load error); `subnet` is
bridge-only and required (forbidden on a router, which is /32-everywhere); edge flows
(`egress`/`ingress`) and `attach` keep the same shape.

## Why JSON (Nix-driven)

The config is authored as a **Nix** attrset and emitted with `builtins.toJSON`, so
JSON is the natural on-disk format and Go's `encoding/json` loads it. JSON's usual
downside — no comments — is moot here: the authoring + documentation layer is Nix
(its `mkOption`s carry the docs, the module source carries the comments); the JSON is
a generated artifact, not hand-edited. Bonus: the polymorphic spots express cleanly
(the `type`-discriminated flow union, `proto`: string|list|"any", `port`: int|"any"),
and a JSON Schema can validate the emitted file independently of the Nix types.

## Global / runtime options — `runtime`

The *model* is separate from the *execution environment* (which user, dirs,
binaries), which goes in `runtime`:

- **`user`** — the unprivileged account that owns rootless podman and is the
  fork-drop target (`/run/user/<uid>` for the pause pid, `podman` run as the user).
  Default: `$SUDO_USER`. Explicit value decouples ownership from the invoker.
  Validated via `getpwnam`.
- **`state_dir`** — where turnip's runtime state lives: the netns mounts and the
  generated per-container hosts files, under `routers/<network>` +
  `containers/<container>/{netns,hosts}`. Default `$XDG_RUNTIME_DIR/turnip`
  (fallback `/run/user/<uid>/turnip`). *Shared* knowledge: must equal the `podman run
  --network ns:<state_dir>/containers/<name>/netns` path (and the hosts bind-mount
  source).
- **`nft` / `podman`** (optional) — binary-path overrides; default to the PATH +
  common-location search.

**Privilege is conditional on the model:** root is required *only when some network
has an `uplink` or some container has `links`* (the host edge needs the init netns).
A pure routed fabric with neither is self-contained.

Config discovery — *where the file is* — is the one global that can't live in the
file: `--config` → `$TURNIP_CONFIG` → `./turnip.json`.

## Validation & expansion rules

Governing rule, because this is default-deny: **omission must never widen.** Breadth
is opt-in and visible — an explicit `proto` list, `proto = "any"`, `port = "any"` —
never the result of a dropped field.

Expansion:
- **A `proto` list fans out** → one nft element per proto. ICMP must be named
  explicitly and carries no port; `proto = "any"` carries no port.
- **`port = "any"`** (the explicit wide token) → an all-ports rule for that proto.
- **`ingress.port` defaults to `host_port`** (the one widening-safe default).
- Flows are **directional** — one map element per flow; the other direction is a
  second, explicit flow.

Validation (load-time, fail fast):
- **A flow with a port-bearing `proto` but no `port` → error** (the fail-closed pin).
  `proto = "any"` or `icmp` with a `port` → error (they carry none).
- **`egress`/`ingress` flow on a network with no `uplink` → error** — no path out.
- **Flow `from`/`to` must be containers attached to *this* network** (reads locally —
  roster and flows are co-located under the network).
- **`internal` flow on a `bridge` network → error** (no L3 forward hop).
- **`ingress` `host_port` unique across *all* networks** per `(listen, proto)`,
  checked *after* proto fan-out — host ports are a single host-wide resource.

Cross-cutting, container-global checks (the price of the network-centric layout):
- **Exactly one `default = true` per container**, across all its attachments *and*
  links. Zero is fine only when the container has a single interface; >1 is an error.
- **Interface names unique within a container**, gathered from its `links` *and*
  every network `attach`.
- **Every `attach` key names a declared `containers.<name>`** — typos error.
- A container appearing **at most once per network** needs no check — keying `attach`
  by container makes a duplicate unrepresentable.

## Container links — veth "holes" into host networking

A network is controlled L3. A `link` is the opposite: a veth that gives the
container direct membership in a host-level domain (a host bridge, or the root
netns) — bypassing every router and its nft policy. This is part of the reason for
the rootful model. Links are **container-scoped** (they don't belong to any network)
and live in a `links` list. Any `link` makes the run rootful, like an `uplink`.

A link interface is **outside every network's default-deny** — `flows` don't apply
to it. Deliberate (a LAN trust escape), but it means a `link` is a conscious "this
container is trusted on that domain."

> **Scope: `veth` only.** The target supports the two veth anchors below.
> `macvlan`/`ipvlan`/`phys` are **dropped from the target** (the current use case is
> covered by veth) and have been trimmed from the code; see "Implementation delta."

### Config — the `links` list

```json
"links": [
  { "type": "veth", "bridge": "br-lan", "name": "eth2", "address": "192.168.1.13/24", "gateway": "192.168.1.1" },
  { "type": "veth", "peer": "host",     "name": "eth2", "address": "192.168.50.2/30" }
]
```

Row by row: veth→host-bridge (the host can share the segment); veth→root-netns
(point-to-point, host routes).

Fields:
- `type` (req): `veth` (the only type for now; kept as a discriminator for
  forward-compat).
- anchor (req, exactly one): `bridge=` (veth into a host bridge) **or**
  `peer="host"` (veth into the root netns).
- `name` (req): container-side iface name; unique within the container (≠ any network
  `interface`, ≠ another link). Stable, so host-side rules can match it.
- `address` (req): a static `"<CIDR>"`, set by the script. **DHCP is deferred** — it
  needs a renewing client *daemon* in the netns. Pick an address outside the LAN's
  DHCP pool; configure DNS separately.
- `gateway` / `routes` (optional). `mac`, `mtu` optional.
- `default` (optional bool): this link owns the container's default route (the same
  one-per-container flag that appears on attachments).

### Ownership: own vs borrow

A veth link is **virtual ⇒ owned by the script**: created from its anchor, moved in,
and reaped with the netns (virtual devices die with their netns, so "keep alive"
isn't an option; persistence you'd want is satisfied at the **anchor** — the
host-managed bridge is durable; the veth into it stays ephemeral, referenced by
**stable name**). The **anchor** (host bridge, root netns) is **borrowed**: validated
to exist + be the right kind, never created, never deleted — turnip adds only its own
member and removes only that on `down`.

### Validation & considerations
- **Rootful:** any `link` ⇒ root + fork-drop.
- **Anchor validated, never created:** fail clearly if absent; don't create host
  infra. (`bridge` must exist and be a bridge.)
- **Exactly one anchor:** a veth link needs exactly one of `bridge` or `peer:"host"`.
- **IPv6:** a link may take SLAAC independently — the v6-disabled invariant is scoped
  to the *router* netns, not the link iface.

## Multiple networks & multi-homing

> **DEFERRED (post-baseline).** The structure already supports it; baseline just
> runs one network.

- **`default`** marks which one interface owns the container's `0.0.0.0/0` route. A
  multi-homed container reaches each network's `/32` peers via that network's
  interface, and *everything else* via the `default` one's uplink (or a `link`'s
  gateway). Validated one-per-container (above).
- **Networks are isolated by default.** A multi-homed container is present on both
  but does **not** forward between them (no `ip_forward` in the container). Routing
  *between* networks is the deferred `peer` axis — explicit + default-deny, never
  implied by co-membership.
- **Interface-name length.** The router-side veth must encode `(network, container)`,
  but `IFNAMSIZ` is **15 chars**, so `veth-<net>-<container>` overflows for longer
  names. The mechanism needs a stable truncation/hash scheme; the loader should
  reject names that can't be encoded uniquely. (`routerIf` currently only rejects
  over-long — see `todo.md`.)
- **rp_filter stays valid multi-homed** — each network's `/32` + per-gateway routing
  keeps every interface's traffic symmetric.
- **Single-network stays terse.** Today's flat schema is just "one implicit
  network" — keep it as sugar that desugars to a single network.

## Inter-network routing & NAT — the `peer` flow type

> **DEFERRED.** Gated behind the `veth-name truncation/hash` work (a peer veth must
> encode `(netA, netB)` on top of the existing `(net, container)` pressure).

A network declares an explicit `peer` — the router↔router veth + its /31 — and a
`peer` flow rides it, naming a cross-network endpoint `<net>:<container>`:

```json
"dmz": {
  "peer":  { "lan": { "link": "169.254.9.0" } },
  "flows": [ { "type": "peer", "from": "proxy", "to": "lan:hass", "proto": "tcp", "port": 443 } ]
}
```

Invariants:
- **Explicit, never implied by co-membership.** The `peer` block declares the link;
  `peer` flows are default-deny like every other flow.
- **Non-transitive by default.** A peer reaches the *adjacent* network's containers,
  not "route through it to a third." So the **forwarding** graph stays acyclic by
  construction even when the **adjacency** graph has a cycle — preserving the
  "validate acyclicity / phases stay global" invariant.
- **Source identity preserved across the peer** — `proxy` arrives at `lan`'s forward
  chain as its own /32, so `from: proxy` is enforceable. This is the hard requirement
  that **forbids internal NAT** (below).

### Does NAT survive deep topologies (container → router → router)?

The short answer: **the DNAT/masquerade rules don't change — routing around them
does, and internal NAT is the wrong fix.**

- **One hop (baseline):** ingress really is "host DNATs dst→container/32, ip_forward
  + the subnet route carry it, the container sees the real client, the reply climbs
  back through the same host whose conntrack reverses the DNAT." Three things make it
  work and are trivially true at one hop: forward routability, return through the same
  conntrack, and a unique path for strict rp_filter.
- **Deep (container → R1 → R2 → host):** the host's DNAT rule is unchanged, but it
  must now reach the /32 *through a chain* — so **routes must be distributed across
  the transit DAG** (each router holds onward routes; `buildPlan` computes
  reachability), the reply must climb the *same* chain (or strict rp_filter drops the
  asymmetric leg), and **each /32 needs a unique path**. Depth costs route
  distribution + unique paths, **not** a new NAT primitive.
- **Internal NAT (SNAT at each hop) is rejected.** Its only forcing functions are
  refusing to distribute routes, non-unique paths, or *overlapping* address spaces —
  and it breaks the model decisively: **`flows` match on source address**, so a
  router that SNATs as traffic crosses makes a `peer` flow arrive wearing the transit
  router's address, unenforceable. (It would also turn ingress-through-a-chain into
  cascaded DNAT.)

So the stance, all loader-enforced: **masquerade only at the single WAN boundary;
routed transit inside; globally-unique container addresses + a DAG with a unique path
per /32** in any peered component. The one scenario that would *force* internal NAT —
address overlap — is forbidden instead, so the source-identity flows depend on is
never lost. (`egress_via` — an attachment riding a non-adjacent peer's uplink — rides
the same route-distribution machinery; deferred, additive once transit lands.)

## The uplink — and the rootful half it implies

A network is self-contained until it gets an `uplink`. An uplink is a veth between
that network's router netns and the **host** netns, which is what makes
egress/ingress flows (and `links`) possible — and it breaks two properties:

1. **Self-containment ends** — the router gets a default route via the host; the host
   forwards/masquerades that network's traffic to the WAN.
2. **`down` is no longer "rm netns = teardown."** The host netns now holds state that
   doesn't die with the router netns: the host-side veth, a route back to the
   network's subnet, and host nftables (masquerade + DNAT). That state is **rootful**.

So the tool grows a component that needs `CAP_NET_ADMIN` in the **init** userns.
turnip runs **rootful**: `sudo turnip up` (real root), dropping to the
rootless-podman owner to enter podman's namespaces.

### The fork is forced by the userns split, not by privilege

The work splits into two branches that **must** be separate processes:

- **host-edge branch** — runs in the **init** netns + init userns (uplink host ends,
  masquerade/DNAT in host nft, host route, `ip_forward`). Needs `CAP_NET_ADMIN` *in
  the init userns*.
- **netns branch** — enters podman's user+mount ns to create/wire the router/container
  netns. Needs to *be* the rootless user.

You can't do both in one process: entering podman's (descendant) userns is a one-way
descent that **loses caps over the init netns**. So `up` **forks**: the **parent is
the host-edge branch** (keeps whatever privilege it started with), the **child is the
netns branch** (drops to `runtime.user` iff it started as root). They hand the
router-netns fd across via `SCM_RIGHTS` — fd passing needs **no capability** (creds
are checked at `open()` and at the privileged op, never at transfer). **Root does NOT
change netns ownership** — even under sudo the persistent netns are created inside
podman's rootless user+mount ns.

What the edge flows expand to:
- **egress** — *rootless (router):* allow `ct new` out the uplink for permitted
  dests/ports; default route via the uplink. *rootful (host):* `ip_forward`, route the
  network's subnet → router, masquerade.
- **ingress** — *rootful (host):* `tcp dport <host_port> dnat to <ip>:<port>`.
  *rootless (router):* allow `iif uplink, ip daddr <ip>, <proto> dport <port>, ct new`
  — keyed on **dest, not source**. No SNAT.

## Security invariants — all preserved (router networks)

These are the guarantees of a **`router`-type** network. `bridge` networks have a
weaker posture by design (shared-L2 trust group); the items below are exactly what a
bridge trades away.

- `rp_filter` strict holds on every router veth (per network), including each uplink.
- IPv6 stays disabled per router → **no v6 WAN** (v4-only egress).
- Default-deny extends across each network's edge: nothing reaches the outside, and
  nothing is reachable from the host, unless an `egress`/`ingress` flow says so.
- Masquerade happens **only at the WAN boundary** (never internally) → source
  identity is preserved everywhere the `flows` matrix needs it.
- **Links are the deliberate exception.** A `link` iface is outside every router and
  its nft policy by design — an explicit trust grant, not a gap.

## How the model loads — implemented in `internal/config`

The schema is parsed and validated in `internal/config` (`config.go` /
`validate.go`): the `Turnip` graph, the polymorphic spots as real types (the
`type`-discriminated flow union, `proto` scalar|list|"any", `port` int|"any", the
`links` veth shape), unknown keys rejected so typos become load errors, and the
validation split into per-network rules + the container-global cross-cutting checks.
Discovery is `--config` → `$TURNIP_CONFIG` → `./turnip.json`. `HOST_PREFIX = 32` is
locked by topology, not configurable.

### Implementation delta (now carried through)

The code implements this target — the flow-only model on veth-only links. What the
reshape from the v1 three-primitive model entailed (all landed):
- **Policy moved off the attachment into typed flows.** `Attachment.Egress` /
  `Attachment.Ingress` are gone; the network carries a `type`-discriminated `Flow`
  union (`InternalFlow`/`EgressFlow`/`IngressFlow`). The wide egress is
  `{type:egress, proto:"any"}`; ingress rows are `{type:ingress, …}`.
- **`proto:"any"`** (a portless token, decoded by `egressProto`) expresses the wide
  egress form.
- **`links` reduced to `veth`.** `macvlan`/`ipvlan`/`phys` are removed from the
  config union *and* from `internal/dataplane/links.go`. The veth anchors (`bridge`,
  `peer:"host"`) stay.
- Validation re-homed onto flows (uplink-required, host_port uniqueness, endpoint
  membership) — a relocation of the existing checks.

## Open questions
- **Single-network sugar.** Offer a flat top-level shorthand that desugars to one
  implicit network, or require the explicit `networks.<name>` form always?
- **DNS.** A public resolver = an `egress` flow on udp/53 (+tcp/53). A host-local
  resolver = the deferred **`host` flow type** (container → the host addr), which
  needs a host input-chain allow.
- **Address auto-assignment.** Once `peer` requires globally-unique /32s, hand-picking
  non-colliding addresses gets tedious; an allocator (pool + persisted leases, read
  before `buildPlan`) starts to look attractive. See the references discussion in
  [CONFIG-SKETCH-substrate.md](CONFIG-SKETCH-substrate.md).
- **NAT vs routed egress.** The host edge always masquerades today; there is **no
  `nat` knob**. When routed egress is wired, `nat = false` would skip masquerade and
  instead need a static route for the subnet on your LAN router.
- **Hairpin** (a container reaching a sibling's published service via the host's
  external IP:host_port) is **out of scope** — it needs hairpin SNAT the routed model
  deliberately avoids.
