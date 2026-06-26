# nix/lib/turnip.nix -- the layered "wrap turnip for Nix" helpers.
#
# Three composable layers, each building on the one before, so a consumer can enter at
# whichever level it has:
#
#   1. turnipWithConfigFile  -- wrap the turnip binary around an arbitrary on-disk config
#                               file (the lowest layer; bakes --config + the runtime deps).
#   2. turnipWithConfig      -- wrap turnip around a Nix ATTRSET: builtins.toJSON -> a
#                               generated turnip.json -> layer 1.
#   3. turnipService         -- a `systemd.services.<name>` FRAGMENT that runs `up` on start
#                               and `down` on stop. Resolves its binary from a passed-in
#                               `package` (e.g. a layer 1/2 wrapper, or any bin) OR builds one
#                               from `config`/`configFile`.
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

  # Layer 3: a systemd-service fragment driving up/down. Give it a prebuilt `package` (a layer 1/2
  # wrapper or any bin exposing `<name> up|down`), or a `config` attrset / `configFile` to build one.
  #
  # turnip is rootful AND must run after the rootless-podman owner's user session exists (the netns
  # are created inside that user's podman userns). `requiresUserSession = <uid>` wires that real
  # ordering: after user@<uid>.service + an ExecStartPre that waits for /run/user/<uid>.
  turnipService =
    { package ? null
    , config ? null
    , configFile ? null
    , name ? "turnip"
    , turnip ? defaultTurnip
    , nft ? pkgs.nftables
    , podman ? pkgs.podman
    , requiresUserSession ? null # uid of the rootless-podman owner, or null
    , after ? [ ]
    , wants ? [ ]
    , path ? [ ]
    , extraServiceConfig ? { }
    }:
    let
      bin =
        if package != null then package
        else if config != null then turnipWithConfig { inherit config name turnip nft podman; }
        else if configFile != null then turnipWithConfigFile { inherit configFile name turnip nft podman; }
        else throw "turnipService: pass one of `package`, `config`, or `configFile`";

      userUnit = "user@${toString requiresUserSession}.service";
      sessionOrdering = requiresUserSession != null;
    in
    {
      systemd.services.${name} = {
        description = "turnip routed container network (${name})";
        wantedBy = [ "multi-user.target" ];
        after = [ "network.target" "podman.service" ]
          ++ pkgs.lib.optional sessionOrdering userUnit
          ++ after;
        wants = pkgs.lib.optional sessionOrdering userUnit ++ wants;
        path = [ nft podman ] ++ path;
        serviceConfig = {
          Type = "oneshot";
          RemainAfterExit = true;
          # Wait for the owner's user session (XDG_RUNTIME_DIR) so `podman unshare` has a userns to
          # enter -- the same gate the integration harness uses before `turnip up`.
          ExecStartPre = pkgs.lib.optional sessionOrdering
            "${pkgs.bash}/bin/bash -c 'until test -d /run/user/${toString requiresUserSession}; do sleep 0.2; done'";
          ExecStart = "${bin}/bin/${name} up";
          ExecStop = "${bin}/bin/${name} down";
        } // extraServiceConfig;
      };
    };
}
