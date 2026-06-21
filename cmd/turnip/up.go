// up.go -- the imperative shell: config/runtime IO and the up/down orchestration over
// internal/config (the model) and internal/netns (the live runtime state). The dataplane
// steps are skeletoned here and filled in over internal/dataplane (ports of main.py's
// create_gateway/connect/build_nft/link_connect/host_edge_connect).

package main

import (
	"fmt"
	"os"
	osuser "os/user"
	"path/filepath"
	"sort"
	"strconv"

	"git.lan/mmazzanti/turnip/internal/config"
	"git.lan/mmazzanti/turnip/internal/netns"
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
// configures the dataplane over them. up = down + build (clean slate); the dataplane steps
// are skeletons for now -- the netns are really created, nothing inside them yet.
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

	// The dataplane -- skeletoned, NOT implemented. Each step drives the live Set the
	// rootful parent holds (set.Enter for sysctls, netlink/nft over the fd).
	if err := configureRouters(cfg, set); err != nil {
		return err
	}
	if err := wireContainers(cfg, set); err != nil {
		return err
	}
	if err := configureLinks(cfg, set); err != nil {
		return err
	}
	if err := configureHostEdge(cfg, set); err != nil {
		return err
	}

	fmt.Println("  (skeleton: routers/containers/links/host-edge dataplane not yet implemented)")
	return nil
}

// down tears it all down. Skeleton: the host-edge state lives in the init netns (parent),
// the netns themselves are removed inside podman (unmount + rm the bind-mounts) -- the
// sibling of Bootstrap/Provision.
func down(configPath string) error {
	cfg, err := loadConfig(configPath)
	if err != nil {
		return err
	}
	if _, _, err := resolveRuntime(cfg.Runtime); err != nil {
		return err
	}
	// TODO(teardown): teardownHostEdge (parent) + a podman-unshare phase that unmounts +
	// removes each netns pin (the Provision counterpart).
	return fmt.Errorf("down: teardown not implemented yet (Go port in progress)")
}

// --- dataplane skeleton (not implemented) ----------------------------------

// configureRouters wires each router netns: the dummy gateway holding <gateway>/32, a /32
// routed veth per attached container (router end + container end + the link-scope route to
// the gateway), then the router dataplane -- sysctls (ip_forward, per-veth proxy_arp +
// strict rp_filter, ipv6 off) and the nft forward flow matrix. All over the netns fd
// (set.Enter + netlink/nft).
func configureRouters(cfg *config.Turnip, set *netns.Set) error {
	// TODO: ports main.py create_gateway / connect / router_sysctls / build_nft.
	return nil
}

// wireContainers brings up loopback in each container netns and writes its generated
// /etc/hosts (its own names per network + the peers its outbound flows may reach).
func wireContainers(cfg *config.Turnip, set *netns.Set) error {
	// TODO: ports main.py set_lo_up / container_peers / hosts_file / write_hosts.
	return nil
}

// configureLinks moves a host netdev into each container's netns -- the deliberate L2 trust
// escape, outside every router and its nft policy. veth->bridge / veth->host / macvlan /
// ipvlan are owned (reaped with the netns); phys is borrowed (returned to the host on down).
func configureLinks(cfg *config.Turnip, set *netns.Set) error {
	// TODO: ports main.py validate_link_anchors / link_connect.
	return nil
}

// configureHostEdge wires each network's uplink: the point-to-point /31 veth across the
// init<->router boundary, the host-side masquerade/DNAT, the router's default route, and
// the uplinked router's dataplane. The rootful, init-netns half.
func configureHostEdge(cfg *config.Turnip, set *netns.Set) error {
	// TODO: ports main.py teardown_host_edge / host_edge_connect / configure_host_nat / build_host_nft.
	return nil
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
