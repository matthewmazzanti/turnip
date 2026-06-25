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

          # Every VM this repo builds (nix/vm/), grouped by usecase: vms.interactive.{host,world}
          # are the built dev VMs; vms.test.{host,world} are the hermetic-check role configs fed to
          # runNixOSTest below; vms.probeImage is the shared netns-probe OCI image (TestPodmanRun's
          # -image payload, also baked into the dev host). mkVM lives in nix/vm/default.nix, hence
          # lib/nixpkgs/system are threaded in.
          vms = import ./nix/vm { inherit pkgs turnip lib nixpkgs system; };
        in
        {
          packages = {
            inherit turnip;
            default = turnip; # `nix build` -> the turnip binary
            host = vms.interactive.host; # `nix build .#host` -> result/bin/run-turnip-vm
            world = vms.interactive.world; # `nix build .#world` -> result/bin/run-turnip-world-vm
          };

          # Hermetic two-node integration check: `nix flake check` (or `nix build
          # .#checks.<sys>.integration`). host runs turnip + the harness; world is a dumb SSH
          # peer. Iterate warm via `.driverInteractive`. See docs/TEST-PLAN.md.
          checks.integration = pkgs.testers.runNixOSTest {
            name = "turnip-integration";
            nodes = {
              # The system under test + the external peer; both from nix/vm/ (vms.test). See
              # docs/TEST-PLAN.md for the topology and what each role carries.
              host = vms.test.host;
              world = vms.test.world;
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

              # host -> world SSH via the baked-in key (base-vm: /etc/turnip/ssh-key; world
              # authorizes its pubkey) -- no manual key staging.
              host.wait_until_succeeds(
                  "ssh -i /etc/turnip/ssh-key -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null"
                  " -o ConnectTimeout=5 root@world true", timeout=90)

              # -test.parallel overrides the GOMAXPROCS default so the timeout-bound flow subtests
              # actually overlap (the work is subprocess-wait-bound, so a few vCPUs suffice).
              print(host.succeed(
                  "${turnipTest}/bin/turnip-integration.test -test.v -test.parallel 8"
                  " -turnip ${turnip}/bin/turnip"
                  " -fixtures ${fixtures}"
                  " -image /etc/turnip/probe-image.tar.gz"  # the OCI archive TestPodmanRun loads + runs
                  " -world root@world -ssh-key /etc/turnip/ssh-key 2>&1"))
            '';
          };

          devShells.default = pkgs.mkShell {
            packages = [
              pkgs.go
              pkgs.gopls
              pkgs.just # task runner (see ./justfile); `just host` / `just world` boot the dev VMs
              pkgs.qemu-utils # qemu-img: qcow2 info + snapshot/rollback (savevm)
              pkgs.python3Packages.qemu-qmp # `qmp-shell vm/<role>.sock` -- interactive QMP client
            ];
          };
        };
    };
}
