// Command turnip builds a persistent, rootless, routed-L3 container network for
// podman from a declarative turnip.json -- the Go rewrite.
//
// The reference Python implementation lives under ./old (its modules map onto the
// planned package layout below); README.md at the repo root and ./docs (CONFIG-SKETCH.md,
// IMPLEMENTATION-PLAN.md) describe the model this port targets. The kernel-interface
// primitives are proven in ./spike/go-netns-bootstrap.
//
// Planned layout as the port grows (cmd is the imperative shell; internal/* are the
// pure-ish mechanism, mirroring the Python modules):
//
//	cmd/turnip          this CLI + orchestration (ports main.py's up/down dispatch)
//	internal/config     the declarative model + validation (ports config.py)
//	internal/netns      podman-userns bootstrap, netns lifecycle, the SCM_RIGHTS fd bridge
//	internal/dataplane  gateway/veth/route wiring + the nft flow matrix (ports the wiring)
//
// Run as root (rootful): `turnip up` creates + wires the namespaces the config
// implies; `turnip down` removes them. Bare `turnip` defaults to `up`.
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"

	"git.lan/mmazzanti/turnip/internal/netns"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "turnip: "+err.Error())
		os.Exit(1)
	}
}

// run is the CLI surface: an optional -c/--config, then an `up`/`down` subcommand
// (bare invocation defaults to `up`), mirroring the Python parse_args.
func run(args []string) error {
	fs := flag.NewFlagSet("turnip", flag.ContinueOnError)
	configPath := fs.String("config", "", "config file (default: $TURNIP_CONFIG, else ./turnip.json)")
	fs.StringVar(configPath, "c", "", "shorthand for --config")
	fs.Usage = usage
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil // -h/--help: usage already printed, exit 0
		}
		return err
	}

	cmd := "up" // bare `turnip` defaults to up
	if fs.NArg() > 0 {
		cmd = fs.Arg(0)
	}
	switch cmd {
	case "up":
		return up(*configPath)
	case "down":
		return down(*configPath)
	case "probe":
		// run a command inside a container's netns via `podman unshare nsenter` -- the
		// test/debug probe. `turnip probe <container> -- <cmd...>`.
		return probe(*configPath, fs.Args()[1:])
	case netns.ProvisionArg:
		// the re-exec'd provisioner child (inside podman's userns) -- not a user command;
		// the name/path specs are the positional args after the sentinel.
		return netns.RunProvisioner(fs.Args()[1:])
	case netns.TeardownArg:
		// the re-exec'd teardown child (inside podman's userns); the paths to scrub follow.
		return netns.RunTeardown(fs.Args()[1:])
	default:
		usage()
		return fmt.Errorf("unknown command %q", cmd)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `turnip -- a persistent rootless routed-L3 container network for podman.

usage:
  turnip [up]        create + wire the namespaces the config implies (default)
  turnip down        tear them down
  turnip probe <target> -- <cmd...>
                     run a command inside a netns (fd-exec; no podman run).
                     <target>: a container name, or router:<net>


flags:
  -c, --config PATH  config file (default: $TURNIP_CONFIG, else ./turnip.json)
`)
}
