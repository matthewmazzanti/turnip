# NixOS dev VM for turnip -- the interactive rootful sandbox.
#
# A persistent qemu VM (boots under KVM) with the turnip repo LIVE-MOUNTED (9p) at
# /mnt/turnip, so host edits run inside with no rebuild -- the fast manual-debug loop.
# It imports the shared substrate (nix/turnip-host.nix: rootless podman + homelab +
# nft/ip tooling) and adds the VM-specific bits: the live-source `turnip` wrapper, the
# 9p mount, ssh/console access. The HERMETIC packaged turnip + assertions live in the
# NixOS integration tests (tests/nixos/), not here.
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
# turnipEnv (the uv2nix env with turnip installed EDITABLE against /mnt/turnip) + testImage
# (the integration test OCI image) come from the flake via specialArgs.
{ pkgs, turnipEnv, testImage, ... }:
{
  imports = [ ./turnip-host.nix ];

  networking.hostName = "turnip";

  # `turnip` / `python3` / `pytest` are the editable uv2nix env (below) -- they run the
  # LIVE /mnt/turnip/src with uv.lock-pinned deps (no wrapper, no nixpkgs/lock skew).

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

  environment.systemPackages = [ turnipEnv ]; # editable turnip + python3 + pytest

  # `just itest` runs the full suite, podman-attach included, so carry the test OCI image:
  # load it into homelab's rootless store at boot, and point the test at it + run-container.sh.
  # (PYTHONDONTWRITEBYTECODE: the live src is a ro 9p mount, so suppress bytecode writes.)
  environment.variables = {
    PYTHONDONTWRITEBYTECODE = "1";
    TURNIP_TEST_IMAGE = "turnip-test:latest";
    TURNIP_RUNCONTAINER = "/mnt/turnip/run-container.sh";
  };
  systemd.services.turnip-test-image = {
    description = "load the integration test OCI image into homelab's rootless podman";
    wantedBy = [ "multi-user.target" ];
    after = [ "user@1001.service" ];
    wants = [ "user@1001.service" ];
    serviceConfig = { Type = "oneshot"; RemainAfterExit = true; };
    # rootless podman needs the setuid newuidmap/newgidmap (/run/wrappers/bin) + podman
    # itself (/run/current-system/sw/bin) -- the system PATH, which a unit otherwise lacks.
    script = ''
      until test -d /run/user/1001; do sleep 0.2; done
      export PATH=/run/wrappers/bin:/run/current-system/sw/bin
      runuser -u homelab -- env XDG_RUNTIME_DIR=/run/user/1001 HOME=/home/homelab PATH="$PATH" \
        podman load -i ${testImage}
    '';
  };

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
