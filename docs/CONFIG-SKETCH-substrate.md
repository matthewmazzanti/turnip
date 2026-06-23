# Appendix — the bare-substrate experiment (explored, NOT the next target)

> **Status: a captured thought experiment, not a roadmap.** This is the
> opposite-extreme of the [main spec](CONFIG-SKETCH.md): what the config becomes with
> *zero* privileged constructs. It is recorded because the exercise was clarifying —
> it explains *why* the higher-level sugar exists — **not** because turnip should
> adopt it. The next target remains the main spec's flow-only model. Read this for the
> reasoning it produced, then put it down.

## The premise — delete every privileged construct

No `router`, `network`, `container`, `uplink`, `flows`/`egress`/`ingress`. Only the
substrate Linux gives us:

- a **netns** = a named namespace with sysctls, local devices, routes, and an nft
  ruleset (plus a `handoff` intent, e.g. podman, and `external: true` for the
  pre-existing host ns);
- a **link** = a veth (or anchored macvlan/ipvlan/phys) wiring two netns;
- a **router** is *not a thing* — just a netns with `ip_forward=1` + routes; a
  **container** is *not a thing* — just a netns whose path you hand to podman.

The whole vocabulary is five kinds (netns, link, device, address, route) — which is
the **dataplane verb set turned into nouns**. Addresses/routes/sysctls/nft are
netns-local; the veth is the only inherently two-netns object, so it's the one
top-level relation.

## What it looks like (a fragment)

v1's `proxy` (ingress 8443→443) on the `lan` router, fully desugared:

```json

{
  "netns": {
    "host": {
      "external": true,
      "sysctls": {
        "net.ipv4.ip_forward": 1
      },
      "routes": [
        { "to": "10.0.0.0/24", "via": "169.254.1.1" }
      ],
      "nft": [
        "ip saddr 10.0.0.0/24 oif eth0 masquerade",
        "iif eth0 tcp dport 8443 dnat to 10.0.0.13:443"
      ]
    },
    "r-lan": {
      "sysctls": {
        "net.ipv4.ip_forward": 1,
        "net.ipv6.conf.all.disable_ipv6": 1
      },
      "devices": [
        {
          "kind": "dummy",
          "name": "fabric0",
          "addrs": [
            "10.0.0.1/32"
          ]
        }
      ],
      "routes": [
        {
          "to": "10.0.0.13/32",
          "dev": "vethR-proxy"
        },
        {
          "to": "default",
          "via": "169.254.1.0"
        }
      ],
      "nft": [
        "ct state established,related accept",
        "iif vethR-up ip daddr 10.0.0.13 tcp dport 443 ct state new accept",
        "drop"
      ]
    },
    "proxy": {
      "handoff": "podman",
      "routes": [
        {
          "to": "10.0.0.1/32",
          "dev": "eth0"
        },
        {
          "to": "default",
          "via": "10.0.0.1"
        }
      ]
    }
  },
  "links": [
    {
      "kind": "veth",
      "a": {
        "netns": "r-lan",
        "name": "vethR-proxy",
        "sysctls": {
          "rp_filter": 1
        }
      },
      "b": {
        "netns": "proxy",
        "name": "eth0",
        "addrs": [
          "10.0.0.13/32"
        ]
      }
    },
    {
      "kind": "veth",
      "a": {
        "netns": "host",
        "name": "veth-lan-host",
        "addrs": [
          "169.254.1.0/31"
        ]
      },
      "b": {
        "netns": "r-lan",
        "name": "vethR-up",
        "addrs": [
          "169.254.1.1/31"
        ],
        "sysctls": {
          "rp_filter": 1
        }
      }
    }
  ]
}
```

What the sugar was doing is now hand-written: the `vethR-*` names, the route-only
router end, the dummy gateway, per-veth `rp_filter=1`, `ip_forward` in both router
*and* host, the subnet→router route, the /31 uplink, the masquerade, the DNAT, and
the whole `established → allows → drop` chain. ~3 lines/container in v1 → ~15 here.

## What it buys

Every "deferred" feature **vanishes as a feature** — it's just more rows:
router↔router is a veth between two router netns; container↔container a veth between
two podman netns; a bridge network a `kind: bridge` device with `master` ports;
host-local / `egress_via` / multi-homing are routes + nft. Expressiveness is total —
anything netlink + nft + sysctl can do.

## What it costs (why this is not the target)

1. **The security invariants stop being structural.** In v1, rp_filter-strict,
   ipv6-off, /32 anti-spoof, masquerade-at-the-boundary, default-deny are guaranteed
   *by construction* — no config can express their violation. Here they're ordinary
   rows you can omit, mistype, or contradict (forget one `rp_filter:1` → silent spoof
   hole; `ip_forward:1` in a container → bridges two networks the model would never
   allow). **The `router` construct was a bundle of enforced invariants wearing a
   name.**
2. **Default-deny inverts** to "allow whatever you wrote" — fail-closed is a property
   of the *compiler*, gone when you hand-write nft.
3. **Referential integrity collapses to string joins** — every `dev`/`via`/`netns`
   is a name that must resolve; v1's keyed maps made whole error classes
   unrepresentable.
4. **Privilege goes implicit** (rootful iff something touches the `external` host ns)
   and **the audit view is gone** (policy scattered across `netns` + `links`).

## The one wound worth fixing anyway — references

The substrate's genuinely bad part isn't verbosity, it's **IP duplication**:
`10.0.0.13` appears in proxy's link addr, the host DNAT, the masquerade subnet, the
router rule, *and* the route — change one, hunt the rest. The minimal fix is a
**reference**: define an address once (on the link end that owns it), name everything
else by symbol, resolve from the links.

```json
"b": { "netns": "proxy", "name": "eth0", "addr": "10.0.0.13/32" },   // the ONE definition
"nft": [ "iif vethR-up ip daddr $proxy tcp dport 443 ct state new accept" ]   // a reference
```

`$name` (string) or `{ "ref": "name" }` (structured) — flat namespace, uniqueness-
checked, qualified (`$hass.eth0`) when a netns is multi-homed. The structured form
targets nft's native **JSON representation** (where `nftlib` is already heading), with
the string DSL parsing into it — that's the "two interfaces" answer.

This unlocks **"just give me an IP"**: once nothing inlines an address, the
definition's literal can be optional too — `addr: { "from": "lan" }` draws from a
**pool** (just a subnet, deliberately *not* a network), the resolver allocates and
auto-labels by netns. The only hard part is **stable allocation across runs**
(persisted leases read before `buildPlan`, fed in as an input artifact so lowering
stays pure — like `resolveRuntime` reading the env). References restore *referential
integrity*, **not** invariant safety — that's still the higher layer's job.

## The takeaway that *does* feed the real design

This substrate is **almost exactly turnip's `Plan`** (see [ARCHITECTURE](ARCHITECTURE.md))
— the resolved, mechanism-faithful description `apply` walks. So the experiment
**validates the layering** rather than challenging it:

```
intent (flows) ──lower──▶ symbolic substrate (+$refs, pools) ──resolve/allocate──▶ concrete substrate ≈ Plan ──apply──▶ kernel
   terse, safe,                                                                      total, verbose,
   invariant-enforcing                                                               invariant-agnostic
```

Three things to keep from the exercise, none of which require adopting the raw model:

- **References are the lowest useful rung** and the shared waist — v1's `flows` can
  lower to `$hass` refs instead of resolving concrete IPs itself, so one allocator
  serves both generated and hand-authored substrate. A worthwhile *internal* step;
  not a user surface.
- **A `turnip plan --emit`** that prints this substrate for a given `turnip.json`
  falls out for free (dry-run / debug / teaching).
- **The two distinctions that survive total dissolution** are the irreducible ones
  worth modeling: **created vs `external`** (the true source of the rootful/rootless
  split — not the `uplink` keyword) and **`handoff` intent** (the only thing that
  separates a "container" from a "router"). Everything else turnip calls a noun is
  sugar over routes + sysctls + nft on a graph of netns — which is exactly why it
  earns its keep.
