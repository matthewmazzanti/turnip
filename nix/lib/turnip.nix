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
#                               a podman package + the owner's uid. Returns just the unit, including
#                               the ordering after the owner's /run/user/<uid> tmpfs.
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
  # a turnip package, a model (a Nix attrset, toJSON'd via layer 2), a podman package (so the rootless
  # newuidmap/newgidmap wrappers line up), and the rootless owner's `uid`. nft is on the unit's PATH
  # (turnip forks it) and defaults to pkgs.nftables.
  #
  # The `uid` is a hard requirement: turnip pins its netns + state under /run/user/<uid> and podman
  # uses it as XDG_RUNTIME_DIR, and that path is logind's per-user tmpfs (`user-runtime-dir@<uid>`).
  # If turnip ran before that tmpfs is mounted it would either miss the dir or get its writes shadowed
  # by the later mount -- so the unit orders after it. Returns just the unit:
  #
  #   systemd.services.turnip = turnipLib.turnipService {
  #     turnip = pkg; podman = config.virtualisation.podman.package; uid = 1001;
  #     config = { runtime.user = "homelab"; /* ... */ };
  #   };
  turnipService =
    { config
    , uid
    , turnip ? defaultTurnip
    , podman ? pkgs.podman
    , nft ? pkgs.nftables
    }:
    let
      bin = turnipWithConfig { inherit config turnip nft podman; };
      exe = pkgs.lib.getExe bin;
      # The owner's user session. We really only need the /run/user/<uid> tmpfs, created by
      # `user-runtime-dir@<uid>.service`; `user@<uid>.service` is `After` it, so this covers it (and is
      # the variant that's been VM-tested). Drop to `user-runtime-dir@<uid>.service` to depend on
      # strictly the dir.
      userSession = "user@${toString uid}.service";
    in
    {
      description = "turnip routed container network";
      wantedBy = [ "multi-user.target" ];
      after = [ "network.target" "podman.service" userSession ];
      wants = [ userSession ];
      path = [ nft podman ];
      serviceConfig = {
        Type = "oneshot";
        RemainAfterExit = true;
        ExecStart = "${exe} up";
        ExecStop = "${exe} down";
      };
    };
}
