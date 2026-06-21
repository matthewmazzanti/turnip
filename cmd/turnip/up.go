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

	// TODO(teardown): up = down + build -- clear prior host-edge state and rebuild the
	// netns clean before the bootstrap below.

	set, err := netns.Bootstrap(owner, specs)
	if err != nil {
		return fmt.Errorf("bootstrap netns: %w", err)
	}
	defer set.Close() // the netns persist by bind-mount; this drops only our fd handles
	fmt.Printf("  bootstrapped %d netns (pinned under %s)\n", len(specs), stateDir)

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

		// sysctls: applied AFTER the veths exist (the per-veth conf.<if> dirs). /proc/sys is
		// per-process-netns with no netlink verb, so it needs a setns episode (set.Enter).
		sysctls := dataplane.RouterSysctls(routerIfs)
		if err := set.Enter("router:"+netName, func() error { return dataplane.WriteSysctls(sysctls) }); err != nil {
			return fmt.Errorf("network %q sysctls: %w", netName, err)
		}
		fmt.Printf("    sysctls: ip_forward + per-veth proxy_arp/rp_filter (strict) + ipv6 off\n")

		// nft: the forward flow matrix + the router's own-address lockdown. Resolve flow
		// endpoints (container names) to IPs; icmp / port="any" in flows isn't wired yet.
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
		rs := dataplane.BuildNFT(flows)
		if err := set.Enter("router:"+netName, func() error { return nftlib.Load(rs) }); err != nil {
			return fmt.Errorf("network %q nft: %w", netName, err)
		}
		fmt.Printf("    nft: forward flow matrix (%d flow(s)) + input lockdown\n", len(flows))
	}

	// --- container-local setup (lo + /etc/hosts + links) -- TODO ------------------------
	// per container: bring up lo; move host-netdev links into the netns (the L2 escape --
	// veth/macvlan/ipvlan owned, phys borrowed); write the generated /etc/hosts.

	// --- the host edge (uplink veth + masquerade/DNAT) -- TODO --------------------------
	// per network with an uplink: the /31 veth across init<->router, host masquerade/DNAT,
	// the router default route, the uplink rp_filter sysctl + nft egress/ingress edge rules.

	fmt.Println("  (container-local setup + host edge not yet implemented)")
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
	// TODO(host-edge): teardownHostEdge (init-netns parent) once uplinks/links are wired.
	// TODO: refuse when a live podman container still holds a target netns (would orphan it).
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

// sortedKeys returns m's keys sorted -- deterministic ordering for the specs + logs.
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
