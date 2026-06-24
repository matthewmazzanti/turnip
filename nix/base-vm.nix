# base-vm.nix -- the shared dev-VM substrate for the interactive host/world VMs: the read-only
# 9p mount of the repo, ssh/console access + the admin login, and systemd-networkd with eth0 as
# the user-mode MANAGEMENT NIC (the host-forwarded ssh port -- out of band, not the modeled
# topology). The LAN interface (eth1) and any bridge are configured per role (host-vm / world-vm),
# and the LAN segment itself is wired at launch by the justfile (a qemu mcast socket NIC).
#
# Stacks under the qemu-vm machinery; host-vm also stacks turnip-host.nix (rootless podman).
{ ... }:
{
  system.stateVersion = "25.05";

  # Explicit, declarative interfaces via systemd-networkd. eth0 is the qemu user-mode NIC --
  # management only (slirp's 10.0.2.15, the forwarded ssh port); the modeled LAN lives on eth1.
  networking.useNetworkd = true;
  networking.useDHCP = false;
  networking.firewall.enable = false; # don't let the host firewall interfere with turnip / probes
  systemd.network.networks."10-mgmt" = {
    matchConfig.Name = "eth0";
    networkConfig.DHCP = "yes";
    linkConfig.RequiredForOnline = "no"; # don't block boot waiting on the mgmt link
  };

  # Admin user: console login (dev/dev) + key ssh + passwordless sudo. The rootless-podman owner
  # (homelab) is defined by turnip-host.nix and gets its login creds in host-vm.nix.
  users.users.dev = {
    isNormalUser = true;
    extraGroups = [ "wheel" ];
    password = "dev";
    openssh.authorizedKeys.keyFiles = [ ./testvm_key.pub ];
  };
  security.sudo.wheelNeedsPassword = false;
  services.openssh.enable = true;

  virtualisation = {
    memorySize = 2048;
    cores = 2;
    graphics = false; # serial console in the terminal; headless + ssh otherwise
    diskSize = 8192; # MB; room for podman images
    # Live-mount the host repo at /mnt/turnip over 9p (tag `turnip`), READ-ONLY. We declare only
    # the GUEST mount; the host path (absolute) is injected at launch by the justfile.
    fileSystems."/mnt/turnip" = {
      device = "turnip"; # 9p mount tag; host path injected via QEMU_OPTS at launch
      fsType = "9p";
      neededForBoot = false;
      options = [
        "trans=virtio"
        "version=9p2000.L"
        "msize=16384"
        "x-systemd.requires=modprobe@9pnet_virtio.service"
        "ro"
        "nofail"
      ];
    };
  };
}
