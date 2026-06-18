# NixOS dev VM for turnip -- the rootful test sandbox.
#
# A persistent qemu VM (boots under KVM) with the turnip repo LIVE-MOUNTED (9p)
# at /mnt/turnip, so host edits run inside with no rebuild. It carries the real
# runtime: rootless podman owned by `homelab` (linger + auto subuid/subgid), the
# nft binary, and turnip's Python deps -- so `turnip up`/`down` runs for real.
#
# Build + run:
#   nix build .#vm
#   ./result/bin/run-turnip-vm          # boots qemu; persists ./turnip.qcow2
#
# Drive it:
#   ssh -i nix/testvm_key -p 2222 dev@localhost        # admin (sudo)
#   ssh -i nix/testvm_key -p 2222 homelab@localhost    # the rootless run user
#   homelab@turnip$ turnip up        # runs /mnt/turnip/src against turnip.example.json
#
# The disk (turnip.qcow2) is persistent across reboots; `rm` it (or `qemu-img
# snapshot`) for a clean slate.
#
# NOTE: turnip.example.json declares an uplink (an M4 config). Until M4 lands the
# mechanism ignores uplink/egress/ingress, so `turnip up` here exercises the
# rootless baseline (netns + /32 veths + the `inet turnip` flow matrix) and skips
# the host edge. That's the intended first smoke; the host-edge bits light up with
# M4 (and will then want the CAP_NET_ADMIN / sudo paths we designed).
{ pkgs, ... }:
let
  # turnip's runtime deps from nixpkgs -- no uv / network needed in the VM. We run
  # the live-mounted src/ directly rather than installing the package.
  pyEnv = pkgs.python314.withPackages (ps: [
    ps.pydantic
    ps.pyroute2
    ps.pytest # run the suite inside the VM against the live-mounted source
  ]);

  # `turnip` on PATH: run the live source as the package, defaulting the config to
  # the bundled example (override with TURNIP_CONFIG). Config path is absolute, so
  # cwd doesn't matter; turnip writes its state under $XDG_RUNTIME_DIR/turnip.
  turnip = pkgs.writeShellScriptBin "turnip" ''
    export PYTHONPATH=/mnt/turnip/src''${PYTHONPATH:+:$PYTHONPATH}
    export TURNIP_CONFIG=''${TURNIP_CONFIG:-/mnt/turnip/turnip.example.json}
    export PYTHONDONTWRITEBYTECODE=1 # don't litter __pycache__ into the 9p-mounted repo
    exec ${pyEnv}/bin/python -m turnip.main "$@"
  '';
in
{
  system.stateVersion = "25.05";
  networking.hostName = "turnip";
  # Don't let the host firewall interfere with turnip's netns/nft experiments.
  networking.firewall.enable = false;

  # Rootless podman, owned by `homelab` (the run target from turnip.example.json).
  virtualisation.podman.enable = true;

  # Admin user for console/ssh debugging.
  users.users.dev = {
    isNormalUser = true;
    extraGroups = [ "wheel" ];
    password = "dev"; # throwaway VM: console login dev/dev
    openssh.authorizedKeys.keyFiles = [ ./testvm_key.pub ];
  };

  # The rootless podman owner. linger => /run/user/<uid> + the pause process exist
  # with no active login; autoSubUidGidRange => the subuid/subgid range podman maps.
  users.users.homelab = {
    isNormalUser = true;
    linger = true;
    autoSubUidGidRange = true;
    password = "homelab";
    openssh.authorizedKeys.keyFiles = [ ./testvm_key.pub ];
  };

  security.sudo.wheelNeedsPassword = false;
  services.openssh.enable = true;

  environment.systemPackages = [
    pkgs.nftables # `nft` -- turnip shells out to it (nftlib.load)
    pkgs.iproute2 # ip/ss for debugging
    pkgs.jq # inspect `nft -j` output
    pyEnv # `python` with turnip's deps, for ad-hoc poking
    turnip # the `turnip` wrapper above
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
    # because the NixOS run script `cd`s into a temp dir before exec'ing qemu: a
    # baked relative "." would resolve there (you'd see its `xchg`, not the repo),
    # and an absolute path would hardcode one checkout. Injecting $PWD from the repo
    # root at launch is both correct and checkout-independent.
    #   - ro: the repo is pure source; the VM never writes to it (state goes under
    #     $XDG_RUNTIME_DIR). 9p can't *filter* (.venv/__pycache__ are visible) but
    #     they're inert -- the `turnip` wrapper uses the VM's pyEnv, never .venv.
    #   - nofail: boot still succeeds if the share isn't passed (bare run-turnip-vm).
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
