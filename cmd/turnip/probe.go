// probe.go -- the test/debug probe: run a command inside a container's netns without a full
// `podman run`. It re-uses the same owner resolution as up/down, resolves the container's pinned
// netns path under the state dir, and hands off to netns.Probe (`podman unshare nsenter --net`).
// The probe command's exit code becomes turnip's exit code, so a harness can read connect-vs-drop
// directly (e.g. `curl` exits non-zero on a refused connection).

package main

import (
	"fmt"
	"os"
	"path/filepath"

	"git.lan/mmazzanti/turnip/internal/netns"
)

// probe parses `<container> -- <cmd...>`, resolves the owner + state dir (as up does), and runs
// cmd inside that container's netns. On a clean launch it exits with the probe's own status.
func probe(configPath string, args []string) error {
	container, cmd, err := splitProbe(args)
	if err != nil {
		return err
	}

	// Owner + state dir come from the same runtime resolution up uses, so the pinned path
	// matches what Bootstrap created. The config is only consulted for runtime.{user,state_dir}.
	cfg, err := loadConfig(configPath)
	if err != nil {
		return err
	}
	owner, stateDir, err := resolveRuntime(cfg.Runtime)
	if err != nil {
		return err
	}

	nsPath := filepath.Join(stateDir, "containers", container, "netns")
	code, err := netns.Probe(owner, nsPath, cmd)
	if err != nil {
		return err
	}
	os.Exit(code)
	return nil // unreachable
}

// splitProbe parses `<container> -- <cmd...>`: the container name before the "--" separator,
// the command after it.
func splitProbe(args []string) (container string, cmd []string, err error) {
	for i, a := range args {
		if a == "--" {
			if i != 1 || i+1 >= len(args) {
				break
			}
			return args[0], args[i+1:], nil
		}
	}
	return "", nil, fmt.Errorf("usage: turnip probe <container> -- <cmd...>")
}
