# turnip — TODO

Open work for the Go rewrite (rootful only; the capability / no-root path is out of scope).

- [x] container **links** — `validate_link_anchors` + `link_connect` (veth→bridge / veth→host / macvlan / ipvlan / phys), the L2 trust escape — feature-complete (`internal/dataplane/links.go`); VM-validated veth→host, veth→bridge, AND phys (spare eth1 in testvm.nix). macvlan/ipvlan still un-exercised in the VM
- [ ] phys up/up idempotency — a borrowed NIC returns under its in-container name (kernel auto-return, no restore), so a second `up` can't find the original `dev`. Matches Python; decide whether to rename-back on `down`
- [ ] running-container teardown guard — refuse when a live container holds a target netns (would orphan it)
- [x] `buildPlan` — landed as the pure `cmd/turnip/plan.go` lowering: config → a fully-resolved `Plan` (not a stateful object graph; see `docs/ARCHITECTURE.md`). `applyPlan` (`apply.go`) drives it
- [ ] `port="any"` / icmp in `flows` — needs a second nft map shape
- [ ] veth-name truncation/hash for multi-network (IFNAMSIZ=15; `routerIf` currently just rejects over-long)
- [ ] `inNetns` thread mechanics — "Go retires the locked thread" only holds when the goroutine exits; run setns episodes on dedicated short-lived goroutines (see the TODO in `internal/netns`)
- [ ] clean up nftlib rendering — decide whether to lean into Go struct marshaling (json tags / `MarshalJSON`) instead of the current `render()` → `map[string]any` builders
- [ ] end-to-end test: `podman run --network ns:` two containers; confirm an allowed flow connects and a denied one drops
