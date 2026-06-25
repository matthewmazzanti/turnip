# nix/vm/default.nix -- every VM this repo builds, grouped by usecase. Two usecases, two roles each.
# Each role has ONE base (host-base / world-base) shared by both its usecases; the interactive
# variant grows from that same base plus ./interactive.nix (the dev carve-outs: 9p, mgmt NIC, dev
# user, qemu sizing) and a couple of role-specific extras.
#
#   test.host        = host-base  + { turnip }
#   test.world       = world-base
#   interactive.host = host-base  + interactive + { go, :2222, test image }
#   interactive.world= world-base + interactive + { :2223, dev tooling }
#
# The bases mirror the runNixOSTest LAN (host 192.168.1.1 / world 192.168.1.2), so the hermetic
# check and the dev VMs run identical network config. mkVM wraps an interactive role module with the
# qemu-vm machinery into a runnable image; the test roles are fed to runNixOSTest, which supplies its
# own VM machinery. The roles close over the turnip package + pkgs from this scope.
#
#   import ./nix/vm { inherit pkgs turnip lib nixpkgs system; }
#     -> { interactive = { host; world; }; test = { host; world; }; }
{ pkgs, turnip, lib, nixpkgs, system }:
let
  # Wrap an inline role module with the qemu-vm machinery into a runnable image
  # (`result/bin/run-*-vm`). The role module pulls its base + carve-outs in via `imports`.
  mkVM = roleModule: (lib.nixosSystem {
    inherit system;
    modules = [ "${nixpkgs}/nixos/modules/virtualisation/qemu-vm.nix" roleModule ];
  }).config.system.build.vm;
in
{
  interactive = lib.mapAttrs (_: mkVM) {
    # The interactive HOST dev VM: the system under test. host-base brings rootless podman, the host
    # toolkit, and the modeled LAN (br-lan / 192.168.1.1); interactive.nix brings the dev substrate.
    # On top: Go (build turnip from the 9p mount) and the podman-attach test image.
    #   just host  (boots qemu; persists vm/host.qcow2; serial console, Ctrl-a x to quit)
    #   nix/ssh-vm.sh [dev|homelab] [cmd]   (host on :2222)
    host = { pkgs, ... }:
      let
        # The container-attach test image: the shared probe image (python3) + the netns inspection
        # CLIs for manual poking inside a container. Loaded into homelab's rootless podman at boot.
        testImage = import ./probe-image.nix {
          inherit pkgs;
          name = "turnip-test";
          extraTools = [ pkgs.iproute2 pkgs.iputils pkgs.nftables ];
        };
      in
      {
        imports = [ ./host-base.nix ./interactive.nix ];

        networking.hostName = "turnip";

        # Go: the dev toolchain (build turnip from the 9p mount). The host toolkit (python3 + the
        # inspection CLIs the harness's in-netns probes exec) comes from host-base.
        environment.systemPackages = [ pkgs.go ];

        # Host-forwarded ssh: host VM on :2222.
        virtualisation.forwardPorts = [
          { from = "host"; host.port = 2222; guest.port = 22; }
        ];

        # Load the test image into homelab's rootless store at boot, and name it in the environment.
        environment.sessionVariables.TURNIP_TEST_IMAGE = "${testImage.imageName}:${testImage.imageTag}";
        systemd.services.turnip-test-image = {
          description = "load the python test OCI image into homelab's rootless podman";
          wantedBy = [ "multi-user.target" ];
          after = [ "user@1001.service" ];
          wants = [ "user@1001.service" ];
          serviceConfig = {
            Type = "oneshot";
            RemainAfterExit = true;
          };
          script = ''
            until test -d /run/user/1001; do sleep 0.2; done
            export PATH=/run/wrappers/bin:/run/current-system/sw/bin
            runuser -u homelab -- env XDG_RUNTIME_DIR=/run/user/1001 HOME=/home/homelab PATH="$PATH" \
              podman load -i ${testImage}
          '';
        };
      };

    # The interactive WORLD dev VM: the external LAN peer for egress/ingress/veth-link exploration.
    # world-base brings peer-echo, sshd, socat, and the static LAN (192.168.1.2); interactive.nix
    # brings the dev substrate. On top: the netns/diagnostic CLIs for poking the LAN.
    #   just world  (boots qemu; persists vm/world.qcow2; serial console)
    #   nix/ssh-vm.sh world [dev] [cmd]   (world on :2223)
    world = { pkgs, ... }: {
      imports = [ ./world-base.nix ./interactive.nix ];

      networking.hostName = "turnip-world";

      # Diagnostic tooling for poking the LAN (socat comes from world-base).
      environment.systemPackages = [ pkgs.python3 pkgs.iproute2 pkgs.iputils ];

      # Host-forwarded ssh: world VM on :2223.
      virtualisation.forwardPorts = [
        { from = "host"; host.port = 2223; guest.port = 22; }
      ];
    };
  };

  # The hermetic-check role configs (checks.integration nodes) -- each just its role base.
  test = {
    # The system under test: host-base (rootless podman + the probe/inspection toolkit + the modeled
    # LAN) + the turnip binary under test.
    host = {
      imports = [ ./host-base.nix ];
      environment.systemPackages = [ turnip ];
    };

    # The external peer: world-base (peer-echo + sshd + socat + the static LAN). No extras.
    world = {
      imports = [ ./world-base.nix ];
    };
  };
}
