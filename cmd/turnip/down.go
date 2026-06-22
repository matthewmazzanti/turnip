// down.go -- the teardown half of the up/down orchestration. Shares the config/runtime
// resolution and host-edge scrub with up (up = down + build); see up.go.

package main

import (
	"fmt"

	"git.lan/mmazzanti/turnip/internal/netns"
)

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
