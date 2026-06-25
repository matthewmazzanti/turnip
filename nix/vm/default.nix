# nix/vm/default.nix -- every VM this repo builds, grouped by usecase. Two usecases, two roles each:
#
#   interactive.{host,world}  the standalone dev VMs, booted by `just host` / `just world` and
#                             driven over ssh. Each is an inline role module (its shared building
#                             blocks pulled in via `imports`) wrapped with the qemu-vm machinery
#                             (mkVM) into a runnable image -- a built VM derivation.
#   test.{host,world}         the hermetic-check role configs, fed to runNixOSTest (which supplies
#                             its own VM machinery, so these carry only the role config -- no dev
#                             substrate).
#
# Shared building blocks (base-vm = rootless-podman capability, base-interactive = dev substrate,
# peer-echo, probe-image) live in sibling files, imported by the role definitions below. The roles
# close over the turnip package + pkgs from this scope; mkVM needs lib/nixpkgs/system to wrap a role
# module into an image.
#
#   import ./nix/vm { inherit pkgs turnip lib nixpkgs system; }
#     -> { interactive = { host; world; }; test = { host; world; }; }
{ pkgs, turnip, lib, nixpkgs, system }:
let
  # Wrap an inline role module with the qemu-vm machinery into a runnable image
  # (`result/bin/run-*-vm`). The role module pulls its shared building blocks in via `imports`.
  mkVM = roleModule: (lib.nixosSystem {
    inherit system;
    modules = [ "${nixpkgs}/nixos/modules/virtualisation/qemu-vm.nix" roleModule ];
  }).config.system.build.vm;
in
{
  # mkVM mapped over the inline role modules (each pulls its shared building blocks in via imports).
  interactive = lib.mapAttrs (_: mkVM) {
    # The interactive HOST dev VM: the system under test. Imports the capability base (rootless
    # podman) + the dev substrate, then adds the dev toolchain, the podman-attach test image, and
    # the modeled LAN -- a single data interface (eth1) enslaved to a bridge (br-lan) that carries
    # the IPs, mirroring a bridged server. A container's veth-bridge link enslaves into br-lan and
    # appears as a new IP on that LAN.
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
        imports = [ ./base-vm.nix ./base-interactive.nix ];

        networking.hostName = "turnip";
        # Resolve the world peer by name on the LAN (the integration harness dials `world`).
        networking.extraHosts = "192.168.50.20 world";

        # Go + python3: the dev toolchain, and python3 on the host PATH is what the integration
        # harness's in-netns connect probes exec (via `turnip probe`) -- matching the check's host.
        environment.systemPackages = [ pkgs.go pkgs.python3 ];

        # homelab (the rootless-podman owner, declared by base-vm) gets its VM login creds here.
        users.users.homelab = {
          password = "homelab";
          openssh.authorizedKeys.keyFiles = [ ../testvm_key.pub ];
        };

        # The throwaway test key, baked in for the integration harness to ssh the world peer -- no
        # manual key staging. Root-owned 0600 so ssh accepts it when the harness runs as root.
        environment.etc."turnip/ssh-key" = {
          source = ../testvm_key;
          mode = "0600";
        };

        # The modeled LAN: eth1 (the qemu mcast NIC, wired at launch) is enslaved to br-lan, which
        # holds the addresses. Two IPs -- the second is the anchor for a yet-to-be-added secondary
        # forward rule. ConfigureWithoutCarrier brings the bridge up before a peer appears.
        systemd.network.netdevs."20-br-lan".netdevConfig = {
          Name = "br-lan";
          Kind = "bridge";
        };
        systemd.network.networks."21-eth1-lan" = {
          matchConfig.Name = "eth1";
          networkConfig.Bridge = "br-lan";
        };
        systemd.network.networks."22-br-lan" = {
          matchConfig.Name = "br-lan";
          address = [ "192.168.50.10/24" "192.168.50.11/24" ];
          networkConfig.ConfigureWithoutCarrier = true;
          linkConfig.RequiredForOnline = "no";
        };

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

    # The interactive WORLD dev VM: the external LAN peer for egress/ingress/veth-link exploration
    # -- no podman, no turnip (no base-vm); a dumb peer. A static address on the shared LAN (eth1)
    # puts it alongside the host's br-lan; peer-echo (imported) is the egress masquerade observer.
    #   just world  (boots qemu; persists vm/world.qcow2; serial console)
    #   nix/ssh-vm.sh world [dev] [cmd]   (world on :2223)
    world = { pkgs, ... }: {
      imports = [ ./base-interactive.nix ./peer-echo.nix ];

      networking.hostName = "turnip-world";
      # Resolve the host by name on the LAN (symmetry / convenience).
      networking.extraHosts = "192.168.50.10 host";

      # Peer tooling: socat (the peer-echo listener) + the netns/diagnostic CLIs for poking the LAN.
      environment.systemPackages = [ pkgs.socat pkgs.python3 pkgs.iproute2 pkgs.iputils ];

      # The shared LAN: a static address on eth1 (the qemu mcast NIC), alongside the host's br-lan
      # (192.168.50.10/.11). Reachable from a host container's veth-bridge link too.
      systemd.network.networks."21-eth1-lan" = {
        matchConfig.Name = "eth1";
        address = [ "192.168.50.20/24" ];
        networkConfig.ConfigureWithoutCarrier = true;
        linkConfig.RequiredForOnline = "no";
      };

      # Host-forwarded ssh: world VM on :2223.
      virtualisation.forwardPorts = [
        { from = "host"; host.port = 2223; guest.port = 22; }
      ];
    };
  };

  # The hermetic-check role configs (checks.integration nodes).
  test = {
    # The system under test: the rootless-podman capability base + turnip + the probe toolbox
    # (python3 drives the in-netns TCP connect; ip/nft from the base).
    host = {
      imports = [ ./base-vm.nix ];
      environment.systemPackages = [ turnip pkgs.python3 pkgs.iputils pkgs.openssh ];
    };

    # The external peer: sshd (control) + the shared peer-echo listener on :8443 (the egress
    # target). Firewall off so the masqueraded egress connection lands.
    world = {
      imports = [ ./peer-echo.nix ];
      networking.firewall.enable = false;
      environment.systemPackages = [ pkgs.socat ]; # the ingress client (L4 IN-1/IN-3)
      services.openssh.enable = true;
      services.openssh.settings.PermitRootLogin = "prohibit-password";
      users.users.root.openssh.authorizedKeys.keyFiles = [ ../testvm_key.pub ];
    };
  };
}
