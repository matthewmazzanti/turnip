package dataplane

import (
	"net/netip"

	"git.lan/mmazzanti/turnip/internal/nftlib"
)

// nftTable is the per-router-netns table (one per netns; constant name).
const nftTable = "turnip"

// flowKeyType is the allowed_flows map's compound key: who (saddr), to-whom (daddr), on
// which (proto, port).
var flowKeyType = []string{"ipv4_addr", "ipv4_addr", "inet_proto", "inet_service"}

// Flow is one directional allow: FromIP may initiate to ToIP on (Proto, Port), and only
// that -- the return path rides conntrack. The caller resolves container names to IPs and
// drops icmp / port="any" (not wired), so Proto is always tcp/udp here.
type Flow struct {
	FromIP netip.Addr
	ToIP   netip.Addr
	Proto  string // "tcp" | "udp"
	Port   uint16
}

// BuildNFT renders the `inet turnip` ruleset for one router netns: the forward flow matrix
// and the router's own-address lockdown. Apply it with nftlib.Load from inside the router
// netns (a netns.Set.Enter episode). Ports main.py build_nft, minus the deferred uplink edge.
//
//   - forward (policy drop): accept ct established/related (the conntrack return path, so
//     flows are one-way in the map); drop ct invalid; for ct new, vmap the (saddr, daddr,
//     l4proto, dport) key into allowed_flows; else policy drop.
//   - input (policy drop): the router's OWN address (gateway, future uplink end) is
//     default-deny. Accept loopback, the conntrack return, and icmp (the gateway ping); tcp/
//     udp fall to the drop, so no router-local service is reachable without a deliberate allow.
func BuildNFT(flows []Flow) nftlib.Ruleset {
	t := nftlib.Table{Family: "inet", Name: nftTable}

	// allowed_flows: one element per flow, DIRECTIONAL (the return path rides ct).
	elems := make([][2]nftlib.Node, 0, len(flows))
	for _, f := range flows {
		key := nftlib.Concat(f.FromIP.String(), f.ToIP.String(), f.Proto, int(f.Port))
		elems = append(elems, [2]nftlib.Node{key, nftlib.Accept()})
	}

	// the per-packet key the vmap looks up: ip saddr . ip daddr . meta l4proto . th dport.
	flowKey := nftlib.Concat(
		nftlib.Payload("ip", "saddr"), nftlib.Payload("ip", "daddr"),
		nftlib.Meta("l4proto"), nftlib.Payload("th", "dport"),
	)

	cmds := append(t.Reload(),
		t.Chain("forward", "filter", "forward", 0, "drop"),
		t.Chain("input", "filter", "input", 0, "drop"),
		t.Map("allowed_flows", flowKeyType, elems),

		t.Rule("forward", nftlib.CtState("established", "related"), nftlib.Accept()),
		t.Rule("forward", nftlib.CtState("invalid"), nftlib.Drop()),
		t.Rule("forward", nftlib.CtState("new"), nftlib.Vmap(flowKey, "allowed_flows")),

		t.Rule("input", nftlib.Match(nftlib.Meta("iifname"), "lo"), nftlib.Accept()),
		t.Rule("input", nftlib.CtState("established", "related"), nftlib.Accept()),
		t.Rule("input", nftlib.Match(nftlib.Meta("l4proto"), "icmp"), nftlib.Accept()),
	)
	return nftlib.Rules(cmds...)
}
