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
	"git.lan/mmazzanti/turnip/internal/dataplane"
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

// --- up / down -------------------------------------------------------------

// up loads the config, resolves the owner, bootstraps the netns the config implies, then
// configures the dataplane over them (inline, until the phases reveal real boundaries to
// extract). up = down + build (clean slate). The routed fabric is wired; container-local
// setup and the host edge are still TODO sections.
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
	linkSpecs, err := buildLinkSpecs(cfg)
	if err != nil {
		return err
	}
	var allLinks []dataplane.LinkSpec
	for _, cname := range sortedKeys(linkSpecs) {
		allLinks = append(allLinks, linkSpecs[cname]...)
	}
	if err := dataplane.ValidateLinkAnchors(allLinks); err != nil {
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

	// loopback up in every netns (routers + containers); all fds are present post-Bootstrap.
	for _, sp := range specs {
		fd, _ := set.FD(sp.Name)
		if err := dataplane.SetLoUp(fd); err != nil {
			return fmt.Errorf("%s lo: %w", sp.Name, err)
		}
	}

	// --- the routed fabric: per network, the L3 wiring in its router netns ---------------
	// gateway + a /32 routed veth per attached container (the container end is born in the
	// container netns -- the attachment), router sysctls, and the nft flow matrix. Drives the
	// live Set the rootful parent holds (netlink/nft over the fd, set.Enter for sysctls).
	counts := interfaceCounts(cfg)
	for _, netName := range sortedKeys(cfg.Networks) {
		net := cfg.Networks[netName]
		routerFd, ok := set.FD("router:" + netName)
		if !ok {
			return fmt.Errorf("router netns %q missing from the bootstrap set", netName)
		}
		if err := dataplane.CreateGateway(routerFd, dataplane.Gateway{IfName: net.GatewayIf, Addr: net.Gateway}); err != nil {
			return fmt.Errorf("network %q: %w", netName, err)
		}
		fmt.Printf("  router %s: gateway %s/%d on %s\n", netName, net.Gateway, config.HOSTPrefix, net.GatewayIf)

		var routerIfs []string
		for _, cname := range sortedKeys(net.Attach) {
			att := net.Attach[cname]
			contFd, ok := set.FD("container:" + cname)
			if !ok {
				return fmt.Errorf("container netns %q missing from the bootstrap set", cname)
			}
			rif, err := routerIf(cname)
			if err != nil {
				return err
			}
			// effective default-route ownership: configured default, OR the container's
			// sole interface (config guarantees at most one configured default).
			ep := dataplane.Endpoint{
				RouterIf: rif,
				ContIf:   att.Interface,
				IP:       att.IP,
				Default:  att.Default || counts[cname] == 1,
			}
			if err := dataplane.Connect(routerFd, contFd, net.Gateway, ep); err != nil {
				return fmt.Errorf("network %q connect %q: %w", netName, cname, err)
			}
			routerIfs = append(routerIfs, rif)
			fmt.Printf("    %s: %s %s/%d -> gw %s%s <-> %s\n",
				cname, att.Interface, att.IP, config.HOSTPrefix, net.Gateway, defaultMark(ep.Default), rif)
		}

		// uplink (the host edge): the /31 veth across init<->router, host NAT (masquerade) +
		// container routes. Wired here, before the router sysctls/nft, so the uplink veth
		// exists when they reference it (its rp_filter + the egress allows).
		uplinkRouterIf := ""
		var edge *dataplane.Edge
		if net.Uplink != nil {
			uplink := dataplane.Uplink{
				HostIf: net.Uplink.HostIf, RouterIf: net.Uplink.RouterIf,
				HostIP: net.Uplink.Link, RouterIP: net.Uplink.Link.Next(),
			}
			if err := dataplane.HostEdgeConnect(routerFd, uplink); err != nil {
				return fmt.Errorf("network %q uplink: %w", netName, err)
			}
			var containerIPs []netip.Addr
			for _, cname := range sortedKeys(net.Attach) {
				containerIPs = append(containerIPs, net.Attach[cname].IP)
			}
			dnats, ingressAllows := buildIngress(net)
			if err := dataplane.ConfigureHostNAT(netName, uplink, containerIPs, dnats); err != nil {
				return fmt.Errorf("network %q host nat: %w", netName, err)
			}
			uplinkRouterIf = uplink.RouterIf
			edge = &dataplane.Edge{
				UplinkIf: uplink.RouterIf,
				Egress:   buildEgressAllows(net),
				Ingress:  ingressAllows,
			}
			fmt.Printf("    uplink: %s <-> %s (%s/%d), host masquerade + %d route(s) + %d dnat\n",
				uplink.HostIf, uplink.RouterIf, uplink.HostIP, config.LINKPrefix, len(containerIPs), len(dnats))
		}

		// sysctls: applied AFTER the veths exist (the per-veth conf.<if> dirs). /proc/sys is
		// per-process-netns with no netlink verb, so it needs a setns episode (set.Enter).
		sysctls := dataplane.RouterSysctls(routerIfs, uplinkRouterIf)
		if err := set.Enter("router:"+netName, func() error { return dataplane.WriteSysctls(sysctls) }); err != nil {
			return fmt.Errorf("network %q sysctls: %w", netName, err)
		}
		fmt.Printf("    sysctls: ip_forward + per-veth proxy_arp/rp_filter (strict) + ipv6 off\n")

		// nft: the forward flow matrix + uplink egress allows + the router's own-address
		// lockdown. Resolve flow endpoints (container names) to IPs; icmp / port="any" in
		// flows isn't wired yet.
		ip := map[string]netip.Addr{}
		for cname, att := range net.Attach {
			ip[cname] = att.IP
		}
		var flows []dataplane.Flow
		for _, fl := range net.Flows {
			if fl.Proto == config.ProtoICMP || fl.Port.Any {
				return fmt.Errorf("network %q: icmp / port=\"any\" in flows not wired yet", netName)
			}
			flows = append(flows, dataplane.Flow{
				FromIP: ip[fl.From], ToIP: ip[fl.To],
				Proto: string(fl.Proto), Port: uint16(fl.Port.Port),
			})
		}
		// nft acts on the process netns, so apply it inside a set.Enter episode: the forked
		// nft child inherits the router netns.
		rs := dataplane.BuildNFT(flows, edge)
		if err := set.Enter("router:"+netName, func() error { return nftlib.Load(rs) }); err != nil {
			return fmt.Errorf("network %q nft: %w", netName, err)
		}
		fmt.Printf("    nft: forward flow matrix (%d flow(s)) + input lockdown\n", len(flows))
	}

	// --- container-local: the generated /etc/hosts + links (lo is up above) ---------------
	// each container resolves itself (its own ip/name on each network) and the peers its
	// outbound flows may reach. Written to <state>/containers/<name>/hosts (the provisioner
	// made the dir, on the shared user-runtime tmpfs); chowned to the owner so podman
	// bind-mounts it to /etc/hosts cleanly. Then any links -- host netdev holes into the
	// netns, the deliberate L2 escape outside every router and its nft policy.
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
				if err := dataplane.LinkConnect(contFd, spec); err != nil {
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

// down scrubs the netns: removing each pinned netns destroys everything inside it (links,
// routes, sysctls, the future nft table), so this is the whole teardown for the routed
// fabric. The host-edge state (in the init netns) is a separate, later concern.
func down(configPath string) error {
	cfg, err := loadConfig(configPath)
	if err != nil {
		return err
	}
	owner, stateDir, err := resolveRuntime(cfg.Runtime)
	if err != nil {
		return err
	}
	specs := netnsSpecs(cfg, stateDir)
	paths := make([]string, len(specs))
	for i, s := range specs {
		paths[i] = s.Path
	}
	// clear the init-netns host edge (uplink veths + nat zones), then scrub the netns (which
	// reaps everything inside: links, routes, sysctls, the nft table).
	// TODO: refuse when a live podman container still holds a target netns (would orphan it).
	if err := clearHostEdge(cfg); err != nil {
		return err
	}
	if err := netns.Teardown(owner, paths); err != nil {
		return err
	}
	fmt.Printf("down: scrubbed %d netns under %s\n", len(paths), stateDir)
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
func buildLinkSpecs(cfg *config.Turnip) (map[string][]dataplane.LinkSpec, error) {
	out := map[string][]dataplane.LinkSpec{}
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
func buildLinkSpec(cname string, link config.Link) (dataplane.LinkSpec, error) {
	b := link.Base()
	spec := dataplane.LinkSpec{
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
		if err := dataplane.TeardownHostEdge(netName, net.Uplink.HostIf); err != nil {
			return fmt.Errorf("network %q host edge teardown: %w", netName, err)
		}
	}
	return nil
}

// buildEgressAllows translates each attachment's egress config into the dataplane edge
// allows: a container that may initiate out the uplink, any (All) or scoped (proto, port).
func buildEgressAllows(net config.Network) []dataplane.EgressAllow {
	var allows []dataplane.EgressAllow
	for _, cname := range sortedKeys(net.Attach) {
		att := net.Attach[cname]
		if !att.Egress.All && len(att.Egress.Rules) == 0 {
			continue
		}
		a := dataplane.EgressAllow{IP: att.IP, All: att.Egress.All}
		for _, r := range att.Egress.Rules {
			sc := dataplane.EgressScope{}
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
func buildIngress(net config.Network) ([]dataplane.DNAT, []dataplane.IngressAllow) {
	var dnats []dataplane.DNAT
	var allows []dataplane.IngressAllow
	for _, cname := range sortedKeys(net.Attach) {
		att := net.Attach[cname]
		for _, ing := range att.Ingress {
			dnats = append(dnats, dataplane.DNAT{
				Listen: ing.Listen, Proto: string(ing.Proto),
				HostPort: ing.HostPort, ContIP: att.IP, ContPort: ing.Port,
			})
			allows = append(allows, dataplane.IngressAllow{
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
