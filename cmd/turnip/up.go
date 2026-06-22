// up.go -- the imperative shell: config/runtime IO and the up/down orchestration over
// internal/config (the model) and internal/netns (the live runtime state). The dataplane
// steps are skeletoned here and filled in over internal/dataplane (ports of main.py's
// create_gateway/connect/build_nft/link_connect/host_edge_connect).

package main

import (
	"fmt"
	"net/netip"
	"os"
	osuser "os/user"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"git.lan/mmazzanti/turnip/internal/config"
	dp "git.lan/mmazzanti/turnip/internal/dataplane"
	"git.lan/mmazzanti/turnip/internal/netns"
	"git.lan/mmazzanti/turnip/internal/nftlib"
)

// loadConfig discovers, reads, and validates the config: an explicit --config, else
// $TURNIP_CONFIG, else ./turnip.json. The file read is the shell's job; the model +
// validation live in internal/config (mirrors the Python main.load_config).
func loadConfig(path string) (*config.Turnip, error) {
	if path == "" {
		path = os.Getenv("TURNIP_CONFIG")
	}
	if path == "" {
		path = "turnip.json"
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}
	return config.Parse(data)
}

// resolveRuntime fills Runtime's environment-dependent bits, resolved by the TARGET user
// so it stays correct under sudo. turnip is rootful: it runs as root and drops to the
// rootless-podman owner (runtime.user, else $SUDO_USER) to enter podman's namespaces.
// Dirs follow the target uid (/run/user/<uid>/turnip), NOT $XDG_RUNTIME_DIR, which under
// sudo is root's. (Ports main.py resolve_runtime, rootful-only.)
func resolveRuntime(rt config.Runtime) (owner netns.Owner, stateDir string, err error) {
	if os.Geteuid() != 0 {
		return owner, "", fmt.Errorf("turnip is rootful: run via sudo")
	}
	user := rt.User
	if user == "" {
		user = os.Getenv("SUDO_USER")
	}
	if user == "" {
		return owner, "", fmt.Errorf(
			"set runtime.user (the rootless-podman owner), or invoke via sudo so $SUDO_USER is set")
	}
	u, err := osuser.Lookup(user)
	if err != nil {
		return owner, "", fmt.Errorf("lookup user %q: %w", user, err)
	}
	uid, _ := strconv.Atoi(u.Uid)
	gid, _ := strconv.Atoi(u.Gid)
	if uid == 0 {
		return owner, "", fmt.Errorf("runtime.user %q resolves to root; it must be the unprivileged owner", user)
	}
	stateDir = rt.StateDir
	if stateDir == "" {
		stateDir = fmt.Sprintf("/run/user/%d/turnip", uid)
	}
	return netns.Owner{User: user, UID: uid, GID: gid, Home: u.HomeDir}, stateDir, nil
}

// netnsSpecs is the set of netns the config implies: a router netns per network and a
// netns per container (the unit podman attaches to). Names are keyed by type ("router:" /
// "container:") so a network and a container that share a name can't collide; paths pin
// them under the state dir (routers/<net>, containers/<name>/netns).
//
// This is only the netns the config requires -- the full runtime model (build_model: the
// config -> Container/Network/Endpoint graph the dataplane walks) is a separate record
// still to come.
func netnsSpecs(cfg *config.Turnip, stateDir string) []netns.Spec {
	var specs []netns.Spec
	for _, net := range sortedKeys(cfg.Networks) {
		specs = append(specs, netns.Spec{
			Name: "router:" + net,
			Path: filepath.Join(stateDir, "routers", net),
		})
	}
	for _, c := range sortedKeys(cfg.Containers) {
		specs = append(specs, netns.Spec{
			Name: "container:" + c,
			Path: filepath.Join(stateDir, "containers", c, "netns"),
		})
	}
	return specs
}

// --- up ---------------------------------------------------------------------

// up loads the config, resolves the owner, bootstraps the netns the config implies, then
// configures the dataplane over them. up = down + build (clean slate): it clears prior
// host-edge state and rebuilds the routed fabric (per-network L3 wiring) plus the
// container-local setup (/etc/hosts + links). The per-step work lives in the helpers below.
func up(configPath string) error {
	cfg, err := loadConfig(configPath)
	if err != nil {
		return err
	}
	owner, stateDir, err := resolveRuntime(cfg.Runtime)
	if err != nil {
		return err
	}

	specs := netnsSpecs(cfg, stateDir)
	fmt.Printf("up: %d network(s), %d container(s); owner=%s, state=%s\n",
		len(cfg.Networks), len(cfg.Containers), owner.User, stateDir)

	// build + validate the container link specs up front -- fail fast on a bad anchor before
	// any netns or host-edge mutation.
	linkSpecs, err := validateLinkSpecs(cfg)
	if err != nil {
		return err
	}

	// up = down + build: clear prior host-edge state (init-netns veths + nat zones) before
	// rebuilding. The netns themselves are recreated clean by Bootstrap (pinNetns is idempotent).
	if err := clearHostEdge(cfg); err != nil {
		return err
	}

	set, err := netns.Bootstrap(owner, specs)
	if err != nil {
		return fmt.Errorf("bootstrap netns: %w", err)
	}
	defer set.Close() // the netns persist by bind-mount; this drops only our fd handles
	fmt.Printf("  bootstrapped %d netns (pinned under %s)\n", len(specs), stateDir)

	if err := setLoopbackUp(set, specs); err != nil {
		return err
	}

	// the routed fabric: per network, the L3 wiring in its router netns.
	counts := interfaceCounts(cfg)
	for _, netName := range sortedKeys(cfg.Networks) {
		if err := configureNetwork(set, netName, cfg.Networks[netName], counts); err != nil {
			return err
		}
	}

	// container-local: the generated /etc/hosts + links (lo is up above).
	return configureContainers(set, cfg, owner, stateDir, linkSpecs)
}

// validateLinkSpecs builds every container's link specs and validates their anchors up front,
// keyed by container for the per-container connect loop -- fail fast on a bad anchor before
// any netns or host-edge mutation.
func validateLinkSpecs(cfg *config.Turnip) (map[string][]dp.LinkSpec, error) {
	linkSpecs, err := buildLinkSpecs(cfg)
	if err != nil {
		return nil, err
	}
	var allLinks []dp.LinkSpec
	for _, cname := range sortedKeys(linkSpecs) {
		allLinks = append(allLinks, linkSpecs[cname]...)
	}
	if err := dp.ValidateLinkAnchors(allLinks); err != nil {
		return nil, err
	}
	return linkSpecs, nil
}

// setLoopbackUp brings lo up in every bootstrapped netns (routers + containers); all fds are
// present post-Bootstrap.
func setLoopbackUp(set *netns.Set, specs []netns.Spec) error {
	for _, sp := range specs {
		fd, _ := set.FD(sp.Name)
		if err := dp.SetLoUp(fd); err != nil {
			return fmt.Errorf("%s lo: %w", sp.Name, err)
		}
	}
	return nil
}

// configureNetwork wires one network's routed fabric in its router netns: the gateway, a
// routed /32 veth per attached container, the optional host-edge uplink, then the router
// sysctls and nft policy (applied last, after the veths they reference exist). Drives the
// live Set the rootful parent holds (netlink/nft over the fd, set.Enter for sysctls).
func configureNetwork(set *netns.Set, netName string, net config.Network, counts map[string]int) error {
	routerFd, ok := set.FD("router:" + netName)
	if !ok {
		return fmt.Errorf("router netns %q missing from the bootstrap set", netName)
	}
	if err := dp.CreateGateway(routerFd, dp.Gateway{IfName: net.GatewayIf, Addr: net.Gateway}); err != nil {
		return fmt.Errorf("network %q: %w", netName, err)
	}
	fmt.Printf("  router %s: gateway %s/%d on %s\n", netName, net.Gateway, config.HOSTPrefix, net.GatewayIf)

	routerIfs, err := connectContainers(set, routerFd, netName, net, counts)
	if err != nil {
		return err
	}

	uplinkRouterIf, edge, err := configureUplink(routerFd, netName, net)
	if err != nil {
		return err
	}

	if err := applyRouterSysctls(set, netName, routerIfs, uplinkRouterIf); err != nil {
		return err
	}
	return applyRouterNFT(set, netName, net, edge)
}

// connectContainers wires a routed /32 veth from the router netns into each attached
// container's netns (the container end is born there -- the attachment), returning the
// router-side interface names for the sysctls.
func connectContainers(set *netns.Set, routerFd int, netName string, net config.Network, counts map[string]int) ([]string, error) {
	var routerIfs []string
	for _, cname := range sortedKeys(net.Attach) {
		att := net.Attach[cname]
		contFd, ok := set.FD("container:" + cname)
		if !ok {
			return nil, fmt.Errorf("container netns %q missing from the bootstrap set", cname)
		}
		rif, err := routerIf(cname)
		if err != nil {
			return nil, err
		}
		// effective default-route ownership: configured default, OR the container's
		// sole interface (config guarantees at most one configured default).
		ep := dp.Endpoint{
			RouterIf: rif,
			ContIf:   att.Interface,
			IP:       att.IP,
			Default:  ownsDefault(att.Default, counts[cname]),
		}
		if err := dp.Connect(routerFd, contFd, net.Gateway, ep); err != nil {
			return nil, fmt.Errorf("network %q connect %q: %w", netName, cname, err)
		}
		routerIfs = append(routerIfs, rif)
		fmt.Printf("    %s: %s %s/%d -> gw %s%s <-> %s\n",
			cname, att.Interface, att.IP, config.HOSTPrefix, net.Gateway, defaultMark(ep.Default), rif)
	}
	return routerIfs, nil
}

// configureUplink wires the optional host edge: the /31 veth across init<->router, then host
// NAT (masquerade + container routes + ingress DNAT). It's done before the router sysctls/nft
// so the uplink veth exists when they reference it (its rp_filter + the egress allows).
// Returns the uplink's router-side interface (for the sysctls) and the nft edge (egress +
// ingress allows); both zero when the network has no uplink.
func configureUplink(routerFd int, netName string, net config.Network) (string, *dp.Edge, error) {
	if net.Uplink == nil {
		return "", nil, nil
	}
	uplink := dp.Uplink{
		HostIf:   net.Uplink.HostIf,
		RouterIf: net.Uplink.RouterIf,
		HostIP:   net.Uplink.Link,
		RouterIP: net.Uplink.Link.Next(),
	}
	if err := dp.HostEdgeConnect(routerFd, uplink); err != nil {
		return "", nil, fmt.Errorf("network %q uplink: %w", netName, err)
	}
	var containerIPs []netip.Addr
	for _, cname := range sortedKeys(net.Attach) {
		containerIPs = append(containerIPs, net.Attach[cname].IP)
	}
	dnats, ingressAllows := buildIngress(net)
	if err := dp.ConfigureHostNAT(netName, uplink, containerIPs, dnats); err != nil {
		return "", nil, fmt.Errorf("network %q host nat: %w", netName, err)
	}
	edge := &dp.Edge{
		UplinkIf: uplink.RouterIf,
		Egress:   buildEgressAllows(net),
		Ingress:  ingressAllows,
	}
	fmt.Printf("    uplink: %s <-> %s (%s/%d), host masquerade + %d route(s) + %d dnat\n",
		uplink.HostIf, uplink.RouterIf, uplink.HostIP, config.LINKPrefix, len(containerIPs), len(dnats))
	return uplink.RouterIf, edge, nil
}

// applyRouterSysctls writes the router netns sysctls AFTER its veths exist (they reference the
// per-veth conf.<if> dirs). /proc/sys is per-process-netns with no netlink verb, so it needs a
// setns episode (set.Enter).
func applyRouterSysctls(set *netns.Set, netName string, routerIfs []string, uplinkRouterIf string) error {
	sysctls := dp.RouterSysctls(routerIfs, uplinkRouterIf)
	if err := set.Enter("router:"+netName, func() error {
		return dp.WriteSysctls(sysctls)
	}); err != nil {
		return fmt.Errorf("network %q sysctls: %w", netName, err)
	}
	fmt.Printf("    sysctls: ip_forward + per-veth proxy_arp/rp_filter (strict) + ipv6 off\n")
	return nil
}

// applyRouterNFT loads the router's nft policy: the forward flow matrix + uplink egress allows
// + the own-address lockdown. Flow endpoints (container names) resolve to IPs; icmp /
// port="any" in flows isn't wired yet. nft acts on the process netns, so it's applied inside a
// set.Enter episode (the forked nft child inherits the router netns).
func applyRouterNFT(set *netns.Set, netName string, net config.Network, edge *dp.Edge) error {
	flows, err := buildFlows(net)
	if err != nil {
		return fmt.Errorf("network %q: %w", netName, err)
	}
	rs := dp.BuildNFT(flows, edge)
	if err := set.Enter("router:"+netName, func() error { return nftlib.Load(rs) }); err != nil {
		return fmt.Errorf("network %q nft: %w", netName, err)
	}
	fmt.Printf("    nft: forward flow matrix (%d flow(s)) + input lockdown\n", len(flows))
	return nil
}

// configureContainers writes each container's generated /etc/hosts and connects its links.
// Each container resolves itself (its own ip/name on each network) and the peers its outbound
// flows may reach. hosts is written to <state>/containers/<name>/hosts (the provisioner made
// the dir, on the shared user-runtime tmpfs) and chowned to the owner so podman bind-mounts it
// to /etc/hosts cleanly. Links are host netdev holes into the netns -- the deliberate L2
// escape outside every router and its nft policy.
func configureContainers(set *netns.Set, cfg *config.Turnip, owner netns.Owner, stateDir string, linkSpecs map[string][]dp.LinkSpec) error {
	for _, cname := range sortedKeys(cfg.Containers) {
		hostsPath := filepath.Join(stateDir, "containers", cname, "hosts")
		if err := os.WriteFile(hostsPath, []byte(hostsFile(cfg, cname)), 0o644); err != nil {
			return fmt.Errorf("container %q hosts: %w", cname, err)
		}
		if err := os.Chown(hostsPath, owner.UID, owner.GID); err != nil {
			return fmt.Errorf("container %q hosts chown: %w", cname, err)
		}

		if links := linkSpecs[cname]; len(links) > 0 {
			contFd, ok := set.FD("container:" + cname)
			if !ok {
				return fmt.Errorf("container netns %q missing from the bootstrap set", cname)
			}
			for _, spec := range links {
				if err := dp.LinkConnect(contFd, spec); err != nil {
					return err
				}
			}
			fmt.Printf("  container %s: hosts written + %d link(s)\n", cname, len(links))
			continue
		}
		fmt.Printf("  container %s: hosts written\n", cname)
	}
	return nil
}

// --- helpers (model derivation -- the seed of build_model) ------------------

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
	case *config.MacvlanLink:
		spec.Kind, spec.Parent, spec.Mode = "macvlan", l.Parent, string(l.Mode)
	case *config.IpvlanLink:
		spec.Kind, spec.Parent, spec.Mode = "ipvlan", l.Parent, string(l.Mode)
	case *config.PhysLink:
		spec.Kind, spec.Dev = "phys", l.Dev
	default:
		return spec, fmt.Errorf("container %q: unknown link type %T", cname, link)
	}
	return spec, nil
}

// interfaceCounts is the total interface count per container (links + attachments across
// every network) -- what resolves "sole interface implies default". (Seed of build_model.)
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

// buildFlows lowers a network's flows to the dataplane Flow list, resolving each endpoint
// container name to its /32. icmp / port="any" need a second nft map shape that isn't wired
// yet, so they're rejected here (the caller wraps with the network name). Ports the flow
// half of build_nft's caller.
func buildFlows(net config.Network) ([]dp.Flow, error) {
	ip := map[string]netip.Addr{}
	for cname, att := range net.Attach {
		ip[cname] = att.IP
	}
	var flows []dp.Flow
	for _, fl := range net.Flows {
		if fl.Proto == config.ProtoICMP || fl.Port.Any {
			return nil, fmt.Errorf("icmp / port=\"any\" in flows not wired yet")
		}
		flows = append(flows, dp.Flow{
			FromIP: ip[fl.From], ToIP: ip[fl.To],
			Proto: string(fl.Proto), Port: uint16(fl.Port.Port),
		})
	}
	return flows, nil
}

func defaultMark(d bool) string {
	if d {
		return " (default)"
	}
	return ""
}

// clearHostEdge removes the init-netns host-edge state (uplink veths + nat zones) for every
// uplinked network -- the parent half of teardown, used by down and up's clean slate.
func clearHostEdge(cfg *config.Turnip) error {
	for _, netName := range sortedKeys(cfg.Networks) {
		net := cfg.Networks[netName]
		if net.Uplink == nil {
			continue
		}
		if err := dp.TeardownHostEdge(netName, net.Uplink.HostIf); err != nil {
			return fmt.Errorf("network %q host edge teardown: %w", netName, err)
		}
	}
	return nil
}

// buildEgressAllows translates each attachment's egress config into the dataplane edge
// allows: a container that may initiate out the uplink, any (All) or scoped (proto, port).
func buildEgressAllows(net config.Network) []dp.EgressAllow {
	var allows []dp.EgressAllow
	for _, cname := range sortedKeys(net.Attach) {
		att := net.Attach[cname]
		if !att.Egress.All && len(att.Egress.Rules) == 0 {
			continue
		}
		a := dp.EgressAllow{IP: att.IP, All: att.Egress.All}
		for _, r := range att.Egress.Rules {
			sc := dp.EgressScope{}
			for _, p := range r.Proto {
				sc.Protos = append(sc.Protos, string(p))
			}
			if r.Port != nil && !r.Port.Any {
				sc.Port = r.Port.Port
			}
			a.Rules = append(a.Rules, sc)
		}
		allows = append(allows, a)
	}
	return allows
}

// buildIngress translates each attachment's ingress config into the host DNAT rules
// (Listen:host_port -> container:port) and the matching router forward-chain allows.
func buildIngress(net config.Network) ([]dp.DNAT, []dp.IngressAllow) {
	var dnats []dp.DNAT
	var allows []dp.IngressAllow
	for _, cname := range sortedKeys(net.Attach) {
		att := net.Attach[cname]
		for _, ing := range att.Ingress {
			dnats = append(dnats, dp.DNAT{
				Listen: ing.Listen, Proto: string(ing.Proto),
				HostPort: ing.HostPort, ContIP: att.IP, ContPort: ing.Port,
			})
			allows = append(allows, dp.IngressAllow{
				IP: att.IP, Proto: string(ing.Proto), Port: ing.Port,
			})
		}
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
			if fl.From == container {
				peers[fl.To] = net.Attach[fl.To].IP
			}
		}
	}
	for _, name := range sortedKeys(peers) {
		fmt.Fprintf(&b, "%s %s\n", peers[name], name)
	}
	return b.String()
}

// sortedKeys returns m's keys sorted -- deterministic ordering for the specs + logs.
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
