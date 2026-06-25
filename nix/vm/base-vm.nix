# base-vm.nix -- the capability base: "a host that can run turnip" -- rootless podman owned by the
# run user (homelab), plus the nft / ip tooling turnip drives. This is the foundation a turnip host
# needs, independent of the interactive dev substrate (nix/vm/base-interactive.nix). Imported by the
# host dev VM and the hermetic check's host node -- both the interactive.host / test.host stacks in
# nix/vm/default.nix.
{ pkgs, ... }:
{
  system.stateVersion = "25.05";

  # Don't let the host firewall interfere with turnip's netns/nft work.
  networking.firewall.enable = false;

  # Rootless podman, owned by `homelab` (the runtime.user in the configs). linger =>
  # /run/user/<uid> + the pause process exist with no active login; autoSubUidGidRange
  # => the subuid/subgid range podman maps. uid is pinned so state paths
  # (/run/user/1001/turnip/...) are deterministic.
  virtualisation.podman.enable = true;
  users.users.homelab = {
    isNormalUser = true;
    uid = 1001;
    linger = true;
    autoSubUidGidRange = true;
  };

  environment.systemPackages = [
    pkgs.nftables # `nft` -- turnip drives nftables
    pkgs.iproute2 # ip/ss -- inspect links/routes
    pkgs.jq # inspect `nft -j`
  ];
}
