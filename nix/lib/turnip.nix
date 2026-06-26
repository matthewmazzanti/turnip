# nix/lib/turnip.nix -- the layered "wrap turnip for Nix" helpers.
#
# Three composable layers, each building on the one before, so a consumer can enter at
# whichever level it has:
#
#   1. turnipWithConfigFile  -- wrap the turnip binary around an arbitrary on-disk config
#                               file (the lowest layer; bakes --config + the runtime deps).
#   2. turnipWithConfig      -- wrap turnip around a Nix ATTRSET: builtins.toJSON -> a
#                               generated turnip.json -> layer 1.
#   3. turnipService         -- build the up/down service UNIT (the value for
#                               `systemd.services.<name>`) from a turnip package + a model attrset +
#                               a podman package. Returns just the unit; the caller adds the
#                               deployment-specific user-session ordering.
#
# What gets baked, and why (see the binaries turnip actually execs):
#   - nft     -- turnip forks `nft -j -f -` (internal/nftlib). It has a NixOS fallback search,
#                but we bake it so the wrapper doesn't depend on one.
#   - podman  -- turnip execs a BARE `podman` (`podman unshare ...`, internal/netns) with NO
#                fallback search and no config-path override plumbed, so PATH is the only lever.
#                Under a systemd oneshot's clean PATH it MUST be baked. Injectable, because
#                rootless podman needs NixOS's newuidmap/newgidmap wrappers -- a consumer with
#                `virtualisation.podman` should pass `config.virtualisation.podman.package`.
#   - ip      -- NOT baked: turnip does all link/addr/route work via netlink syscalls
#                (vishvananda/netlink), never the `ip` binary. iproute2 is only an INSPECTION
#                tool (for a demo / operator), not a turnip runtime dep.
#
# writeShellApplication PREPENDS runtimeInputs to PATH rather than clobbering it, so a service's
# own `path` (e.g. the system podman) still augments what the wrapper bakes.
{ pkgs, turnip }:
let
  defaultTurnip = turnip;
in
rec {
  # Layer 1: wrap turnip around a concrete config file on disk.
  turnipWithConfigFile =
    { configFile
    , name ? "turnip"
    , turnip ? defaultTurnip
    , nft ? pkgs.nftables
    , podman ? pkgs.podman
    , extraRuntimeInputs ? [ ]
    }:
    pkgs.writeShellApplication {
      inherit name;
      runtimeInputs = [ turnip nft podman ] ++ extraRuntimeInputs;
      # exec so the wrapper is transparent (signals, exit code -- the latter matters for `probe`,
      # whose exit status is the in-netns command's). --config is baked; up/down/probe pass through.
      text = ''exec turnip --config ${configFile} "$@"'';
    };

  # Layer 2: wrap turnip around a Nix attrset (the authoring layer -- toJSON the model, generate
  # the on-disk turnip.json, then hand to layer 1).
  turnipWithConfig =
    { config
    , name ? "turnip"
    , turnip ? defaultTurnip
    , nft ? pkgs.nftables
    , podman ? pkgs.podman
    , extraRuntimeInputs ? [ ]
    }:
    turnipWithConfigFile {
      inherit name turnip nft podman extraRuntimeInputs;
      configFile = pkgs.writeText "${name}.json" (builtins.toJSON config);
    };

  # Layer 3: build the turnip up/down SERVICE UNIT -- the value for `systemd.services.<name>` -- from
  # a turnip package, a model (a Nix attrset, toJSON'd via layer 2), and a podman package (so the
  # rootless newuidmap/newgidmap wrappers line up). nft is on the unit's PATH (turnip forks it) and
  # defaults to pkgs.nftables.
  #
  # Returns JUST the unit. It sets the ordering any turnip deployment wants (network.target,
  # podman.service) but NOT the rootless owner's `user@<uid>.service` -- this generic function can't
  # know the uid -- so the caller merges that in (the netns are created inside that user's podman
  # userns). E.g.:
  #
  #   systemd.services.turnip = lib.mkMerge [
  #     (turnipLib.turnipService { turnip = pkg; podman = config.virtualisation.podman.package;
  #                                config = { runtime.user = "homelab"; /* ... */ }; })
  #     { after = [ "user@1001.service" ]; wants = [ "user@1001.service" ]; }
  #   ];
  turnipService =
    { config
    , turnip ? defaultTurnip
    , podman ? pkgs.podman
    , nft ? pkgs.nftables
    }:
    let
      bin = turnipWithConfig { inherit config turnip nft podman; };
      exe = pkgs.lib.getExe bin;
    in
    {
      description = "turnip routed container network";
      wantedBy = [ "multi-user.target" ];
      after = [ "network.target" "podman.service" ];
      path = [ nft podman ];
      serviceConfig = {
        Type = "oneshot";
        RemainAfterExit = true;
        ExecStart = "${exe} up";
        ExecStop = "${exe} down";
      };
    };
}
