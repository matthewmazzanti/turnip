# turnip — TODO

Open work for the Go rewrite (rootful only; the capability / no-root path is out of scope).

- [x] container **links** — `ValidateLinkAnchors` + `LinkConnect` (veth→bridge / veth→host), the L2 trust escape — feature-complete (`internal/dataplane/links.go`); VM-validated. (macvlan/ipvlan/phys were trimmed from the target — see `docs/CONFIG-SKETCH.md`.)
- [ ] running-container teardown guard — refuse when a live container holds a target netns (would orphan it)
- [x] `buildPlan` — landed as the pure `cmd/turnip/plan.go` lowering: config → a fully-resolved `Plan` (not a stateful object graph; see `docs/ARCHITECTURE.md`). `applyPlan` (`apply.go`) drives it
- [ ] `port="any"` / icmp in `flows` — needs a second nft map shape
- [ ] veth-name truncation/hash for multi-network (IFNAMSIZ=15; `routerIf` currently just rejects over-long)
- [ ] `inNetns` thread mechanics — "Go retires the locked thread" only holds when the goroutine exits; run setns episodes on dedicated short-lived goroutines (see the TODO in `internal/netns`)
- [ ] clean up nftlib rendering — decide whether to lean into Go struct marshaling (json tags / `MarshalJSON`) instead of the current `render()` → `map[string]any` builders
- [x] end-to-end test: `podman run --network ns:` two containers; confirm an allowed flow connects and a denied one drops — landed as `TestPodmanRun` (POD-1/POD-2) over the L1 fixture, run from the nix-built `probeImage` (flake `-image`); see `docs/TEST-PLAN.md` §6 E2E-2/E2E-3. (egress/ingress operator variants E2E-4/E2E-5 still open)
- [ ] **reevaluate apply-phase ordering** — `applyNetwork` now front-loads `nft → sysctls` before its gateway/veths (so interfaces are born into a hardened, policy-loaded netns). Consider lifting that to a GLOBAL staging across all networks/containers: load all router `nft`+`sysctls` first, then create every interface (gateway/veths/links). Cleaner for multi-network containers and makes every netns policy-ready before any interface (or cross-netns link) exists. (`cmd/turnip/apply.go:applyPlan`)
- [x] **example executable VM** — a runnable NixOS VM that wires up `quadlet-nix` + turnip for a real network deployment, as a worked end-to-end example. Landed as `nix/demo/homelab.nix` (single-file: the README hass homelab — zwave/hass/proxy + a host-LAN link — as turnip + quadlet-nix containers) built on the layered `nix/lib/turnip.nix` helpers; `nix run .#demo` boots it, `turnip-demo` is the guided tour. Containers run rootless-under-system-systemd via quadlet-nix `rootlessConfig.uid` (the "unsupported" mode; env hand-wired in the demo)
- [x] **clean up flake** — consolidated the VMs under `nix/vm/` (one entry point, per-role base shared by test+interactive over the mirrored `192.168.1.x` LAN), unified the probe image (`vms.probeImage`, baked at `/etc`), embedded the fixtures (dropped `-fixtures`). See `nix/vm/default.nix`.
