# Architecture — the layers (Go rewrite)

How `turnip up`/`down` are structured: a **pure core** that lowers the declarative
config into a fully-resolved description, wrapped by an **imperative shell** that
pushes that description onto live network namespaces. The split is the same one the
Python had (`config` = model, `main` = shell) but drawn sharper — the boundary
between "decide" and "do" is now a *type* (`Plan`), not a comment.

```
turnip.json ──▶  config  ──▶  Plan  ──▶  apply  ──▶  kernel
                (parse +     (lower,    (drive netns    (netlink / nft /
                 validate)    pure)      Set by fd)      sysctl / mount)
                    │           │           │                 │
              internal/config  model.go   apply.go      internal/dataplane
                                          up.go         internal/netns
                                       (the shell)      (the mechanism)
```

Two axes to keep separate while reading this:

- **Decide vs. do** — `config` + `Plan` are pure (no IO, no fds, no root); `apply` +
  the mechanism packages are effectful. Everything that can *fail to resolve* lives
  on the decide side, so the do side is total over a valid `Plan`.
- **Policy vs. mechanism** — `cmd/turnip` owns *sequencing and IO*; `internal/dataplane`
  and `internal/netns` own *capabilities*. The shell decides the order; the mechanism
  knows how to make a gateway / enter a namespace.

---

## Layer 1 — Config (the model)

**Where:** `internal/config` (`config.go`, `validate.go`). **Purity:** pure.

The declarative `turnip.json` parsed into the `Turnip` graph — `runtime`, `containers`,
`networks` with their `attach` rosters, `flows`, `uplink`, and per-container `links`.
This is the user's **intent**: symbolic (containers named, not addressed), optional
(defaults unfilled), and nested by *ownership* (a network's ingress lives under the
attachment that owns it).

The security invariants are **code, not config** (rp_filter-strict, ipv6-off, the
implicit gateway/icmp allows) — see [CONFIG-SKETCH](CONFIG-SKETCH.md). The config
describes who exists and who may talk; it can never flip the mechanism that secures it.

Validation here is *structural* (shape, types, referential integrity within the
schema). Validation that needs the lowered form — ifname lengths, unwired flow
shapes, link anchors — happens in Layer 2.

## Layer 2 — Plan (build_model, the lowering)

**Where:** `cmd/turnip/model.go`. **Purity:** pure — no IO, no fds, no root.

`buildModel(cfg, owner, stateDir) → *Plan` resolves the config into the
fully-concrete dataplane description. Where the config is symbolic-and-nested, the
`Plan` is **concrete-and-grouped-by-consumer**:

- **Names → addresses.** Flow endpoints (`from`/`to` container names) resolve to
  their `/32`s; `/etc/hosts` peers resolve likewise.
- **Synthesis.** Router-side veth names (`vethR-<c>`), the host-edge `/31` peer
  (`Link.Next()`), the sysctl set, the nft inputs — none exist in the config.
- **Defaulting with global context.** `Default` route ownership folds in the
  container's interface count *across all networks and links* (sole interface ⇒
  default), which a single `config.Network` can't see.
- **Re-grouping.** Ingress, stored per-attachment in the config, flattens into
  NAT-oriented `DNAT` records; egress/ingress allows gather into an `Edge`.

The `Plan` types (`Plan`, `NetworkPlan`, `EndpointPlan`, `UplinkPlan`,
`ContainerPlan`) are an **aggregate of `dataplane` structs** — `dp.Gateway`,
`dp.Endpoint`, `dp.Uplink`, `dp.Flow`, `dp.DNAT`, `dp.LinkSpec` — plus the netns keys
and paths the shell needs. They live in `cmd` (not `dataplane`) so the dataplane
library stays config-agnostic.

**Everything fallible happens here.** `routerIf` rejects an over-IFNAMSIZ name,
`buildFlows` rejects icmp/`port="any"`, `ValidateLinkAnchors` checks every anchor —
all *before* `up` bootstraps a single netns. A bad config fails with nothing mutated.

This purity is the testability win: the whole resolution is assertable on a `Plan`
literal with no VM and no root (Layer-1 unit tests). The `Plan` is also the shared
fixture type — apply consumes exactly what the lowering tests construct, and you can
feed apply a hand-authored `Plan` that no config could express.

> The `Plan` carries the **resolved argument list for the effectful dataplane calls** —
> uniformly. For sysctls/nft that means the *built artifacts* (the sysctl map, the
> `nftlib.Ruleset`), because the effectful primitives apply calls (`WriteSysctls`,
> `nftlib.Load`) take artifacts; the pure builders (`dp.RouterSysctls`, `dp.BuildNFT`)
> therefore run in lowering. This is the invariant that keeps apply a pure walk —
> **apply calls only effectful primitives; the pure builders and the `router:`/
> `container:` key scheme belong to lowering.** The flow-resolution *reasoning* stays
> visible and tested in the lowering helpers (`buildFlows`, `buildEgressAllows`,
> `buildIngress`), so an artifact-shaped `Plan` doesn't hide it — it relocates the
> render step to where the rest of the resolution lives.

## Layer 3 — Apply (the imperative driver)

**Where:** `cmd/turnip/apply.go`. **Purity:** effectful (the do side).

`applyPlan(set, plan)` walks the `Plan` and pushes it onto the live netns `Set`:
loopback up in every netns, then per-network (`applyNetwork`), then per-container
(`applyContainer`). This is the only place that threads fds, runs `set.Enter` setns
episodes, calls the effectful `dataplane` primitives, and emits the progress output.

Because every decision was made in Layer 2, apply is **total over a valid Plan** — its
only errors are real runtime/IO faults (a netns missing from the bootstrap set, a
netlink failure), never config problems. Ordering *is* the policy apply owns, and it's
load-bearing: the uplink veth is wired before the router sysctls/nft so its `rp_filter`
dir and egress allows exist when they're referenced; sysctls and nft come last.

## The shell — up / down

**Where:** `cmd/turnip/up.go`, `cmd/turnip/down.go`. Dispatched from `main.go`.

`up` is two passes over the seam:

```
up = loadConfig → resolveRuntime → buildModel → clearHostEdge → Bootstrap → applyPlan
       (Layer 1)    (env/IO)        (Layer 2)    (clean slate)   (netns)    (Layer 3)
```

`up = down + build` (clean slate): `clearHostEdge` scrubs prior init-netns host-edge
state, and `Bootstrap` mints the netns fresh. `down` is the teardown half —
`clearHostEdge` + `netns.Teardown` (removing a pinned netns reaps everything inside
it: links, routes, sysctls, the nft table). `clearHostEdge` is shared by both and so
lives in the shell.

`resolveRuntime` is the rootful resolution: turnip runs as root and drops to the
rootless-podman owner (`runtime.user`, else `$SUDO_USER`); state dirs follow the
*target* uid (`/run/user/<uid>/turnip`), not root's `$XDG_RUNTIME_DIR`.

---

## internal/dataplane — what it does / doesn't do

**Does:** build the routed L3 fabric inside a netns *given its fd*. Each function is
a capability the shell sequences:

| primitive | effect |
|---|---|
| `SetLoUp(fd)` | bring `lo` up |
| `CreateGateway(fd, Gateway)` | the dummy gateway + its `/32` |
| `Connect(routerFd, contFd, gw, Endpoint)` | a routed veth pair across two netns |
| `HostEdgeConnect(routerFd, Uplink)` | the `/31` veth across init↔router |
| `ConfigureHostNAT(net, Uplink, ips, dnats)` | host masquerade + container routes + ingress DNAT |
| `WriteSysctls(map)` | write a resolved sysctl map into the netns (`/proc/sys`) |
| `BuildNFT(flows, edge)` / *(caller `nftlib.Load`)* | build / load the `inet turnip` ruleset |
| `LinkConnect(contFd, LinkSpec)` | a container link (veth/macvlan/ipvlan/phys) |
| `ValidateLinkAnchors([]LinkSpec)` | fail-fast anchor checks (pure) |
| `TeardownHostEdge(net, hostIf)` | remove the init-netns uplink veth + nat zones |

**Doesn't:** know about `config` (it speaks `Flow`/`Endpoint`/`Gateway`, never
`Network`/`Attachment`); know about the `netns.Set`, the netns naming scheme
(`router:` / `container:`), bootstrapping, or pinning; decide *order*. It takes a raw
`fd int` and concrete values and acts. The package is **almost entirely effectful** —
its job is kernel ops by fd. The one purely-derived artifact that was a *plain*
transform of names, the router sysctl set, moved *out* to lowering (`routerSysctls` in
`model.go`); `WriteSysctls` (the kernel write) stays. The pure builders that remain
(`BuildNFT`, `ValidateLinkAnchors`) consume dataplane's own policy types, so they sit
here for now. That fd-level seam is exactly why apply, not dataplane, owns the `Plan`:
apply's one real dependency is the `Set`, the thing dataplane deliberately doesn't know.

## internal/netns — netns + fd management

**Where:** `internal/netns/netns.go`. Owns the live runtime state: the open netns fds
and their lifecycle. Like dataplane, decoupled from config and policy — it knows only
"named netns: here are their fds, enter them, close them." The caller passes a
`[]Spec` (name + bind-mount path); `netns` does the rest.

**The re-exec / fd boundary.** Everything must happen inside podman's user+mount
namespaces (mount ns so the persistent bind-mounts are visible; user ns so we hold
`CAP_*` over the namespaces podman owns). Go's multithreaded runtime can't
`setns(CLONE_NEWUSER)` in-process, so `netns` uses **`podman unshare` as an exec
boundary**: it drops a fresh copy of the binary inside podman's userns as the
*provisioner* child (`main.go` dispatches the `ProvisionArg`/`TeardownArg` sentinels —
these are not user commands). The provisioner creates each netns, bind-mounts it for
persistence *while inside it*, and ships its fd back to the rootful parent over
`SCM_RIGHTS`. The parent collects them into a `Set` and drives the dataplane against
those fds. (Both load-bearing constraints — the provisioner can't `setns` back to the
host netns, and `unshare` chains forward minting a fresh netns — are validated in
[`spike/go-netns-bootstrap`](../spike/go-netns-bootstrap).)

**The `Set`** is the parent's handle to the live netns:

- `set.FD(name) → (fd, ok)` — the fd for a named netns, for netlink-over-fd
  (`CreateGateway`, `Connect`, …).
- `set.Enter(name, fn)` — run `fn` *inside* a netns. Needed for the bits with no
  netlink verb and no fd parameter: `/proc/sys` (sysctls) and forked `nft`, both of
  which act on the **process** netns. Apply wraps the sysctl write and `nftlib.Load`
  in `set.Enter` episodes.
- `set.Close()` — drop the parent's fd handles. The netns themselves **persist** via
  bind-mount, so this is just handle cleanup.

**Lifecycle.** `Bootstrap(owner, specs)` → live `Set`. `Teardown(owner, paths)` removes
the pinned netns (the whole teardown for the routed fabric — host edge aside). The
state lives under `runtime.state_dir`: `routers/<net>`, `containers/<name>/netns`.

---

## Why the seam pays off

- **The boundary is a type.** Takes `*netns.Set` ⇒ apply; returns/consumes a `*Plan` ⇒
  lowering. No interleaving, no trust-the-comment.
- **Fail-fast is structural.** Resolution errors surface from `buildModel` before
  `Bootstrap`, so a bad config never half-builds a fabric.
- **Testable without root.** The entire config→dataplane resolution is a pure function
  over a `Plan` literal.
- **A dry-run falls out.** Once the `Plan` is data, a `turnip plan` that prints what
  `up` would do — without touching a namespace — is a walk over the same structure.
  (Not yet built; noted as the natural next step.)

## Map

| concern | package / file | pure? |
|---|---|---|
| model + structural validation | `internal/config` | ✓ |
| lowering (`buildModel`) + `Plan` types | `cmd/turnip/model.go` | ✓ |
| apply driver | `cmd/turnip/apply.go` | ✗ |
| shell: up / down / runtime resolution | `cmd/turnip/{up,down}.go`, `main.go` | ✗ |
| dataplane capabilities (fd-level) | `internal/dataplane` | mixed* |
| netns + fd lifecycle, re-exec boundary | `internal/netns` | ✗ |

\* `dataplane` is mostly effectful fd primitives; the two pure builders left
(`BuildNFT`, `ValidateLinkAnchors`) consume its own policy types. The plain-typed
sysctl builder moved to lowering (`routerSysctls` in `model.go`).
