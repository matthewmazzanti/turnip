{
  description = "turnip: a persistent rootless container network for podman";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
  };

  # Outputs rebuilt on ./nix/lib (mkOutputs): the turnip binary, the dev VM, and the Go
  # devShell. (A hermetic integration check is still to be re-added for the Go port.)
  outputs = inputs@{ nixpkgs, ... }:
    let
      lib = nixpkgs.lib;

      # turnip's in-repo lib (./nix/lib): mkOutputs -- one system's outputs, transposed
      # into the flake schema.
      turnipLib = import ./nix/lib { inherit inputs; };
      inherit (turnipLib) mkOutputs;
    in
    mkOutputs {
      systems = [ "x86_64-linux" "aarch64-linux" ];
      perSystem = { system, pkgs }:
        let
          # The dev VM, layered explicitly: the qemu-vm machinery, the rootless-podman host
          # base, then the dev-VM specifics (9p mount of this repo, ssh/console, login users,
          # Go toolchain).
          testVM = lib.nixosSystem {
            inherit system;
            modules = [
              "${nixpkgs}/nixos/modules/virtualisation/qemu-vm.nix"
              ./nix/turnip-host.nix # base: rootless podman host + nft/ip tooling
              ./nix/testvm.nix # dev VM: 9p mount, ssh/console, login users, Go
            ];
          };

          # The turnip binary itself: `nix build .#turnip` -> result/bin/turnip.
          # vendorHash = null while the port is stdlib-only; set it once the netlink/nft
          # deps land (nix prints the expected hash on the first mismatch).
          turnip = pkgs.buildGoModule {
            pname = "turnip";
            version = "0.1.0-dev";
            src = ./.;
            vendorHash = "sha256-pGZ0ilQ4TC0aUP6mbWeby6Dj2yqngJeOwOQAmV1c9cg=";
            subPackages = [ "cmd/turnip" ];
            meta.mainProgram = "turnip";
          };

          # The compiled integration test binary (`go test -c`): the harness driver run on the
          # host node by checks.integration. Reuses turnip's src + vendoring; the test package is
          # stdlib-only, so no new module deps (vendorHash unchanged).
          turnipTest = turnip.overrideAttrs (_: {
            pname = "turnip-integration-test";
            doCheck = false;
            buildPhase = ''
              runHook preBuild
              go test -c -o turnip-integration.test ./test/integration
              runHook postBuild
            '';
            installPhase = ''
              runHook preInstall
              install -Dm755 turnip-integration.test $out/bin/turnip-integration.test
              runHook postInstall
            '';
          });

          # The fixture configs (one turnip.json per topology), referenced by -fixtures.
          fixtures = ./test/integration/fixtures;

          # The world peer's egress target: a forking TCP listener on :8443 that replies with the
          # source address it sees (SOCAT_PEERADDR). An egress connect reads this back to verify
          # the source was masqueraded to the host edge (EG-2), not the container's 10.x.
          peerEcho = pkgs.writeShellScript "peer-echo" ''
            exec ${pkgs.socat}/bin/socat TCP-LISTEN:8443,reuseaddr,fork SYSTEM:'printf "%s" "$SOCAT_PEERADDR"'
          '';
        in
        {
          packages = {
            inherit turnip;
            default = turnip; # `nix build` -> the turnip binary
            vm = testVM.config.system.build.vm; # `nix build .#vm` -> result/bin/run-turnip-vm
          };

          # Hermetic two-node integration check: `nix flake check` (or `nix build
          # .#checks.<sys>.integration`). host runs turnip + the harness; world is a dumb SSH
          # peer. Iterate warm via `.driverInteractive`. See docs/TEST-PLAN.md.
          checks.integration = pkgs.testers.runNixOSTest {
            name = "turnip-integration";
            nodes = {
              # The system under test: rootless-podman host (turnip-host base) + turnip + the
              # probe toolbox (python3 drives the in-netns TCP connect; ip/nft from the base).
              host = { ... }: {
                imports = [ ./nix/turnip-host.nix ];
                environment.systemPackages = [ turnip pkgs.python3 pkgs.iputils pkgs.openssh ];
              };
              # The external peer: sshd (control) + a peer-echo listener on :8443 (the egress
              # target). Firewall off so the masqueraded egress connection lands. Reached from host.
              world = { ... }: {
                networking.firewall.enable = false;
                systemd.services.peer-echo = {
                  description = "echo the source address a connecting client presents";
                  wantedBy = [ "multi-user.target" ];
                  serviceConfig = {
                    ExecStart = "${peerEcho}";
                    Restart = "always";
                  };
                };
                services.openssh.enable = true;
                services.openssh.settings.PermitRootLogin = "prohibit-password";
                users.users.root.openssh.authorizedKeys.keyFiles = [ ./nix/testvm_key.pub ];
              };
            };
            testScript = ''
              start_all()
              host.wait_for_unit("multi-user.target")
              world.wait_for_unit("sshd.service")
              world.wait_for_open_port(8443)  # the egress peer-echo target

              # rootless podman owner (homelab, uid 1001) must be live before `turnip up`.
              host.wait_until_succeeds("test -d /run/user/1001", timeout=90)
              host.wait_until_succeeds(
                  "su homelab -c 'XDG_RUNTIME_DIR=/run/user/1001 podman info >/dev/null'",
                  timeout=120)

              # host -> world SSH: the shared committed test key (world authorizes its pubkey).
              host.succeed("install -m600 ${./nix/testvm_key} /root/id")
              host.wait_until_succeeds(
                  "ssh -i /root/id -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null"
                  " -o ConnectTimeout=5 root@world true", timeout=90)

              # -test.parallel overrides the GOMAXPROCS default so the timeout-bound flow subtests
              # actually overlap (the work is subprocess-wait-bound, so a few vCPUs suffice).
              print(host.succeed(
                  "${turnipTest}/bin/turnip-integration.test -test.v -test.parallel 8"
                  " -turnip ${turnip}/bin/turnip"
                  " -fixtures ${fixtures}"
                  " -world root@world -ssh-key /root/id 2>&1"))
            '';
          };

          devShells.default = pkgs.mkShell {
            packages = [
              pkgs.go
              pkgs.gopls
              pkgs.just # task runner (see ./justfile); `just vm` builds + boots the dev VM
              pkgs.qemu-utils # qemu-img: qcow2 info + snapshot/rollback (savevm)
            ];
          };
        };
    };
}
