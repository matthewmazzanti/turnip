# turnip — TODO

Open work for the Go rewrite (rootful only; the capability / no-root path is out of scope).

- [ ] `configureContainers` — `lo` up + generated `/etc/hosts` + container links (the L2 escapes)
- [ ] `configureHostEdge` — uplink veth + masquerade/DNAT (the rootful init-netns half), the uplink rp_filter sysctl, and the nft egress/ingress edge rules
- [ ] `down`: also tear down host-edge state in the init netns (currently only scrubs the netns)
- [ ] running-container teardown guard — refuse when a live container holds a target netns (would orphan it)
- [ ] `build_model` — the real config→Container/Network/Endpoint graph (`netnsSpecs` is just a seed)
- [ ] `port="any"` / icmp in `flows` — needs a second nft map shape
- [ ] veth-name truncation/hash for multi-network (IFNAMSIZ=15; `routerIf` currently just rejects over-long)
- [ ] `inNetns` thread mechanics — "Go retires the locked thread" only holds when the goroutine exits; run setns episodes on dedicated short-lived goroutines (see the TODO in `internal/netns`)
- [ ] clean up nftlib rendering — decide whether to lean into Go struct marshaling (json tags / `MarshalJSON`) instead of the current `render()` → `map[string]any` builders
- [ ] end-to-end test: `podman run --network ns:` two containers; confirm an allowed flow connects and a denied one drops
