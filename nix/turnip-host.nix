# Shared substrate: "a host that can run turnip" -- rootless podman owned by the run
# user, plus the nft / ip tooling turnip drives. Imported by BOTH the interactive dev
# VM (nix/testvm.nix) and the hermetic NixOS integration tests (tests/nixos/*).
#
# It deliberately does NOT decide how `turnip` itself is delivered: the dev VM runs
# the live 9p-mounted source (no rebuild on edit), the tests install the uv2nix-built
# package. Each consumer puts a `turnip` on PATH; this module is everything else.
{ pkgs, ... }:
{
  system.stateVersion = "25.05";

  # Don't let the host firewall interfere with turnip's netns/nft experiments.
  networking.firewall.enable = false;

  # Rootless podman, owned by `homelab` (the runtime.user in the configs). linger =>
  # /run/user/<uid> + the pause process exist with no active login; autoSubUidGidRange
  # => the subuid/subgid range podman maps. uid is pinned so state paths
  # (/run/user/1001/turnip/...) are deterministic for the probes.
  virtualisation.podman.enable = true;
  users.users.homelab = {
    isNormalUser = true;
    uid = 1001;
    linger = true;
    autoSubUidGidRange = true;
  };

  environment.systemPackages = [
    pkgs.nftables # `nft` -- turnip shells out to it (nftlib.load)
    pkgs.iproute2 # ip/ss -- probes read `ip -j`
    pkgs.jq # inspect `nft -j`
  ];
}
