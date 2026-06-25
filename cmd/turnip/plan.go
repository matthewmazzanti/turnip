// plan.go -- buildPlan: the pure lowering from config to a fully-resolved dataplane Plan.
// No IO, no fds, no root. Everything that can fail to resolve (oversized ifnames, unsupported
// flows) is decided here, so applyPlan (apply.go) is total over a valid Plan. This is the seam
// the imperative shell drives: cfg -> Plan -> effects. Unit-testable without a VM (Layer 1).

package main

import (
	"fmt"
	"net/netip"
	"path/filepath"
	"strings"

	"git.lan/mmazzanti/turnip/internal/config"
	dp "git.lan/mmazzanti/turnip/internal/dataplane"
	"git.lan/mmazzanti/turnip/internal/netns"
	"git.lan/mmazzanti/turnip/internal/nftlib"
)

// Plan is the fully-resolved dataplane description lowered from the config: every netns to pin
// and the wiring to apply inside it. Pure data -- buildable and assertable without root, and
// the shared fixture type for both the lowering tests and the apply driver.
type Plan struct {
	Specs      []netns.Spec    // every netns to bootstrap (routers + containers), in order
	Networks   []NetworkPlan   // per network, sorted by name
	Containers []ContainerPlan // per container, sorted by name
	Owner      netns.Owner     // the rootless-podman owner (hosts-file chown)
}

// NetworkPlan is one network's resolved L3 wiring in its router netns: the gateway, the routed
// veths into attached containers, the optional host edge, and the fully-built router policy
// artifacts (the sysctl set + the nft ruleset). apply only pushes these -- the pure builders
// (routerSysctls here, dp.BuildNFT) ran in lowering.
type NetworkPlan struct {
	Name      string
	Router    string // the router netns key, "router:<name>"
	Gateway   dp.Gateway
	Endpoints []EndpointPlan    // routed veths, sorted by container
	Uplink    *UplinkPlan       // nil = no host edge
	Sysctls   []dp.Sysctl    // the router netns sysctls, in apply order (built from the veth/uplink ifnames)
	NFT       nftlib.Ruleset // the forward flow matrix + edge allows + input lockdown
}

// EndpointPlan pairs a routed veth's dataplane Endpoint with the container netns it connects.
// Netns is the resolved FD-lookup key; Container is the bare name for logs/errors.
type EndpointPlan struct {
	Container string // the attached container -- log label + error text
	Netns     string // the container netns key, "container:<name>" -- the FD lookup
	Endpoint  dp.Endpoint
}

// UplinkPlan is the host edge: the /31 veth across init<->router, the init-netns container
// routes, and the fully-built host policy artifacts (the init-netns sysctls + nat zone ruleset).
// Present iff the network has an uplink. apply only pushes these; the builders ran in lowering.
// (The router-side nft edge allows the uplink implies are folded into NetworkPlan.NFT.)
type UplinkPlan struct {
	Uplink       dp.Uplink
	ContainerIPs []netip.Addr      // container /32s to route via the uplink (init netns)
	DNATs        []dp.DNAT      // ingress host_port -> container:port (kept for reference/logs)
	HostSysctls  []dp.Sysctl    // init-netns sysctls (ip_forward)
	HostNFT      nftlib.Ruleset // the `ip turnip_host_<net>` nat zone (masquerade + DNAT)
}

// ContainerPlan is one container's local setup: the generated /etc/hosts (path + body) and its
// links (host netdev holes into the netns -- the L2 escape outside every router and nft policy).
type ContainerPlan struct {
	Name      string
	Netns     string // the container netns key, "container:<name>"
	HostsPath string // <state>/containers/<name>/hosts
	Hosts     string // the /etc/hosts body
	Links     []dp.LinkSpec
}

// buildPlan lowers the validated config into the dataplane Plan. Pure: no IO, no fds, no
// kernel. All resolution that can fail WITHOUT the host (ifname lengths, unwired flows, link
// conflicts) happens here, so a bad config fails before bootstrapping a single netns. The
// host-dependent checks (link anchors exist/are valid) are the caller's preflight phase.
func buildPlan(cfg *config.Turnip, owner netns.Owner, stateDir string) (*Plan, error) {
	plan := &Plan{
		Specs: netnsSpecs(cfg, stateDir),
		Owner: owner,
	}

	// links first: build every container's veth specs (their IFNAMSIZ resolution can fail).
	// The host-anchor checks happen later, in preflightAnchors (they read the live host).
	linkSpecs, err := buildLinkSpecs(cfg)
	if err != nil {
		return nil, err
	}

	counts := interfaceCounts(cfg)
	for _, netName := range sortedKeys(cfg.Networks) {
		np, err := lowerNetwork(netName, cfg.Networks[netName], counts)
		if err != nil {
			return nil, err
		}
		plan.Networks = append(plan.Networks, np)
	}

	for _, cname := range sortedKeys(cfg.Containers) {
		plan.Containers = append(plan.Containers, ContainerPlan{
			Name:      cname,
			Netns:     "container:" + cname,
			HostsPath: filepath.Join(stateDir, "containers", cname, "hosts"),
			Hosts:     hostsFile(cfg, cname),
			Links:     linkSpecs[cname],
		})
	}
	return plan, nil
}

// lowerNetwork resolves one network's config into its NetworkPlan. Pure: no IO, no fds -- which
// is exactly what makes the whole per-network resolution unit-testable.
func lowerNetwork(name string, net config.Network, counts map[string]int) (NetworkPlan, error) {
	np := NetworkPlan{
		Name:    name,
		Router:  "router:" + name,
		Gateway: dp.Gateway{IfName: net.GatewayIf, Addr: net.Gateway},
	}

	var routerIfs []string
	for _, cname := range sortedKeys(net.Attach) {
		att := net.Attach[cname]
		rif, err := routerIf(cname)
		if err != nil {
			return np, err
		}
		// effective default-route ownership: configured default, OR the container's sole
		// interface (config guarantees at most one configured default).
		np.Endpoints = append(np.Endpoints, EndpointPlan{
			Container: cname,
			Netns:     "container:" + cname,
			Endpoint: dp.Endpoint{
				RouterIf: rif,
				ContIf:   att.Interface,
				IP:       att.IP,
				Default:  ownsDefault(att.Default, counts[cname]),
			},
		})
		routerIfs = append(routerIfs, rif)
	}

	// the nft edge (egress/ingress allows) and the uplink's router-side ifname exist only with a
	// host edge; both feed the policy builders below.
	var edge *dp.Edge
	uplinkRouterIf := ""
	if net.Uplink != nil {
		uplink := dp.Uplink{
			HostIf:   net.Uplink.HostIf,
			RouterIf: net.Uplink.RouterIf,
			HostIP:   net.Uplink.Link,
			RouterIP: net.Uplink.Link.Next(),
		}
		var ips []netip.Addr
		for _, cname := range sortedKeys(net.Attach) {
			ips = append(ips, net.Attach[cname].IP)
		}
		dnats, ingressAllows := buildIngress(net)
		np.Uplink = &UplinkPlan{
			Uplink:       uplink,
			ContainerIPs: ips,
			DNATs:        dnats,
			HostSysctls:  []dp.Sysctl{dp.Sys("net.ipv4.ip_forward", "1")}, // the host routes/forwards this net
			HostNFT:      dp.BuildHostNFT(name, uplink, dnats),
		}
		edge = &dp.Edge{
			UplinkIf: uplink.RouterIf,
			Egress:   buildEgressAllows(net),
			Ingress:  ingressAllows,
		}
		uplinkRouterIf = uplink.RouterIf
	}

	// the router policy artifacts -- built here (pure) so apply only pushes them. sysctls key
	// off the per-veth conf.<if> dirs; nft is the forward flow matrix + edge allows + lockdown.
	flows, err := buildFlows(net)
	if err != nil {
		return np, err
	}
	np.Sysctls = routerSysctls(routerIfs, uplinkRouterIf)
	np.NFT = dp.BuildNFT(flows, edge)
	return np, nil
}

// --- lowering helpers (config -> dataplane structs) -------------------------

// routerIf is the router-side veth name for a container's attachment. It must fit IFNAMSIZ
// (15); reject an over-long name rather than letting the kernel truncate it into a silent
// collision. (Ports main.py router_if; the general (net,container) scheme is deferred.)
func routerIf(container string) (string, error) {
	name := "vethR-" + container
	if len(name) > 15 {
		return "", fmt.Errorf("router veth name %q exceeds IFNAMSIZ (15); shorten %q", name, container)
	}
	return name, nil
}

// linkHostIf is the init-side veth end name for a veth link. Like routerIf it must fit
// IFNAMSIZ (15) -- reject rather than let the kernel truncate into a silent collision.
func linkHostIf(container, linkName string) (string, error) {
	name := "vethL-" + container + "-" + linkName
	if len(name) > 15 {
		return "", fmt.Errorf("link host veth name %q exceeds IFNAMSIZ (15); shorten container %q / link %q",
			name, container, linkName)
	}
	return name, nil
}

// buildLinkSpecs derives every container's link specs from the config -- the model
// derivation for the L2 escape, grouped by container for the per-netns connect loop.
func buildLinkSpecs(cfg *config.Turnip) (map[string][]dp.LinkSpec, error) {
	out := map[string][]dp.LinkSpec{}
	for _, cname := range sortedKeys(cfg.Containers) {
		for _, link := range cfg.Containers[cname].Links {
			spec, err := buildLinkSpec(cname, link)
			if err != nil {
				return nil, err
			}
			out[cname] = append(out[cname], spec)
		}
	}
	return out, nil
}

// buildLinkSpec translates one config.Link union member into the flat dataplane.LinkSpec.
func buildLinkSpec(cname string, link config.Link) (dp.LinkSpec, error) {
	b := link.Base()
	spec := dp.LinkSpec{
		Container: cname,
		Name:      b.Name,
		Address:   b.Address,
		Gateway:   b.Gateway,
		Routes:    b.Routes,
		Mac:       b.Mac,
		Default:   b.Default,
	}
	if b.Mtu != nil {
		spec.MTU = *b.Mtu
	}
	switch l := link.(type) {
	case *config.VethLink:
		hostIf, err := linkHostIf(cname, b.Name)
		if err != nil {
			return spec, err
		}
		spec.HostIf = hostIf
		if l.Bridge != "" {
			spec.Kind, spec.Bridge = "veth-bridge", l.Bridge
		} else {
			spec.Kind = "veth-host"
		}
	default:
		return spec, fmt.Errorf("container %q: unknown link type %T", cname, link)
	}
	return spec, nil
}

// interfaceCounts is the total interface count per container (links + attachments across
// every network) -- what resolves "sole interface implies default".
func interfaceCounts(cfg *config.Turnip) map[string]int {
	counts := map[string]int{}
	for name, c := range cfg.Containers {
		counts[name] = len(c.Links)
	}
	for _, net := range cfg.Networks {
		for cname := range net.Attach {
			counts[cname]++
		}
	}
	return counts
}

// ownsDefault resolves whether an endpoint owns the container's default route: a configured
// default, OR being the container's sole interface (config guarantees at most one configured
// default, so the implicit case can't conflict).
func ownsDefault(configuredDefault bool, ifaceCount int) bool {
	return configuredDefault || ifaceCount == 1
}

// routerSysctls is the sysctl set for a router netns:
//
// The set is ORDERED (see dp.Sysctl): everything is pinned explicitly rather than trusting the
// netns to have inherited a safe value -- a fresh IPv4 netns copies conf/{all,default} from
// init_net's LIVE values, so a host override would otherwise leak in.
//
//   - ip_forward on (we route) -- FIRST, because writing it re-derives the per-interface RFC1812
//     router defaults (send_redirects / accept_source_route flip toward enabled); the pins below
//     must follow so they win;
//   - all.rp_filter=0 so the per-veth values are authoritative (the kernel uses max(conf.all,
//     conf.<if>); pinning all=0 also blocks a host all=1 from forcing strict everywhere);
//   - all.accept_source_route=0: forwarding => RFC1812 defaults this TRUE; drop source-routed
//     (SRR) packets (acceptance ANDs all+iface, so all=0 closes it everywhere);
//   - all.send_redirects=0: a forwarding router emits ICMP redirects by default -- off. (In our
//     /32 p2p model they never fire anyway, since in/out veths always differ -- hardening hygiene);
//   - nf_conntrack_tcp_loose=0 so conntrack does NOT pick up mid-stream connections: a bare
//     out-of-state ACK/RST/FIN becomes `ct invalid` (-> the forward chain's invalid drop) instead
//     of a forwarded `ct new`. Safe because the routed model is strictly symmetric (each
//     container's traffic crosses its one router) -- loose only matters for asymmetric paths;
//   - nf_conntrack_tcp_be_liberal=0 (the default, pinned): out-of-WINDOW TCP also stays `ct
//     invalid`, reinforcing tcp_loose=0;
//   - ipv6 disabled router-wide (the routed model has no L2 path between containers, so
//     killing v6 on the router severs inter-container v6);
//   - then per fabric veth: proxy_arp=1 (answer the gateway ARP), rp_filter=1 (STRICT -- the
//     anti-spoof pin, paired with that veth's /32 route), and send_redirects=0 (redirects fire if
//     EITHER all OR the iface is set, and the iface can inherit a host conf/default=1, so the
//     all=0 above is not sufficient on its own).
//
// Pure lowering -- the result is written (by dataplane.WriteSysctls, in apply) AFTER the veths
// exist (the per-veth conf.<if> dirs) and after the nft load (the conntrack knobs need the netns
// conntrack hooks, which the ct-state rules register). uplinkRouterIf is the uplink veth's
// router-side name (or "" for no uplink); it gets strict rp_filter too -- the reverse path for an
// internet source is the default route = the uplink, while a container-spoofed source resolves to
// its own /32 veth (not the uplink) and is dropped (the anti-spoof pin).
func routerSysctls(routerIfs []string, uplinkRouterIf string) []dp.Sysctl {
	s := []dp.Sysctl{
		dp.Sys("net.ipv4.ip_forward", "1"), // FIRST: re-derives router defaults the pins below override
		dp.Sys("net.ipv4.conf.all.rp_filter", "0"),
		dp.Sys("net.ipv4.conf.all.accept_source_route", "0"),
		dp.Sys("net.ipv4.conf.all.send_redirects", "0"),
		dp.Sys("net.netfilter.nf_conntrack_tcp_loose", "0"),
		dp.Sys("net.netfilter.nf_conntrack_tcp_be_liberal", "0"),
		dp.Sys("net.ipv6.conf.all.disable_ipv6", "1"),
		dp.Sys("net.ipv6.conf.default.disable_ipv6", "1"),
	}
	for _, rif := range routerIfs {
		s = append(s,
			dp.Sys("net.ipv4.conf."+rif+".proxy_arp", "1"),
			dp.Sys("net.ipv4.conf."+rif+".rp_filter", "1"),
			dp.Sys("net.ipv4.conf."+rif+".send_redirects", "0"),
		)
	}
	if uplinkRouterIf != "" {
		s = append(s,
			dp.Sys("net.ipv4.conf."+uplinkRouterIf+".rp_filter", "1"),
			dp.Sys("net.ipv4.conf."+uplinkRouterIf+".send_redirects", "0"),
		)
	}
	return s
}

// buildFlows lowers a network's internal flows to the dataplane Flow matrix, resolving each
// endpoint container name to its /32. icmp / port="any" need a second nft map shape that isn't
// wired yet, so they're rejected here (the caller wraps with the network name). Egress/ingress
// flows are the edge (buildEgressAllows / buildIngress), not the forward matrix.
func buildFlows(net config.Network) ([]dp.Flow, error) {
	ip := map[string]netip.Addr{}
	for cname, att := range net.Attach {
		ip[cname] = att.IP
	}
	var flows []dp.Flow
	for _, fl := range net.Flows {
		f, ok := fl.(*config.InternalFlow)
		if !ok {
			continue
		}
		if f.Proto == config.ProtoICMP || f.Port.Any {
			return nil, fmt.Errorf("icmp / port=\"any\" in flows not wired yet")
		}
		flows = append(flows, dp.Flow{
			FromIP: ip[f.From], ToIP: ip[f.To],
			Proto: string(f.Proto), Port: uint16(f.Port.Port),
		})
	}
	return flows, nil
}

// buildEgressAllows translates the network's egress flows into the dataplane edge allows: a
// container that may initiate out the uplink, wide (proto="any" => All) or scoped (proto, port).
// Multiple egress flows for one container fold into a single allow (grouped by source /32).
func buildEgressAllows(net config.Network) []dp.EgressAllow {
	byIP := map[netip.Addr]*dp.EgressAllow{}
	var order []netip.Addr
	for _, fl := range net.Flows {
		f, ok := fl.(*config.EgressFlow)
		if !ok {
			continue
		}
		ip := net.Attach[f.From].IP
		a := byIP[ip]
		if a == nil {
			a = &dp.EgressAllow{IP: ip}
			byIP[ip] = a
			order = append(order, ip)
		}
		if f.Proto.Any {
			a.All = true
			continue
		}
		sc := dp.EgressScope{}
		for _, p := range f.Proto.List {
			sc.Protos = append(sc.Protos, string(p))
		}
		if f.Port != nil && !f.Port.Any {
			sc.Port = f.Port.Port
		}
		a.Rules = append(a.Rules, sc)
	}
	var allows []dp.EgressAllow
	for _, ip := range order {
		allows = append(allows, *byIP[ip])
	}
	return allows
}

// buildIngress translates the network's ingress flows into the host DNAT rules
// (Listen:host_port -> container:port) and the matching router forward-chain allows.
func buildIngress(net config.Network) ([]dp.DNAT, []dp.IngressAllow) {
	var dnats []dp.DNAT
	var allows []dp.IngressAllow
	for _, fl := range net.Flows {
		ing, ok := fl.(*config.IngressFlow)
		if !ok {
			continue
		}
		contIP := net.Attach[ing.To].IP
		dnats = append(dnats, dp.DNAT{
			Listen: ing.Listen, Proto: string(ing.Proto),
			HostPort: ing.HostPort, ContIP: contIP, ContPort: ing.Port,
		})
		allows = append(allows, dp.IngressAllow{
			IP: contIP, Proto: string(ing.Proto), Port: ing.Port,
		})
	}
	return dnats, allows
}

// hostsFile is the /etc/hosts body for a container: localhost, the container's own name on
// each network it's attached to (so it resolves itself -- the bind-mount replaces podman's
// generated file), then the peers it may reach by name (the targets of its outbound flows;
// flows are directional, so from == container). Ports main.py hosts_file / container_peers.
func hostsFile(cfg *config.Turnip, container string) string {
	var b strings.Builder
	b.WriteString("127.0.0.1 localhost\n")
	for _, netName := range sortedKeys(cfg.Networks) {
		if att, ok := cfg.Networks[netName].Attach[container]; ok {
			fmt.Fprintf(&b, "%s %s\n", att.IP, container)
		}
	}
	peers := map[string]netip.Addr{}
	for _, netName := range sortedKeys(cfg.Networks) {
		net := cfg.Networks[netName]
		if _, ok := net.Attach[container]; !ok {
			continue
		}
		for _, fl := range net.Flows {
			// only internal flows name a reachable peer; egress/ingress cross the edge.
			if f, ok := fl.(*config.InternalFlow); ok && f.From == container {
				peers[f.To] = net.Attach[f.To].IP
			}
		}
	}
	for _, name := range sortedKeys(peers) {
		fmt.Fprintf(&b, "%s %s\n", peers[name], name)
	}
	return b.String()
}
