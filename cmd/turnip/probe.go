// probe.go -- the test/debug probe: run a command inside a netns without a full `podman run`. It
// re-uses the same owner resolution as up/down, resolves the target's pinned netns path under the
// state dir, and hands off to netns.Probe (`podman unshare nsenter --net`). The probe command's
// exit code becomes turnip's exit code, so a harness can read connect-vs-drop directly (e.g.
// `curl` exits non-zero on a refused connection).
//
// The target selects which netns: a bare name (or "container:<name>") is a container; "router:<net>"
// is that network's router netns -- so a test can inspect the gateway / nft ruleset / sysctls too.

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"git.lan/mmazzanti/turnip/internal/netns"
)

// probe parses `<target> -- <cmd...>`, resolves the owner + state dir (as up does), and runs cmd
// inside that target's netns. On a clean launch it exits with the probe's own status.
func probe(configPath string, args []string) error {
	target, cmd, err := splitProbe(args)
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

	nsPath, err := netnsPath(stateDir, target)
	if err != nil {
		return err
	}
	code, err := netns.Probe(owner, nsPath, cmd)
	if err != nil {
		return err
	}
	os.Exit(code)
	return nil // unreachable
}

// netnsPath maps a probe target to its pinned netns path. "router:<net>" -> the router netns;
// anything else (optionally "container:<name>") -> that container's netns. Mirrors netnsSpecs.
func netnsPath(stateDir, target string) (string, error) {
	if net, ok := strings.CutPrefix(target, "router:"); ok {
		if net == "" {
			return "", fmt.Errorf("probe: empty router name")
		}
		return filepath.Join(stateDir, "routers", net), nil
	}
	name := strings.TrimPrefix(target, "container:")
	if name == "" {
		return "", fmt.Errorf("probe: empty container name")
	}
	return filepath.Join(stateDir, "containers", name, "netns"), nil
}

// splitProbe parses `<target> -- <cmd...>`: the target before the "--" separator, the command
// after it.
func splitProbe(args []string) (target string, cmd []string, err error) {
	for i, a := range args {
		if a == "--" {
			if i != 1 || i+1 >= len(args) {
				break
			}
			return args[0], args[i+1:], nil
		}
	}
	return "", nil, fmt.Errorf("usage: turnip probe <target> -- <cmd...>")
}
