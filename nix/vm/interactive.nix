# interactive.nix -- the carve-outs that turn a role base (host-base / world-base) into a standalone
# interactive dev VM, shared by both interactive.host and interactive.world. It adds what the
# hermetic check doesn't want: the read-only 9p mount of the repo, the user-mode MANAGEMENT NIC
# (eth0, host-forwarded ssh -- out of band, not the modeled LAN on eth1), the admin login, and the
# qemu sizing. The networkd backend, firewall, and stateVersion come from the role base it's stacked
# on; the LAN (eth1) is the role base's job too.
{ ... }:
{
  # eth0 is the qemu user-mode NIC -- management only (slirp's 10.0.2.15, the forwarded ssh port);
  # the modeled LAN lives on eth1 (configured by the role base).
  systemd.network.networks."10-mgmt" = {
    matchConfig.Name = "eth0";
    networkConfig.DHCP = "yes";
    linkConfig.RequiredForOnline = "no"; # don't block boot waiting on the mgmt link
  };

  # Admin user: console login (dev/dev) + key ssh + passwordless sudo. (The rootless-podman owner
  # homelab is defined by host-base; the world peer authorizes root/dev via world-base.)
  users.users.dev = {
    isNormalUser = true;
    extraGroups = [ "wheel" ];
    password = "dev";
    openssh.authorizedKeys.keyFiles = [ ./testvm_key.pub ];
  };
  security.sudo.wheelNeedsPassword = false;
  services.openssh.enable = true;

  # graphics=true keeps -nographic OUT of the generated runner, so `up` can detach with qemu's own
  # -daemonize (no sleep/setsid hack -- see nix/vm.sh). The serial console isn't on stdio by default
  # then; vm.sh wires it per command (run: -serial mon:stdio; up: -serial file). qemu.consoles pins
  # the serial port as the primary console (graphics=true otherwise prefers tty0), so the full boot
  # lands on the serial -- the `run` terminal and the `up` log.
  virtualisation = {
    memorySize = 2048;
    cores = 2;
    graphics = true;
    qemu.consoles = [ "tty0" "ttyS0,115200n8" ];
    diskSize = 8192; # MB; room for podman images
    # Live-mount the host repo at /mnt/turnip over 9p (tag `turnip`), READ-ONLY. We declare only the
    # GUEST mount; the host path (absolute) is injected at launch by the justfile.
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
