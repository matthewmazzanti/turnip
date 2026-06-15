# Declarative config — sketch

Today the fabric's *model* (already cleanly separated into `fabric.py`) is Python
source: `HOSTS`, `FABRIC_FLOWS`, `GW_IP`, `ROUTER`, etc. Everything else —
`main.py`, `nftlib.py`, `verify.py` — is pure mechanism that consumes that model.
So "make it declarative" means lifting that model out into a config file (JSON,
generated from a Nix module — see "Why JSON") and turning `fabric.py` into a
loader + validator. With multiple networks the model
grows from one fabric to three entities — **containers**, **networks**, and the
**attachments** that join them — but `main.py`/`nftlib.py`/`verify.py` still just
consume the parsed result.

The security invariants stay as **code, not config**: rp_filter-strict,
ipv6-disabled, the implicit gateway + icmp allows. They *are* the routed model's
guarantees; a config that could flip them is one that can silently defeat the
anti-spoof pin. The file describes the *model* (who exists, who may talk, what
crosses the edge), never the mechanism that secures it.

## Scope — baseline vs deferred

The design here is the full target. The **baseline** (build first) is a deliberate
subset; the rest is sound but each piece drags in a whole subsystem, so it's
deferred — the design notes stay (their sections tagged **DEFERRED**) as the spec
for when each lands.

**Baseline**
- The routed model lifted to config: `runtime` + `containers` + a **single**
  routed `networks.<n>` with its `attach` roster and `flows`.
- The host edge: one `uplink` with **egress + ingress** (NAT/masquerade + host
  DNAT), built on the rootful sudo/fork-drop/`SCM_RIGHTS` primitive as the
  reusable core (`wire_into(netns)` + keyed host zones).

**Deferred** (useful, but each adds disproportionate surface)
- **Multiple networks / multi-homing** — the per-network loop, the `default`-route
  flag, IFNAMSIZ veth naming. (The structure is already multi-network-ready;
  baseline just exercises N=1.)
- **Container `links` (holes)** + **link unification** (folding uplink + links into
  one default-deny `links` concept).
- **`bridge` network type** — a second security model + L2 enforcement.
- **Network→network** (transit / `egress_via` / `net:container` flows) — keep the
  "phases stay global, validate acyclicity" invariant in mind so it stays additive.

Build order: (1) loader/validator for the single-network routed model, rootless,
= today's behaviour declaratively; (2) the minimal uplink (egress + ingress) on
the rootful primitive; then the deferred set as needed.

## The model in one file — `fabric.json` (baseline)

Baseline: a single routed network with a host uplink (egress + ingress). Deferred
features — multiple networks, container `links`, `bridge` type — appear in their
(DEFERRED) sections below, not here. JSON has no comments; with Nix as the
authoring layer, annotations live in the Nix module (see "Why JSON"). This is the
on-disk artifact `fabric.example.json`.

```json
{
  "runtime": {
    "user": "homelab",
    "netns_dir": "/home/homelab/netns"
  },
  "containers": { "zwave": {}, "hass": {}, "proxy": {} },
  "networks": {
    "lan": {
      "gateway": "10.0.0.1",
      "fabric_if": "fabric0",
      "uplink": { "host_if": "veth-lan-host", "router_if": "vethR-lan-up", "link": "169.254.1.0/31", "nat": true },
      "attach": {
        "zwave": { "ip": "10.0.0.11", "interface": "eth0", "egress": [ { "proto": ["udp", "tcp"], "port": 53 } ] },
        "hass":  { "ip": "10.0.0.12", "interface": "eth0", "egress": true },
        "proxy": { "ip": "10.0.0.13", "interface": "eth0", "ingress": [ { "proto": "tcp", "host_port": 8443, "port": 443 } ] }
      },
      "flows": [ { "from": "zwave", "to": "hass", "proto": "tcp", "port": 443 } ]
    }
  }
}
```

## Three entities, three scopes

The old single "host" conflated a container with its one network presence. With N
networks it's a many-to-many, so it splits:

| entity | where | carries |
|---|---|---|
| **container** | `containers.<name>` | identity; `links` (router-independent holes); the one-default-route invariant |
| **network** (= a router netns) | `networks.<name>` | `gateway`, `fabric_if`, `uplink`, `flows`, and its `attach` roster |
| **attachment** (container × network) | `networks.<name>.attach.<container>` | `ip`, `interface`, `default`, `egress`, `ingress` (keyed by container ⇒ unique per network, structurally) |

Everything stateful is **network-scoped** — the nft table, the gateway, the
host-facing uplink/DNAT all live *in* a router netns, so "two networks" replicates
each. `flows` is therefore per-network (a relation between two members of the
*same* router's forward chain). `egress`/`ingress` ride a *specific* network's
uplink, so they live on the **attachment**. `links` bypass routers entirely, so
they live on the **container**.

**Why network-centric.** Putting the roster + `flows` + `uplink` under the network
gives the best *security-audit* view: open `networks.dmz` and see its whole
population, internal policy, and egress path in one block. The trade is that a few
**container-global** concerns get written network-side and must be validated
non-locally (see below) — and "what does container X touch?" is spread across
networks, which `verify` reconstructs as a per-container view.

**Naming split.** Two different "attach" ideas collide, so they get distinct
names: **`links`** = an L2 hole into host networking (on the container);
**`attach`** = a container's membership in a network (under the network).

## Network type — `router` (default) vs `bridge`

> **DEFERRED (post-baseline).** Baseline is `router`-only. A `bridge` type is a
> second security model (shared-L2 trust group + L2 enforcement); spec preserved.

A network carries a `type`. The default, `router`, is everything above: the
routed, /32, default-deny model. `bridge` is a *different trust primitive* — a
shared L2 segment — not a variant of the same one. (The repo deliberately
"retired the bridge" for routed; this brings it back only as an explicit,
opt-in network type.) The two coexist Docker-style: `router` for policed L3
isolation, `bridge` for an L2 trust group that needs discovery/L2 services.

| aspect | `router` (default) | `bridge` |
|---|---|---|
| topology | L3; /32 per container; router forwards by dest IP | L2; shared subnet; bridge switches by MAC |
| intra-network policy | default-deny **`flows`** matrix | **open trust domain** — no `flows` |
| anti-spoof | /32 route + strict rp_filter | **none by default** (shared L2 ⇒ ARP/MAC exposure is *accepted within the group*) |
| L2 services (bcast/mcast, **mDNS/discovery**) | none (no broadcast domain) | **yes** — the reason to choose it |
| IPv6 | disabled (severs inter-container v6) | may be enabled (an L2 path exists) |
| addressing | /32, no `subnet` | real `subnet`/`prefix` (the /32 lock is router-only) |
| edge (`egress`/`ingress`/`uplink`) | same | same |

The pivotal difference is **intra-network policy**. A router network polices
who-talks-to-whom with the L3 `flows` matrix (saddr.daddr enforced at the
forward chain). A bridge network is L2-switched, so intra-network traffic never
hits that chain — its members **trust each other by construction**, like a VLAN.
That's the point of choosing it, not a gap; but it means a bridge network's
security posture is "trusted L2 group," and its anti-spoof guarantees (the /32 +
rp_filter pin, ipv6-disable) simply don't apply. Re-imposing default-deny on a
bridge is *possible* (bridge-family nft + per-port MAC/ARP hygiene) but rebuilds
exactly the L2 hygiene the routed model exists to avoid — not worth it; if you
need default-deny, use a router network.

```json
{
  "networks": {
    "iot": {
      "type": "bridge",
      "subnet": "10.2.0.0/24",
      "gateway": "10.2.0.1",
      "uplink": { "host_if": "veth-iot-host", "router_if": "vethR-iot-up", "link": "169.254.2.0/31", "nat": true },
      "attach": {
        "sensor": { "ip": "10.2.0.10", "interface": "eth0", "egress": true }
      }
    }
  }
}
```
(`type` = "bridge"; a real `subnet` instead of /32; **no** `flows` — intra-network
traffic is open by construction; edge policy is still per-attachment.)

Config/validation deltas (everything else — skeleton, deployment phases,
privilege model, `links`, multi-homing — is unchanged):
- **`type`** defaults to `router`, so secure-by-default is the one you fall into.
- **`flows` is router-only** — a `flows` key on a `bridge` network is a load
  error (it can't be enforced where there's no L3 forward hop).
- **`subnet` is bridge-only and required** — and conversely forbidden on a router
  network (which is /32-everywhere). This is the `prefix` knob, reintroduced but
  scoped to `bridge`.
- Edge (`uplink`/`egress`/`ingress`) and `attach` keep the same shape; only `ip`
  changes meaning (a subnet address vs a /32).

## Why JSON (Nix-driven)

The config is authored as a **Nix** attrset and emitted with `builtins.toJSON`, so
JSON is the natural on-disk format and `json` (stdlib) loads it. JSON's usual
downside — no comments — is moot here: the authoring + documentation layer is Nix
(its `mkOption`s carry the docs, the module source carries the comments); the JSON
is a generated artifact, not hand-edited. Bonus: the polymorphic spots express
cleanly (`egress`: bool|list, `proto`: string|list, `port`: int|"any"), and a JSON
Schema can validate the emitted file independently of the Nix types. TOML/`tomllib`
would also work for hand-authoring but loses the Nix-generation path; YAML adds a
dependency for nothing.

## Global / runtime options — `runtime`

The *model* is separate from the *execution environment* (which user, dirs,
binaries), which goes in `runtime`:

- **`user`** — the unprivileged account that owns rootless podman and is the
  fork-drop target (`/run/user/<uid>` for the pause pid, `sudo -u <user> podman
  …`). Default: `$SUDO_USER`. Explicit value decouples ownership from the invoker
  (admin runs `sudo fabric up`; it drops to `homelab`). Validated via `getpwnam`.
- **`netns_dir`** — where persistent netns mounts live. Default `<user>`'s
  `$HOME/netns`. *Shared* knowledge: must equal the `podman run --network
  ns:<netns_dir>/<name>` path.
- **`nft` / `podman`** (optional) — binary-path overrides; default to the PATH +
  common-location search.

**Privilege is conditional on the model:** sudo is required *only when some
network has an `uplink` or some container has `links`* (the host edge needs
root). A pure routed fabric with neither is the self-contained rootless tool of
today — run it directly as `user`, no drop, no sudo.

Config discovery — *where the file is* — is the one global that can't live in the
file: `$FABRIC_CONFIG` / `--config` (see open questions).

## The two edges — `egress` / `ingress` (on the attachment)

`egress`/`ingress` belong to an **attachment** because they ride that network's
uplink — `hass`'s egress on `lan` may differ from its egress on `dmz`.

**`egress`** — outbound. A bool or a list of scoped rules:
- `egress = true` → any external dest/proto/port. The one wide form, safe because
  it's a deliberate `true` — unreachable by forgetting a field.
- `egress = [ {proto, port}, … ]` → only those. `proto` and `port` are both
  **required**; `proto` may be a list (`["udp","tcp"]`). Omitting either is a load
  error, never a wildcard.
- absent / `false` / `[]` → no egress (default-deny).

**`ingress`** — inbound, a list of host→container DNAT mappings:
- `{ proto, host_port, port }` — `host_port` is published on the host edge, `port`
  is the container port (carries **two** ports because it does DNAT; `port`
  defaults to `host_port` — the one widening-*safe* default). `proto` required.

## Validation & expansion rules

Governing rule, because this is default-deny: **omission must never widen.**
Breadth is opt-in and visible — an explicit `proto` list, `port = "any"`, or the
deliberate `egress = true` — never the result of a dropped field. A missing
`proto`/`port` is a *load error*, not a wildcard.

Expansion:
- **A `proto` list fans out** → one nft element per proto. ICMP must be named
  explicitly and carries no port.
- **`port = "any"`** (the explicit wide token) → an all-ports rule for that proto.
- **`ingress.port` defaults to `host_port`** (the one widening-safe default).
- `flows` entries expand to both directions.

Validation (load-time, fail fast):
- **A scoped `egress`/`ingress`/`flows` rule missing `proto` or `port` → error**
  (the fail-closed pin).
- **`egress`/`ingress` on an attachment whose network has no `uplink` → error**
  — no path out, so the rule is meaningless.
- **`flows` endpoints must be containers attached to *that* network** (reads
  locally — roster and flows are co-located under the network).
- **`ingress` `host_port` unique across *all* networks** per `(listen, proto)`,
  checked *after* proto fan-out — host ports are a single host-wide resource, so
  the collision check spans every network's uplink, not just one.

Cross-cutting, container-global checks (the price of the network-centric layout —
these gather across scattered locations):
- **Exactly one `default = true` per container**, across all its attachments *and*
  links. Zero is fine only when the container has a single interface; >1 is an
  error.
- **Interface names unique within a container**, gathered from its `links` (on the
  container) *and* every network `attach` (on the networks). `eth0`, `eth1`,
  `lan0` must not collide.
- **Every `attach` key names a declared `containers.<name>`** — networks may
  only attach containers that exist (typos error, not conjure a netns).
- A container appearing **at most once per network** needs no check — keying
  `attach` by container makes a duplicate unrepresentable (JSON objects and Nix
  attrsets both reject duplicate keys). The `(network, container)` pair is the
  attachment's primary key.

## Multiple networks & multi-homing

> **DEFERRED (post-baseline).** The structure already supports it; baseline just
> runs one network. This is the spec for when multi-network lands.

- **`default`** marks which one interface owns the container's `0.0.0.0/0` route.
  A multi-homed container reaches each network's `/32` peers via that network's
  interface, and *everything else* via the `default` one's uplink (or a `link`'s
  gateway). Validated one-per-container (above).
- **Networks are isolated by default.** A multi-homed container is present on both
  but does **not** forward between them (no `ip_forward` in the container). Routing
  *between* networks is a deliberately-deferred next axis — and when added it must
  be explicit + default-deny (a transit policy between two routers), not implied
  by co-membership.
- **Interface-name length.** The router-side veth must encode `(network,
  container)`, but `IFNAMSIZ` is **15 chars** — `vethR-…` is already long, so
  `veth-<net>-<container>` overflows for longer names. The mechanism needs a
  stable truncation/hash scheme; the loader should reject names that can't be
  encoded uniquely.
- **rp_filter stays valid multi-homed** — each network's `/32` + per-gateway
  routing keeps every interface's traffic symmetric, so strict rp_filter per
  router holds. (Watch the *container* netns's own rp_filter only if you later add
  asymmetric routes.)
- **Single-network stays terse.** Today's flat schema is just "one implicit
  network" — keep it as sugar that desugars to a single network so the common case
  doesn't pay the multi-network verbosity tax.

## The uplink — and the rootful half it implies

A network is self-contained until it gets an `uplink`. An uplink is a veth
between that network's router netns and the **host** netns, which is what makes
`egress`/`ingress` (and `links`) possible — and it breaks two properties:

1. **Self-containment ends** — the router gets a default route via the host; the
   host forwards/masquerades that network's traffic to the WAN.
2. **`down` is no longer "rm netns = teardown."** The host netns now holds state
   that doesn't die with the router netns: the host-side veth, a route back to the
   network's subnet, and host nftables (masquerade + DNAT). That state is
   **rootful**. (With multiple uplinks, the host holds one such zone per network.)

So the tool grows a root-required component. The chosen model: **run the whole
thing under `sudo`, and drop privilege for the netns ops** — one invocation,
iterated per network.

Privilege model (mechanism — the `uplink`/`egress`/`ingress`/`links` config
drives it):

- **Root does NOT change netns ownership.** Rootless `podman run` can only
  `setns()` into netns owned by podman's rootless userns — a property of the
  *containers*. So even under sudo, the persistent netns are still created inside
  podman's user+mount ns.
- **Fork and drop, don't `su`.** A literal `su` is irrecoverable and loses root
  for the host work. Instead: fork; the **parent stays root** for the host edge
  (uplink host ends, masquerade/DNAT, host sysctls); the **child drops to
  `runtime.user`'s uid/gid** (initgroups → setresgid → setresuid) and runs the
  podman-context body. Becoming the login user (vs root entering the userns) is
  the proven path: it maps to uid 0 inside podman's userns via the uid_map, and
  the `setns(CLONE_NEWUSER)` join is permitted by owner-match (`euid == owner`).
- **An uplink veth crosses a userns boundary** (root netns is init-userns; a
  router netns is rootless-userns; caps propagate to descendants, never up). The
  child (in podman's mount ns, where the router-netns bind-mount is visible)
  **opens the router-netns fd and hands it to the root parent over a `socketpair`
  via `SCM_RIGHTS`**. Fd passing needs **no capability** — creds are checked at
  `open()` and at the privileged op (`setns`/the `net_ns_fd` move), never at
  transfer. So the child's mount-ns visibility does the opening and the root
  parent's caps do the move.
- **sudo breaks the login-user assumptions:** `_pause_pid()` reads
  `/run/user/<runtime.user uid>/...` (bootstrap via `sudo -u <user> podman
  unshare true`); `NETNS_DIR` comes from `runtime.netns_dir`, not root's
  `$HOME`. With an uplink/link present the script refuses to run without sudo, and
  refuses real-root with no resolvable `runtime.user`/`$SUDO_USER`.

What the edges expand to:
- **egress** — *rootless (router):* allow `ct new` out the uplink for permitted
  dests/ports; default route via the uplink. *rootful (host):* `ip_forward`, route
  the network's subnet → router, masquerade.
- **ingress** — *rootful (host):* `tcp dport <host_port> dnat to <ip>:<port>`.
  *rootless (router):* allow `iif uplink, ip daddr <ip>, <proto> dport <port>, ct
  new` — keyed on **dest, not source** (client IP wildcard after DNAT). No SNAT
  needed: return rides the host's conntrack DNAT reversal.

## Container links — "holes" into host networking

> **DEFERRED (post-baseline).** Not in the first cut. When it lands it folds into
> the unified default-deny `links` concept (uplink + links, `allow = "open" |
> [rules]`, soft/`strict` enforcement). Spec and ownership doctrine preserved below.

A network is controlled L3. A `link` is the opposite: it moves a host netdev into
a container so the container gets direct membership in some host-level domain (the
LAN, a host bridge, a VLAN) — bypassing every router and its nft policy. This is
the whole reason for the rootful/sudo model. Links are **container-scoped** (they
don't belong to any network) and live in a `links` list. Any `link` makes the run
rootful, like an `uplink`.

A link interface is **outside every network's default-deny** — `flows`/`egress`/
`ingress` don't apply to it. Deliberate (a LAN trust escape), but it means a
`link` is a conscious "this container is trusted on that domain."

### Ownership: own vs borrow

A link has three layers, each with a different owner. The script **owns what it
creates and what dies with the namespace; borrows what the host provisions and
what outlives it** — and the kernel's own lifecycle draws the line:

| layer | examples | owner | `up` | `down` |
|---|---|---|---|---|
| **anchor** | host bridge, parent NIC, VLAN, root netns | host (borrow) | validate exists + right kind; add our member | remove only our member; never delete |
| **entity — virtual** | veth, macvlan, ipvlan | script (own) | create from anchor, move in | destroy (or reaped with the netns) |
| **entity — physical** | NIC, SR-IOV VF | host (borrow) | move in | return to root; never delete |
| **in-ns config** | address, route | script (static) | set address/route | gone with the netns |

Ownership is **implied by `type`**, never a flag: veth/macvlan/ipvlan are virtual
⇒ owned; `phys` is physical ⇒ borrowed. The kernel enforces it — virtual devices
die with their netns (so "keep alive" isn't an option), physical devices return to
root on netns destroy (so "keep alive" is automatic). Persistence you'd want for a
virtual entity is satisfied at the **anchor** (the host-managed bridge is durable);
the veth into it stays ephemeral, referenced by **stable name**.

Teardown doctrine: destroy only entities the script created — matched by **name
AND kind** (`IFLA_INFO_KIND`; absent ⇒ physical), idempotent, refusing to delete
anything it didn't make. Return phys to root. Never touch anchors.

### Config — the `links` list

One shape covers the menu: `{ type, <anchor-ref>, name, address, … }`. The
`links` value (a list) on a container — one comment-annotated entry per row:

```json
"links": [
  { "type": "macvlan", "parent": "eth0", "mode": "bridge", "name": "lan0",
    "address": "192.168.1.12/24", "gateway": "192.168.1.1" },

  { "type": "veth", "bridge": "br-lan", "name": "eth2", "address": "192.168.1.13/24" },

  { "type": "ipvlan", "parent": "wlan0", "mode": "l2", "name": "lan0",
    "address": "192.168.1.50/24", "gateway": "192.168.1.1" },

  { "type": "veth", "peer": "host", "name": "eth2", "address": "192.168.50.2/30" },

  { "type": "phys", "dev": "enp3s0", "name": "eth2", "address": "192.168.1.20/24", "default": true }
]
```
Row by row: macvlan (own MAC/IP on the LAN; mDNS works; host↔child isolated);
veth→host-bridge (most flexible; host can share the segment); ipvlan L2 (single
MAC — works on WiFi); veth→root-netns (point-to-point, host routes); phys (a
BORROWED NIC/VF — moved in, returned on down, never deleted).

Fields:
- `type` (req): `veth` | `macvlan` | `ipvlan` | `phys`.
- anchor ref (req, by type): `bridge=` (veth→bridge), `parent=` (macvlan/ipvlan),
  `peer="host"` (veth→root netns), `dev=` (phys — the device itself; no anchor).
- `name` (req): container-side iface name; unique within the container (≠ any
  network `interface`, ≠ another link). Stable, so host-side rules can match it.
- `address` (req): a static `"<CIDR>"`, set by the script. **DHCP is deferred** —
  it needs a renewing client *daemon* in the netns (the image's job, or the script
  becomes a supervisor); neither fits configure-and-exit. Pick an address outside
  the LAN's DHCP pool; configure DNS separately.
- `gateway` / `routes` (optional). `mode`: macvlan `bridge`(default)|`private`|
  `vepa`|`passthru`; ipvlan `l2`(default)|`l3`|`l3s`. `mac`, `mtu` optional.
- `default` (optional bool): this link owns the container's default route (the
  same one-per-container flag that appears on attachments).

### Validation & considerations
- **Rootful:** any `link` ⇒ sudo + fork-drop.
- **Anchor validated, never created:** fail clearly if absent; don't create host
  infra.
- **Hard rejects:** `macvlan` over a wireless `parent` (won't work — use
  `ipvlan`); `phys` on a netns-local/wireless device (`NETIF_F_NETNS_LOCAL` can't
  be moved — wifi needs the unbuilt `iw phy` path); `phys` on the host's primary/
  default-route NIC.
- **Soft warning:** `ipvlan mode = "l3"` kills broadcast/multicast → mDNS/discovery
  silently break; warn since the move "works" but the use case doesn't.
- **macvlan host isolation:** the host can't reach its own macvlan children on that
  parent (a host-side shim fixes it).
- **IPv6:** a link may take SLAAC independently — the v6-disabled invariant is
  scoped to the *router* netns, not the link iface.

## Security invariants — all preserved (router networks)

These are the guarantees of a **`router`-type** network. `bridge` networks have a
different, weaker posture by design (shared-L2 trust group — see "Network type");
the items below are exactly what a bridge trades away.

- `rp_filter` strict holds on every router veth (per network), including each
  uplink (reverse path for an internet source = the default route = that uplink).
- IPv6 stays disabled per router → **no v6 WAN** (v4-only egress).
- Default-deny extends across each network's edge: nothing reaches the outside,
  and nothing is reachable from the host, unless an `egress`/`ingress` says so.
- **Links are the deliberate exception.** A `link` iface is outside every router
  and its nft policy by design — an explicit trust grant, not a gap.

## How `fabric.py` changes

From "module of literals" to "loader + validator." Downstream now iterates
networks (one router netns, nft table, and uplink each) rather than the single
`router`; `fabric.py` returns the parsed containers/networks/attachments.

```python
import os, json, pwd

HOST_PREFIX = 32   # locked by topology; not configurable

def _require(d, *keys):                       # fail closed: omission never widens
    for k in keys:
        if k not in d:
            raise ValueError(f"{d!r} missing required {k!r}")

def _runtime(cfg):
    rt = cfg.get("runtime", {})
    user = rt.get("user") or os.environ.get("SUDO_USER")
    if not user:
        raise ValueError("set runtime.user or run via sudo ($SUDO_USER)")
    pw = pwd.getpwnam(user)
    return {"user": user, "uid": pw.pw_uid, "gid": pw.pw_gid,
            "netns_dir": rt.get("netns_dir") or os.path.join(pw.pw_dir, "netns"),
            "nft": rt.get("nft"), "podman": rt.get("podman")}

def _load(path):
    with open(path) as f:
        cfg = json.load(f)
    runtime = _runtime(cfg)
    containers = {name: {"links": c.get("links", [])}
                  for name, c in cfg.get("containers", {}).items()}

    networks, ports, defaults, ifaces = {}, {}, {}, {}   # last three: cross-cutting
    for cname, c in containers.items():                  # seed iface/default from links
        for ln in c["links"]:
            ifaces.setdefault(cname, set())
            if ln["name"] in ifaces[cname]:
                raise ValueError(f"{cname}: duplicate iface {ln['name']!r}")
            ifaces[cname].add(ln["name"])
            if ln.get("default"):
                defaults[cname] = defaults.get(cname, 0) + 1

    for net, n in cfg["networks"].items():
        uplink = n.get("uplink")
        members, attach = set(), {}
        for cname, a in n.get("attach", {}).items():     # keyed by container = unique per net
            _require(a, "ip", "interface")
            if cname not in containers:
                raise ValueError(f"network {net}: unknown container {cname!r}")
            members.add(cname)
            ifaces.setdefault(cname, set())
            if a["interface"] in ifaces[cname]:
                raise ValueError(f"{cname}: duplicate iface {a['interface']!r}")
            ifaces[cname].add(a["interface"])
            if a.get("default"):
                defaults[cname] = defaults.get(cname, 0) + 1
            eg, ing = a.get("egress"), a.get("ingress", [])
            if (eg or ing) and uplink is None:
                raise ValueError(f"{net}/{cname}: egress/ingress needs this network's uplink")
            if isinstance(eg, list):
                for r in eg: _require(r, "proto", "port")
            for fwd in ing:                              # host_port unique across ALL networks
                _require(fwd, "proto", "host_port")
                p = fwd["proto"]; protos = p if isinstance(p, list) else [p]
                for proto in protos:
                    key = (fwd.get("listen", "0.0.0.0"), proto, fwd["host_port"])
                    if key in ports:
                        raise ValueError(f"host_port collision {key}: {cname} vs {ports[key]}")
                    ports[key] = cname
            attach[cname] = a
        flows = []
        for fl in n.get("flows", []):
            _require(fl, "from", "to", "proto", "port")
            for end in (fl["from"], fl["to"]):
                if end not in members:                   # endpoints attached to THIS network
                    raise ValueError(f"network {net}: flow endpoint {end!r} not attached here")
            flows.append((fl["from"], fl["to"], fl["proto"], fl["port"]))
        networks[net] = {"gateway": n["gateway"], "fabric_if": n["fabric_if"],
                         "uplink": uplink, "flows": flows, "attach": attach}

    for cname, n in defaults.items():                    # exactly one default per container
        if n > 1:
            raise ValueError(f"{cname}: {n} interfaces marked default; pick one")

    return {"runtime": runtime, "containers": containers, "networks": networks}

CONFIG = _load(os.environ.get("FABRIC_CONFIG", "fabric.json"))
```

## Open questions
- **Config discovery.** `$FABRIC_CONFIG` (sketched) vs a `--config` flag.
- **Single-network sugar.** Offer a flat top-level shorthand that desugars to one
  implicit network, or require the explicit `networks.<name>` form always?
- **Inter-network routing.** Deferred; when added, an explicit default-deny transit
  policy between two routers (never implied by co-membership).
- **DNS.** A public resolver = an `egress` rule on udp/53 (+tcp/53). A host-local
  resolver is a different shape (egress/ingress to the host addr). Per deployment.
- **NAT vs routed egress.** `nat = true` (masquerade) is the home default; routed
  (`nat = false`) needs a static route for the subnet on your LAN router.
- **Per-flow direction.** `flows` are bidirectional-initiate; add optional
  `direction` only if one-way is ever wanted.
```
