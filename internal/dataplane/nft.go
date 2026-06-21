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

// Edge is the uplink policy in a router's forward chain (only with an uplink): which
// containers may INITIATE out the uplink (egress) and which published ports may be reached
// IN through it (ingress, post-DNAT). UplinkIf is the router-side uplink veth.
type Edge struct {
	UplinkIf string
	Egress   []EgressAllow
	Ingress  []IngressAllow
}

// EgressAllow lets a container (by IP) initiate out the uplink. All = any proto/port; else
// the scoped rules.
type EgressAllow struct {
	IP    netip.Addr
	All   bool
	Rules []EgressScope
}

// EgressScope is one scoped allowance: Protos fan out to a rule each; Port 0 means no port
// (icmp, or an "any" port).
type EgressScope struct {
	Protos []string
	Port   int
}

// IngressAllow lets traffic in the uplink reach a published container port. Port is the
// CONTAINER port (the host's DNAT has already rewritten the dport by the time the packet
// reaches the router's forward chain).
type IngressAllow struct {
	IP    netip.Addr
	Proto string
	Port  int
}

// BuildNFT renders the `inet turnip` ruleset for one router netns: the forward flow matrix,
// the uplink egress allows (when edge != nil), and the router's own-address lockdown. Apply
// it with nftlib.Load from inside the router netns (a netns.Set.Enter episode). Ports build_nft.
//
//   - forward (policy drop): accept ct established/related (the conntrack return path, so
//     flows are one-way in the map); drop ct invalid; for ct new, vmap the (saddr, daddr,
//     l4proto, dport) key into allowed_flows; then the uplink egress allows; else policy drop.
//   - input (policy drop): the router's OWN address (gateway, uplink end) is default-deny.
//     Accept loopback, the conntrack return, and icmp (the gateway ping); tcp/udp fall to the
//     drop, so no router-local service is reachable without a deliberate allow.
func BuildNFT(flows []Flow, edge *Edge) nftlib.Ruleset {
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
	)
	if edge != nil {
		cmds = append(cmds, egressRules(t, edge)...)
		cmds = append(cmds, ingressRules(t, edge)...)
	}
	cmds = append(cmds,
		t.Rule("input", nftlib.Match(nftlib.Meta("iifname"), "lo"), nftlib.Accept()),
		t.Rule("input", nftlib.CtState("established", "related"), nftlib.Accept()),
		t.Rule("input", nftlib.Match(nftlib.Meta("l4proto"), "icmp"), nftlib.Accept()),
	)
	return nftlib.Rules(cmds...)
}

// egressRules are the forward-chain uplink egress allows: a container may INITIATE out the
// uplink (oifname = uplink, saddr = container). The return path rides ct, so these are
// one-directional. `All` = any; else scoped to (proto, port).
func egressRules(t nftlib.Table, edge *Edge) []nftlib.Node {
	var rules []nftlib.Node
	for _, e := range edge.Egress {
		base := []nftlib.Node{
			nftlib.CtState("new"),
			nftlib.Match(nftlib.Meta("oifname"), edge.UplinkIf),
			nftlib.Match(nftlib.Payload("ip", "saddr"), e.IP.String()),
		}
		if e.All {
			rules = append(rules, t.Rule("forward", append(base, nftlib.Accept())...))
			continue
		}
		for _, sc := range e.Rules {
			for _, proto := range sc.Protos {
				exprs := append(append([]nftlib.Node{}, base...), nftlib.Match(nftlib.Meta("l4proto"), proto))
				if proto != "icmp" && sc.Port != 0 {
					exprs = append(exprs, nftlib.Match(nftlib.Payload("th", "dport"), sc.Port))
				}
				rules = append(rules, t.Rule("forward", append(exprs, nftlib.Accept())...))
			}
		}
	}
	return rules
}

// ingressRules are the forward-chain uplink ingress allows: post-DNAT traffic IN the uplink
// to a published container port (iifname = uplink, daddr = container, dport = the CONTAINER
// port). Keyed on dest, since the client source is a wildcard after the host's DNAT.
func ingressRules(t nftlib.Table, edge *Edge) []nftlib.Node {
	var rules []nftlib.Node
	for _, in := range edge.Ingress {
		rules = append(rules, t.Rule("forward",
			nftlib.CtState("new"),
			nftlib.Match(nftlib.Meta("iifname"), edge.UplinkIf),
			nftlib.Match(nftlib.Payload("ip", "daddr"), in.IP.String()),
			nftlib.Match(nftlib.Meta("l4proto"), in.Proto),
			nftlib.Match(nftlib.Payload("th", "dport"), in.Port),
			nftlib.Accept(),
		))
	}
	return rules
}
