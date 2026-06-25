# Integration Test Plan — the two-VM harness and the scenario matrix

How we prove turnip's dataplane does what the config says, end to end, on a real
kernel. The unit tests already cover the **decide** side (config → `Plan`, pure); this
plan covers the **do** side — netns, veth, nft, sysctl, conntrack — and the security
invariants that live in code, not config (rp_filter-strict, ipv6-off, fail-closed).

Every expected outcome below is annotated with the mechanism that enforces it, so a
reviewer can check the expectation against the source, not against intuition.

---

## 1. Harness (settled)

**One hermetic check, two VMs, always.** `nix flake check` *is* CI — there is no
separate CI path. The check boots two nodes on a shared test LAN:

```
checks.integration:
  nodes = { host; world; }          # world authorizes host's test key
  testScript = start_all(); host.succeed("turnip.test -world ssh://world -test.v")
```

- **`host`** — the system under test. Rootful. Runs turnip, the router netns, podman.
  The orchestrator runs *here*, so host operations are local (in-process or exec).
- **`world`** — a dumb peer reached only over SSH. Runs `nc`/listeners/`scapy`, observes
  what arrives. Holds no test logic.
- **orchestrator** = a compiled `go test -c` binary (`turnip.test`). `-test.list` / `-test.run`
  give us list-tests / run-by-name for free. Custom `-host`/`-world` flags select endpoints.

**Why dual-VM everywhere:** the hermetic check has no host laptop to borrow as the peer —
world can *only* be a second node there. Local dev mirrors it so the topology under test is
identical to the one CI gates on. `driverInteractive` keeps both VMs warm; re-run a single
test by name after an edit.

**Two execution styles for host-side assertions:**
- **in-process** — link turnip's own `netns`/`dataplane` packages, drive a `Set` by fd, assert
  on netlink/nft state directly. Fast, white-box, for structural checks (§2) and crafted packets.
- **black-box** — shell out to the real `turnip up`/`down` + `podman run --network ns:`. Slower,
  proves the operator path (§6).

**`needsWorld` tag:** tests that need the peer (§4 ingress/egress, §5 egress-spoof) skip when no
world is configured, so a single-VM run still executes the host-only majority (§2, §3, internal §5)
as a fast inner loop.

**Probing inside a netns** (no podman needed for most dataplane checks): exec a busybox/scapy
into the container netns by fd, or `ip netns exec`-equivalent against the pinned path
`/run/user/<uid>/turnip/containers/<c>/netns`. Reserve real `podman run` for §6. fd-exec is a
hidden `turnip.test __probe <netns> -- <cmd…>` subcommand that `LockOSThread → setns → exec`s —
turnip's own `Provision` pattern reused, not a parallel impl.

**Toolchain:** **scapy** for adversarial crafting (§5 — the frame *is* the test intent),
**socat**/**curl**/`nc` for benign clients and listeners (§3/§4 — normal traffic should read as
normal).

**Bootstrap order (where to stand it up first):**
1. **Local single VM** (the host dev VM, `just host`) — prove the one risky primitive: a rootful
   `__probe` entering the *rootless-podman-owned* netns. Host-only, no world, already warm.
2. **`checks.integration.driverInteractive`** — the two-node check's interactive driver; boots
   `host`+`world` once and keeps them warm. Iterate the suite, re-run a single test by name.
3. **`nix flake check`** — same nodes, cold + hermetic. The gate; free once (2) is green.

The check and the local loop are *one* nixosTest definition — `driverInteractive` is just its
interactive entrypoint, not a separate harness. The single host dev VM is only the step-1
scratchpad for the fd-exec primitive.

---

## 2. Simple network configurations — structure materializes

Black/white-box structural assertions after `turnip up`. No traffic yet — prove the plan
became kernel state. Host-only.

| ID | Scenario | Assert | Enforced by |
|----|----------|--------|-------------|
| NET-1 | single network, 2 containers | router netns pinned at `routers/<net>`; each container netns at `containers/<c>/netns` | `netns.Provision`, `up.go:netnsSpecs` |
| NET-2 | gateway present | dummy `gw0` up, addr = gateway `/32`, in router netns | `dataplane.CreateGateway` |
| NET-3 | routed veths | per-container veth pair; router side has `/32` device route to container IP; container has default route via gateway | `dataplane.Connect`, host32 route |
| NET-4 | nft loaded | `inet turnip` table present in router netns; `forward` + `input` chains both policy `drop` | `dataplane.BuildNFT` |
| NET-5 | router sysctls | `ip_forward=1`; `conf.all.rp_filter=0`; per-veth `rp_filter=1` + `proxy_arp=1`; `nf_conntrack_tcp_loose=0`; ipv6 disabled | `plan.go:routerSysctls` |
| NET-6 | two networks isolate | two router netns, two distinct nft tables; no route between them | one router/netns per network |
| NET-7 | network with uplink | `/31` uplink veth; router default route via uplink host end; host-edge `ip turnip_host_<net>` nat table with masquerade | `dataplane.HostEdgeConnect`, `BuildHostNFT` |
| NET-8 | network without uplink | no uplink veth, no host-edge nat table, no default route in router | uplink branch skipped |

---

## 3. Flow checks — internal (container → container, one network)

The core policy matrix. `forward` is policy **drop**; a `new` packet is accepted only if
`(ip saddr . ip daddr . l4proto . th dport)` hits the `allowed_flows` vmap. Flows are
**directional** — the return path rides conntrack, there is no reverse vmap entry. Host-only.

Reference config: net `lan`, containers `a` (10.0.0.11), `b` (10.0.0.12); one flow
`{type:internal, from:a, to:b, proto:tcp, port:8080}`.

| ID | Action | Expect | Enforced by |
|----|--------|--------|-------------|
| FLOW-1 | `a → b` tcp/8080 | **connect** | vmap hit → accept |
| FLOW-2 | reply `b → a` on that conn | **connect** (return path) | `ct established,related → accept` |
| FLOW-3 | `b → a` tcp/8080 (new, reverse) | **drop** | no reverse vmap entry; policy drop |
| FLOW-4 | `a → b` tcp/**9090** | **drop** | wrong dport, vmap miss |
| FLOW-5 | `a → b` **udp**/8080 | **drop** | wrong l4proto, vmap miss |
| FLOW-6 | `a → b` icmp (ping) | **drop** | no icmp flow; policy drop |
| FLOW-7 | unrelated `a → 10.0.0.99` | **drop** | no route / vmap miss → drop |
| FLOW-8 | container with zero flows reaches nothing | every probe **drops** | fail-closed default |

**Validation gaps (assert `turnip up` *rejects* the config — these are decide-side tests):**

| ID | Config | Expect | Enforced by |
|----|--------|--------|-------------|
| FLOW-G1 | internal flow `port:"any"` | `turnip up` errors | parser: "icmp / port=any in flows not wired yet" |
| FLOW-G2 | internal flow `proto:icmp` | `turnip up` errors | same parser guard |

> These mark known unimplemented surface. When wired, promote them to FLOW-* dataplane rows.

---

## 4. Ingress / egress checks — needs world

Egress leaves via the uplink veth with **unconditional masquerade** (there is no `nat:false`
mode). Ingress is host-netns DNAT + a post-DNAT `forward` allow. World is the internet stand-in.

**Egress** — net `lan` with uplink; container `a`; egress flow `{from:a, proto:tcp, port:443}`.

| ID | Action | Expect | Enforced by |
|----|--------|--------|-------------|
| EG-1 | `a → world:443` tcp | **connect** | egress rule: `ct new, oifname=uplink, saddr=a` |
| EG-2 | world observes source addr | **masqueraded** (uplink/host edge, never 10.0.0.11) | `BuildHostNFT` postrouting masquerade |
| EG-3 | `a → world:80` tcp | **drop** | scoped egress (443 only) |
| EG-4 | container w/o egress flow → world | **drop** | forward policy drop, no egress allow |
| EG-5 | egress flow `proto:any` (portless) | all ports/proto to world **connect** | wide egress branch |

> **Observer (EG-2):** `world` runs `socat TCP-LISTEN:443,fork SYSTEM:'printf %s "$SOCAT_PEERADDR"'`;
> the container reads back the source `world` saw and asserts it equals the host-edge IP and **not**
> `10.0.0.11`. One string compare covers connectivity (EG-1) *and* masquerade (EG-2). `conntrack -L`
> on the router is optional white-box corroboration.

**Ingress** — published `{type:ingress, to:a, proto:tcp, host_port:8443, port:443}`.

| ID | Action | Expect | Enforced by |
|----|--------|--------|-------------|
| IN-1 | world → `host:8443` | reaches `a:443` | prerouting DNAT + forward ingress allow |
| IN-2 | container `a` sees connection | peer = world (post-DNAT daddr=a) | `ingressRules` daddr/dport match |
| IN-3 | world → `host:9999` (unpublished) | **drop/refused** | no DNAT, no forward allow |
| IN-4 | `port` omitted defaults to `host_port` | DNAT targets that port | `IngressFlow.Port` 0→HostPort |

> **Return path (no ingress masquerade):** the host-edge masquerade matches `iifname=uplink-veth`
> (egress only), so ingress is *not* SNAT'd — `a` sees world's **real** source IP (assert this; the
> whole return path depends on it). The reply follows `a → router → (default via uplink) → host →
> world` on the host's connected LAN route, and conntrack reverses the DNAT so world sees `host:8443`.
> No explicit route needed while world is on a host-routable subnet (true on the test LAN); IN-1+IN-2
> passing *is* the confirmation.

**Router-local (input chain)** — host-only, no world needed:

| ID | Action | Expect | Enforced by |
|----|--------|--------|-------------|
| IN-5 | container ping gateway | **reply** | `input`: icmp accept |
| IN-6 | container → gateway tcp (any port) | **drop** | `input` policy drop; no router service exposed |

---

## 5. Packet crafting / bad-actor checks

Adversary = a container that crafts frames (scapy/nping inside its netns). Goal: confirm the
**security invariants hold** — most of these expect the *attack to fail*. The anti-spoof pin is
strict rp_filter + a `/32` device route per veth, enforced by the kernel before nft.

| ID | Attack | Expect | Enforced by |
|----|--------|--------|-------------|
| BAD-1 | `a` sends with spoofed saddr (unowned IP) | **drop** | rp_filter strict: reverse path ≠ this veth |
| BAD-2 | `a` spoofs `b`'s source IP (lateral) | **drop** | source resolves to b's `/32` veth, not a's iif |
| BAD-3 | `a` rewrites veth MAC, replays allowed flow | **no extra access** (still bound by rp_filter + vmap) | L3-routed: MAC is per-hop, not a trust boundary |
| BAD-4 | crafted out-of-state pkt (bogus ACK, no conn) | **drop** | `ct invalid → drop` |
| BAD-5 | egress with spoofed saddr | **drop** | rp_filter + egress `saddr=a` match (double) |
| BAD-6 | IPv6 between containers | **no connectivity** | ipv6 disabled router-wide |
| BAD-7 | denied container floods `new` to many dports | **all drop**, fail-closed | forward policy drop |
| BAD-8 | reach router-local service (input) via crafted dport | **drop** | input policy drop |

**Open documentation point (not a pass/fail):** BAD-3 — turnip does no MAC validation in nft.
The claim under test is that it *doesn't need to* (routed veths, MAC rewritten each hop). The
test exists to **prove the negative**: a rewritten MAC grants no bypass. If it ever does, that's
a finding, not an expected-fail.

---

## 6. Full bring-up + podman network — the operator path

Black-box. Real `turnip up`, real `podman run --network ns:`, real `turnip down`. This is the
todo.md end-to-end item. Mostly host-only; the ingress/egress variant needs world.

| ID | Scenario | Assert | Enforced by |
|----|----------|--------|-------------|
| E2E-1 | `turnip up` realistic multi-net config | exit 0; all netns/veths/nft/sysctls present (re-uses §2 asserts) | full apply path |
| E2E-2 | `podman run --network ns:<a>` + `<b>`, allowed flow | container `a` has IP 10.0.0.11; `a → b:8080` **connects** | bind-mounted netns join |
| E2E-3 | same, denied flow | `b → a:8080` **drops** | policy drop |
| E2E-4 | podman egress (uplink net) | container reaches `world`, masqueraded | §4 EG via real podman |
| E2E-5 | podman ingress | `world → host:8443` reaches the published container | §4 IN via real podman |
| E2E-6 | `turnip down` | every netns/veth/nft/host-edge table scrubbed; state dir empty | `__teardown` |
| E2E-7 | `turnip up` twice (idempotent) | second run stable, no dup veths/rules | provision unmount-then-pin |

> E2E-6 is also the **reset between scenarios** primitive — running it between cases both isolates
> them and continuously verifies teardown.

---

## Appendix A — gap registry (expected-fail / unimplemented)

| Gap | Current behavior | Test treatment |
|-----|------------------|----------------|
| internal flow `port:"any"` | parser rejects | FLOW-G1 asserts the error |
| internal flow `proto:icmp` | parser rejects | FLOW-G2 asserts the error |
| `nat:false` (no-masquerade) | knob removed; masquerade unconditional | EG-2 documents NAT-always |
| MAC anti-spoof in nft | none (L3 routed) | BAD-3 proves no bypass |
| IPv6 | disabled router-wide | BAD-6 asserts no connectivity |

## Appendix B — fixtures (test organization)

Tests are organized by **topology**, not one-per-check: ~4 distinct layouts, each brought up once,
a batch of asserts run against the warm fixture, then `turnip down` (which is also the isolation
reset and a free teardown test). ~4 bring-ups for the whole matrix.

| Fixture | Layout | Covers | World? |
|---|---|---|---|
| **L1** | one network, 2–3 containers, internal flows | §2 (partial), §3 all, §5 internal (BAD-1..4,6,7,8) | no |
| **L2** | two isolated networks | NET-6 | no |
| **L3** | network + uplink | §4 egress (EG-*), BAD-5 | yes |
| **L4** | network + uplink + ingress publish | §4 ingress (IN-*) | yes |

NET-1..5/7/8 fold into each fixture's post-bring-up structural batch. Start **fully black-box**
(parse `nft list` / `ip -j` / `/proc/sys`, fd-exec for traffic); promote a check to in-process
(link `dataplane`) only if it proves unobservable from outside — none identified yet.

## Appendix C — resolved decisions (was: open questions)

1. **Probe mechanism** — fd-exec (`__probe`) for §3/§5; real `podman run` only for §6. Risk to
   prove first: a rootful `__probe` entering the rootless-podman-owned netns (step 1 of bootstrap).
2. **Masquerade observer** — `socat` peer-addr echo on world (§4 EG-2 note); `conntrack -L` optional.
3. **Crafting toolchain** — stdlib `socket`+`struct` raw sends (spoofed saddr, bare ACK; no scapy
   dep) for the attacks; `socat`/python connect for the benign side.
4. **Organization** — by fixture (Appendix B), black-box first.
5. **Ingress return route** — free on the shared LAN; ingress isn't masqueraded (§4 IN note).
6. **Out-of-state drop needs `nf_conntrack_tcp_loose=0`** (BAD-4). The forward chain's `ct invalid
   → drop` is dead under the kernel default (loose=1 *picks up* a bare ACK as `ct new` and forwards
   it on allowed ports). `routerSysctls` sets loose=0 so out-of-state packets become `ct invalid`;
   safe because the routed model is strictly symmetric (no asymmetric path where conntrack misses
   the SYN). This forced apply to load nft *before* sysctls: the ct-state rules register the netns
   conntrack hooks that create `/proc/sys/net/netfilter` (where the loose knob lives).
