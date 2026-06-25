# turnip — sysctl hardening verdicts

The kernel `net.*` knobs turnip sets (and the ones it deliberately doesn't), with the reasoning,
so this can be re-evaluated against a new kernel, a topology change, or new research. The
authoritative builder is `cmd/turnip/plan.go:routerSysctls`; the structural test is NET-5
(`TestL1Structure`) + `TestRouterSysctls`.

## Model recap (why these and not the usual host baseline)

turnip is a **rootless, routed-L3** container network. Per network: one **router netns**; each
container sits on its own **`/32` point-to-point veth** (router end `vethR-<name>`, container end
`eth0` with a `/32` + default route via the gateway dummy). No shared L2 bridge between containers
— pure L3 routing. nft is fail-closed (forward + input policy drop; directional flow vmap;
conntrack return). The container is **untrusted** and owns its own netns, so all enforcement lives
on the **router side of the veth**, which the container can't touch.

Two consequences drive the verdicts:
- The relevant threats are **source spoofing** (→ strict rp_filter + `/32` route), **out-of-state
  injection** (→ conntrack), and **router-integrity** (redirects / source-routing). Host-baseline
  items aimed at *listening services* or *shared L2* are mostly moot.
- **Pin, don't inherit.** A fresh IPv4 netns copies `conf/{all,default}` from init_net's **live**
  values (`devconf_inherit_init_net=0`), so a host override would otherwise leak in. We pin every
  value we depend on.

## How they're applied (mechanism)

- **Ordered list**, `ip_forward` **first** — writing `ip_forward` re-derives the per-interface
  RFC1812 router defaults (`send_redirects`/`accept_source_route` flip toward TRUE), so the pins
  must follow it.
- **`conf.default` is the template**, written **before** any interface is created — so the gateway
  dummy, every fabric veth, and the uplink veth are **born hardened** by inheritance. No per-veth
  pinning; a future veth-adding path can't forget it.
- **One pass**, ordered `nft → WriteSysctls → gateway → veths` (`apply.go:applyNetwork`). nft is
  name/IP-based (loads with no interfaces present) and its ct-state rules register the netns
  conntrack hooks that create `/proc/sys/net/netfilter`, so the conntrack knobs resolve in the same
  pass.

## SET — router netns

| sysctl | value | gap or pin | what / why |
|---|---|---|---|
| `net.ipv4.ip_forward` | 1 | required | we route. Written FIRST (re-derives router defaults). |
| `conf.all.rp_filter` | 0 | pin | per-interface value authoritative (effective = `max(all, if)`); also blocks a host `all=1` forcing strict everywhere. |
| `conf.default.rp_filter` | 1 | **anti-spoof (core)** | STRICT reverse-path, paired with the `/32` device route. The spoof defense (BAD-1/BAD-2). |
| `conf.default.proxy_arp` | 1 | required | router answers the gateway ARP on each fabric veth. Inert on the p2p uplink/dummy. |
| `conf.all.accept_source_route` | 0 | **real gap** | `ip_forward=1` → RFC1812 router mode defaults this **TRUE**; drop source-routed (SRR) packets. Acceptance ANDs all+iface, so `all=0` closes it everywhere. |
| `conf.default.accept_source_route` | 0 | pin | belt-and-suspenders template (the `all=0` already closes it). |
| `conf.all` / `conf.default.send_redirects` | 0 | pin | router-mode default TRUE; no ICMP redirects out. Inert on /32 p2p (in/out veths always differ) but pinned. Enabled if EITHER all OR iface, so both pinned. |
| `conf.all` / `conf.default.accept_redirects` | 0 | pin | don't let an inbound ICMP redirect rewrite the router's static `/32` routes. Already off under forwarding; pinned for determinism. |
| `conf.all` / `conf.default.secure_redirects` | 0 | pin | CIS companion to `accept_redirects`; moot once accept=0, pinned anyway. |
| `net.netfilter.nf_conntrack_tcp_loose` | 0 | **real gap** | default (1) PICKS UP a bare out-of-state ACK as `ct new` and forwards it on allowed ports — making the forward chain's `ct invalid → drop` nearly dead. 0 → out-of-state pkts are `ct invalid` → dropped (BAD-4). Safe because the routed model is strictly symmetric. |
| `net.netfilter.nf_conntrack_tcp_be_liberal` | 0 | pin | default 0; pinned so out-of-**window** TCP also stays `ct invalid`, reinforcing tcp_loose=0. |
| `conf.all` / `conf.default.disable_ipv6` | 1 | pin | no L2 path between containers; v6 off router-wide severs inter-container v6. |

## SKIP — with grounds

| sysctl | why skipped |
|---|---|
| `nf_conntrack_checksum` (=1) | already the default; verifying checksums can false-positive under NIC checksum-offload. Pinning the default buys little; revisit only if bad-checksum tracking is ever observed. |
| `nf_conntrack_tcp_ignore_invalid_rst` | niche; default fine. `be_liberal=0` already governs out-of-window. |
| `nf_conntrack_log_invalid`, `log_martians` | observability, not enforcement — we already get the signal as **counters** (`IPReversePathFilter` for spoof drops in BAD-1/2, conntrack `invalid` in BAD-4). Logging is just dmesg noise. |
| `tcp_syncookies` | the router netns has **no listeners** (input policy-drop); nothing to SYN-flood. A container's own concern, not turnip's. |
| `icmp_echo_ignore_broadcasts` | no broadcast domain on /32 p2p veths (default 1 anyway). |
| `icmp_ignore_bogus_error_responses`, `ip_forward_update_priority`, `bootp_relay` | irrelevant / noise-reduction only. |
| ARP knobs: `arp_ignore`, `arp_announce`, `drop_gratuitous_arp`, `arp_accept` | the routed model already neutralizes ARP poisoning **structurally** — routing (the `/32` device route, container-uneditable) decides delivery, not ARP; the link is 2-party. Proven empirically by the §5 ARP test (`TestBADArpPoison`): `arp_accept=0` (default) + routing authority. `drop_gratuitous_arp` is orthogonal to `proxy_arp` but redundant. `arp_ignore`/`arp_announce` would fight the `proxy_arp=1` we rely on. |
| GLOBAL conntrack: `nf_conntrack_max`, `nf_conntrack_expect_max`, `nf_conntrack_buckets` | **read-only outside init_net** (kernel commit 671c54ea, 2021) — a write from the router netns errors. If ever needed, tune in the host/init netns. |

## Other netns

- **Container netns** — turnip sets **nothing**, by design. The container is untrusted and can
  revert anything we set; enforcement lives at the router's `/32` boundary (the whole BAD-* thesis).
- **Host / init netns** — only `net.ipv4.ip_forward=1`, and only when a network has an uplink (so
  the host routes/forwards it). Broader host sysctls are the operator's domain; turnip does not
  clobber host-wide state.

## Re-evaluation triggers

- **New kernel** — defaults can shift with compile config; NET-5 reads the live `/proc` values, so
  a regression surfaces as a test failure. Confirm `accept_source_route`/`send_redirects` router
  defaults on the target kernel.
- **Topology change** — if any interface is ever created in a router netns by a path that doesn't
  go through the `conf.default`-before-creation ordering, it won't be born hardened. (See the
  global-apply-ordering TODO in `apply.go`.)
- **`conf.default` open question** — interfaces present at netns *birth* (`lo`) predate the template
  write and keep inherited values; fine today (`lo` doesn't forward), revisit if that changes.
- **New research** — the original sweep is grounded in kernel `ip-sysctl.rst` / `nf_conntrack-sysctl.rst`
  + CIS/RHEL/SUSE, adversarially verified. The `accept_source_route` and `tcp_loose` gaps are the
  load-bearing findings; the rest are determinism pins or grounded skips.
