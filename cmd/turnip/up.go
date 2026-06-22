// up.go -- the imperative shell: config/runtime IO and the up/down orchestration. up is two
// passes over a clean seam: buildPlan (plan.go) lowers the config to a fully-resolved Plan
// (pure, all fallible resolution up front), then applyPlan (apply.go) pushes that Plan to the
// live netns Set. The dataplane verbs live in internal/dataplane; this layer only sequences.

package main

import (
	"fmt"
	"os"
	osuser "os/user"
	"path/filepath"
	"sort"
	"strconv"

	"git.lan/mmazzanti/turnip/internal/config"
	dp "git.lan/mmazzanti/turnip/internal/dataplane"
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

// up loads the config and resolves the owner, then runs two passes: buildPlan lowers the
// config to a fully-resolved Plan (pure -- every fallible resolution happens here, before any
// mutation), and applyPlan pushes it to the freshly-bootstrapped netns. up = down + build: it
// clears prior host-edge state before rebuilding (Bootstrap recreates the netns clean).
func up(configPath string) error {
	cfg, err := loadConfig(configPath)
	if err != nil {
		return err
	}
	owner, stateDir, err := resolveRuntime(cfg.Runtime)
	if err != nil {
		return err
	}

	// define: lower the config to a fully-resolved Plan (pure -- no kernel). Fails fast on
	// oversized ifnames / unwired flows / link conflicts before any host read or mutation.
	plan, err := buildPlan(cfg, owner, stateDir)
	if err != nil {
		return err
	}
	fmt.Printf("up: %d network(s), %d container(s); owner=%s, state=%s\n",
		len(cfg.Networks), len(cfg.Containers), owner.User, stateDir)

	// preflight: validate the plan's link anchors against the LIVE init netns (read-only) --
	// still fail-fast, but this is the first thing that reads the kernel, kept out of buildPlan.
	if err := preflightAnchors(plan); err != nil {
		return err
	}

	// up = down + build: clear prior host-edge state (init-netns veths + nat zones) before
	// rebuilding. The netns themselves are recreated clean by Bootstrap (pinNetns is idempotent).
	if err := clearHostEdge(cfg); err != nil {
		return err
	}

	// run: bootstrap the netns the plan implies, then apply the plan over them.
	set, err := netns.Bootstrap(owner, plan.Specs)
	if err != nil {
		return fmt.Errorf("bootstrap netns: %w", err)
	}
	defer set.Close() // the netns persist by bind-mount; this drops only our fd handles
	fmt.Printf("  bootstrapped %d netns (pinned under %s)\n", len(plan.Specs), stateDir)

	return applyPlan(set, plan)
}

// preflightAnchors validates every link's host-side anchor against the live init netns
// (exists, right kind, not wireless/primary) -- read-only kernel IO, run as root in the init
// netns before any mutation. The pure cross-spec conflicts were caught in buildPlan; this is
// the host-dependent half, kept out of the pure lowering and pushed to its own phase.
func preflightAnchors(plan *Plan) error {
	var links []dp.LinkSpec
	for _, cp := range plan.Containers {
		links = append(links, cp.Links...)
	}
	return dp.ValidateLinkAnchors(links)
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

// sortedKeys returns m's keys sorted -- deterministic ordering for the specs + logs.
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
