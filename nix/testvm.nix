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
#   homelab@turnip$ turnip up        # runs /mnt/turnip/src against turnip.example.json
{ pkgs, ... }:
let
  # turnip's runtime deps from nixpkgs -- no uv / network needed in the VM. We run the
  # live-mounted src/ directly rather than the packaged wheel (that's what the dev VM
  # is FOR; the tests use the uv2nix package instead).
  pyEnv = pkgs.python314.withPackages (ps: [
    ps.pydantic
    ps.pyroute2
    ps.pytest # run the unit suite inside the VM against the live-mounted source
  ]);

  # `turnip` on PATH: run the live source as the package, defaulting the config to the
  # bundled example (override with TURNIP_CONFIG). State goes under $XDG_RUNTIME_DIR.
  turnip = pkgs.writeShellScriptBin "turnip" ''
    export PYTHONPATH=/mnt/turnip/src''${PYTHONPATH:+:$PYTHONPATH}
    export TURNIP_CONFIG=''${TURNIP_CONFIG:-/mnt/turnip/turnip.example.json}
    export PYTHONDONTWRITEBYTECODE=1 # don't litter __pycache__ into the 9p-mounted repo
    exec ${pyEnv}/bin/python -m turnip.main "$@"
  '';
in
{
  imports = [ ./turnip-host.nix ];

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

  environment.systemPackages = [
    pyEnv # `python` with turnip's deps, for ad-hoc poking
    turnip # the live-source `turnip` wrapper above
  ];

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
