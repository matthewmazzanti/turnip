# nix/demo/base.nix -- the uninteresting substrate for the turnip demo VM, split out of homelab.nix
# so that file can focus on the turnip + quadlet-nix + networkd setup. Everything here is plumbing the
# demo needs but isn't ABOUT: rootless podman + its owner, console autologin + ssh, DNS for the image
# pull, and VM sizing. A plain NixOS module -- pull it in via `imports = [ ./base.nix ]`.
{ pkgs, ... }:
let
  owner = "homelab"; # the rootless-podman owner / login user (homelab.nix defines its own copy)
in
{
  system.stateVersion = "25.05";
  networking.hostName = "turnip-demo";
  networking.firewall.enable = false; # don't let the host firewall interfere with turnip's netns/nft

  # Rootless podman owned by `owner`: linger => /run/user/<uid> + the pause process exist with no
  # login; autoSubUidGidRange => the subuid/subgid range podman maps; uid pinned so state paths
  # (/run/user/1001/turnip/...) are stable. The demo runs AS this user (autologin + the dev key over
  # ssh) so `podman exec` into the containers needs no sudo.
  virtualisation.podman.enable = true;
  users.users.${owner} = {
    isNormalUser = true;
    uid = 1001; # the single source of truth; homelab.nix's `uid` looks this up via config.users.users
    linger = true;
    autoSubUidGidRange = true;
    password = owner;
    openssh.authorizedKeys.keyFiles = [ ../vm/testvm_key.pub ];
  };
  services.getty.autologinUser = owner;
  services.openssh.enable = true;

  # DNS, so rootless podman can resolve the registry to pull the image. resolved picks up the DNS
  # server br0's DHCP lease hands out (slirp's proxy); /etc/resolv.conf -> the resolved stub.
  services.resolved.enable = true;

  # ssh on a forwarded port (host :2224 -> guest 22). graphics=false so `nix run .#demo` lands on
  # the serial console.
  virtualisation.forwardPorts = [{ from = "host"; host.port = 2224; guest.port = 22; }];
  virtualisation = {
    graphics = false;
    memorySize = 2048;
    cores = 2;
    diskSize = 8192; # room for the netshoot image
  };
  users.motd = ''

    ===========================================================================
      turnip demo -- a routed podman fabric (zwave / hass / proxy) on this host.

      Run the guided tour:    turnip-demo
      Poke it by hand:        podman exec <container> curl http://<peer>   (plain HTTP, :80)
                              podman exec <container> ip -br addr
    ===========================================================================
  '';
}
