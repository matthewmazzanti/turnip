# NixOS dev VM for turnip -- the interactive rootful sandbox.
#
# A persistent qemu VM (boots under KVM) with the turnip repo LIVE-MOUNTED (9p) at
# /mnt/turnip, so host edits run inside with no rebuild -- the fast manual-debug loop.
# The flake's testVM stacks the layers explicitly (turnip-host -> this -> customizations),
# so this module is ONLY the VM-specific bits: the 9p mount, ssh/console access, login
# users. The HERMETIC packaged turnip + assertions live in the integration check, not here.
#
# Build + run:
#   nix build .#vm
#   ./result/bin/run-turnip-vm          # boots qemu; persists ./turnip.qcow2
#
# Drive it:
#   ssh -i nix/testvm_key -p 2222 dev@localhost        # admin (sudo)
#   ssh -i nix/testvm_key -p 2222 homelab@localhost    # the rootless run user
#   homelab@turnip$ TURNIP_CONFIG=... turnip up        # `turnip` is the live /mnt/turnip/src
#
# This module is just the dev-VM-specific config (ssh, users, 9p mount, console). The
# editable turnip env and the test OCI image are supplied by the flake via the shared
# turnip.{env,testImage} options (see flake.nix's testVM), not through this module's args.
#
# `turnip` / `python3` / `pytest` come from that editable env -- they run the LIVE
# /mnt/turnip/src with uv.lock-pinned deps (no wrapper, no nixpkgs/lock skew).
{ ... }:
{
  networking.hostName = "turnip";

  # Admin user for console/ssh debugging.
  users.users.dev = {
    isNormalUser = true;
    extraGroups = [ "wheel" ];
    password = "dev"; # throwaway VM: console login dev/dev
    openssh.authorizedKeys.keyFiles = [ ./testvm_key.pub ];
  };
  # The shared module defines `homelab` (rootless podman owner); add VM login creds.
  users.users.homelab = {
    password = "homelab";
    openssh.authorizedKeys.keyFiles = [ ./testvm_key.pub ];
  };

  security.sudo.wheelNeedsPassword = false;
  services.openssh.enable = true;

  # `just itest` runs the suite against the live 9p source; the shared base provides
  # TURNIP_TEST_IMAGE + PYTHONDONTWRITEBYTECODE and loads the OCI image at boot, and the
  # recipe passes TURNIP_INTEGRATION itself.

  virtualisation = {
    memorySize = 2048;
    cores = 2;
    graphics = false; # serial console in the terminal; headless + ssh otherwise
    diskSize = 8192; # MB; room for podman images
    forwardPorts = [
      {
        from = "host";
        host.port = 2222;
        guest.port = 22;
      }
    ];
    # Live-mount the host repo at /mnt/turnip over 9p (tag `turnip`), READ-ONLY.
    #
    # We declare only the GUEST mount here; the host side is supplied at LAUNCH by
    # `just vm` (QEMU_OPTS with an ABSOLUTE path). NOT `sharedDirectories.source`,
    # because the NixOS run script `cd`s into a temp dir before exec'ing qemu: a baked
    # relative "." would resolve there, and an absolute path would hardcode one
    # checkout. Injecting $PWD from the repo root at launch is correct + checkout-free.
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
