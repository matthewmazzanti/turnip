# turnip — TODO / deferred work

A living list of deliberately-deferred work, with enough context to resume without
re-deriving it.

## Capability-based privilege (no-root / systemd-service path) — DEFERRED

The probe and M4 currently run at **sudo level** (as root throughout; the podman
child does a plain `setresuid` drop to the rootless user, the host-edge parent stays
root). Running instead as an **unprivileged user holding ambient `CAP_NET_ADMIN`**
(the systemd `User=` + `AmbientCapabilities` deployment, no root at all) is fully
designed and was validated in the VM — but it's a yak-shave we don't need to prove
the host↔podman bridge, so it's pulled out for now.

What's decided (so we don't re-litigate it when we pick it back up):

- **One code path, two sources:** `sudo` (root) **or** user + ambient `CAP_NET_ADMIN`.
  Gate on `requires_root` (any uplink/links).
- **In-process drop, NOT a setpriv re-exec.** Re-exec is fragile under nix wrappers /
  uv / a package `main` with relative imports (re-exec as a bare script breaks
  `from . import`). The dance: `PR_SET_KEEPCAPS` → `setresgid`/`initgroups`/
  `setresuid` → `capset` P=E=I={NET_ADMIN} → raise ambient. One conditional (the
  identity drop is root-only). Idempotent for the already-user path. Was green on
  BOTH sources in the VM before removal.
- **The netns child drops all caps**, then re-gains them inside podman's userns via
  owner-match. It must keep a **broad bounding set** — podman/nft exec as
  uid-0-in-userns and get `P' = bounding`; clamp it and they're starved.
- **Bounding-set clamp to {NET_ADMIN}** is therefore **parent-only, post-fork** (needs
  `CAP_SETPCAP`; a pre-fork clamp poisons the child). Low value — the parent only
  execs non-setuid `nft`, so a broad bounding set is never activated. Optional.
- **No distro bindings.** `capng` isn't on PyPI (SWIG + compiled `.so`); `nftables.py`
  is GPLv2. Hand-roll instead: caps via direct **syscalls** (kernel ABI — license-
  clean, dep-free, identical under uv and the nixpkgs VM env), nft via arm's-length
  **subprocess** (separate GPL program = clean boundary). Constants are frozen UAPI
  (`CAP_NET_ADMIN=12`; v3 capset = two 32-cap blocks; ambient/bounding are `prctl`,
  not `capset`) — hardcode + cite `<linux/capability.h>`/`<linux/prctl.h>`.
- **Trust via verification, not provenance.** After each drop, **read caps back from
  `/proc/self/status`** (the kernel's independent view) and assert the exact intended
  set, aborting on mismatch. VM tests: differential vs `getpcaps`/`capsh`, plus a
  negative test (the dropped child genuinely cannot do an init-netns op).

Resume by adding a `privilege` module (`have_net_admin` / `become` / `drop_all`) with
the read-back assertion baked in, gated by `requires_root`. The last validated probe
version (in-process drop, both sources green) is in git history around commit
`f6e787e`.

## Other known deferrals (cross-ref docs/IMPLEMENTATION-PLAN.md)

- Running-container teardown guard: `up` must refuse when a live container holds a
  target netns (teardown would orphan it).
- Veth name truncation/hash scheme for multi-network (IFNAMSIZ = 15).
- `port="any"` / icmp-in-`flows` (needs a second nft map shape).
