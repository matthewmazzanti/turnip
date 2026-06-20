# Shared substrate: "a host that can run turnip" -- rootless podman owned by the run
# user, plus the nft / ip tooling turnip drives. Imported by BOTH the interactive dev
# VM (nix/testvm.nix) and the hermetic NixOS integration tests (tests/nixos/*).
#
# Two REQUIRED options let each consumer supply what differs, while the base does the
# common wiring. Both consumers of this base are test hosts that always set them:
#   turnip.env       -- the turnip venv to put on PATH. The base stays agnostic about WHICH:
#                       the dev VM gives an editable env (live 9p source), the check the
#                       uv2nix-built package.
#   turnip.testImage -- the test OCI image; a boot-time oneshot loads it into homelab's
#                       rootless podman (the dev VM for `just itest`, the check before pytest).
{ config, lib, pkgs, ... }:
{
  options.turnip = {
    env = lib.mkOption {
      type = lib.types.package;
      description = "The turnip venv to put on PATH (editable for the dev VM, packaged for the check).";
    };
    testImage = lib.mkOption {
      type = lib.types.package;
      description = "Integration test OCI image (a dockerTools image), loaded into rootless podman at boot.";
    };
  };

  config = {
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
      config.turnip.env # `turnip` + python3 + pytest (editable or packaged, per consumer)
      pkgs.nftables # `nft` -- turnip shells out to it (nftlib.load)
      pkgs.iproute2 # ip/ss -- probes read `ip -j`
      pkgs.jq # inspect `nft -j`
    ];

    # Test config shared by both consumers (the dev VM + the check): suppress bytecode
    # writes (sources are read-only -- a store path or a ro 9p mount) and name the OCI
    # image this host loads at boot, derived from the image itself so it can't drift.
    # TURNIP_INTEGRATION (un-skip the live scenarios) is each consumer's call, not the base.
    environment.variables = {
      PYTHONDONTWRITEBYTECODE = "1";
      TURNIP_TEST_IMAGE = "${config.turnip.testImage.imageName}:${config.turnip.testImage.imageTag}";
    };

    # Load the test OCI image into homelab's rootless store at boot, so both consumers get
    # the identical image with no per-host loader.
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
          podman load -i ${config.turnip.testImage}
      '';
    };
  };
}
