{
  description = "turnip: a persistent rootless container network for podman";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";

    # uv2nix: build turnip + its locked deps straight from uv.lock -> the packaged env.
    # pyproject.nix is the provider-agnostic engine (metadata -> derivation, venv machinery);
    # uv2nix is the uv frontend (reads uv.lock); build-system-pkgs supplies build backends
    # (hatchling), which uv.lock does NOT lock.
    pyproject-nix = {
      url = "github:pyproject-nix/pyproject.nix";
      inputs.nixpkgs.follows = "nixpkgs";
    };
    uv2nix = {
      url = "github:pyproject-nix/uv2nix";
      inputs.pyproject-nix.follows = "pyproject-nix";
      inputs.nixpkgs.follows = "nixpkgs";
    };
    pyproject-build-systems = {
      url = "github:pyproject-nix/build-system-pkgs";
      inputs.pyproject-nix.follows = "pyproject-nix";
      inputs.uv2nix.follows = "uv2nix";
      inputs.nixpkgs.follows = "nixpkgs";
    };
  };

  # Outputs rebuilt on ./lib (mkOutputs + mkUvEnv): the packaged/editable envs, the dev VM,
  # the hermetic integration check, and the devShell.
  outputs = inputs@{ self, nixpkgs, ... }:
    let
      lib = nixpkgs.lib;

      # turnip's in-repo lib (./lib): the bundled flake helpers.
      #   mkOutputs -- one system's outputs, transposed into the flake schema.
      #   mkUvEnv   -- uv2nix in one call: a venv from uv.lock for the given pkgs.
      turnipLib = import ./lib { inherit inputs; };
      inherit (turnipLib) mkOutputs mkUvEnv;
    in
    mkOutputs {
      systems = [ "x86_64-linux" "aarch64-linux" ];
      perSystem = { system, pkgs }:
        let
          python = pkgs.python314;

          # A registry-free OCI image (just python3) for the real container-attach test: the
          # container runs `python3 -c <connect>` (the test supplies the snippet). PATH set so
          # bare `python3` resolves. Loaded into both hosts' rootless podman (dev VM at boot,
          # the check in its testScript).
          testImage = pkgs.dockerTools.buildLayeredImage {
            name = "turnip-test";
            tag = "latest";
            contents = [ pkgs.python3Minimal ];
            config.Env = [ "PATH=${pkgs.python3Minimal}/bin" ];
          };

          # The dev VM, layered explicitly: the qemu-vm machinery, the shared turnip host base,
          # the dev-VM specifics, then this build's customizations (which env + image). The
          # editable env and test OCI image are supplied via the shared turnip.{env,testImage}
          # options -- no specialArgs threading.
          testVM = lib.nixosSystem {
            inherit system;
            modules = [
              "${nixpkgs}/nixos/modules/virtualisation/qemu-vm.nix"
              ./nix/turnip-host.nix # base: rootless podman host + the turnip.* options
              ./nix/testvm.nix # dev VM: 9p mount, ssh/console, login users
              {
                # turnip installed EDITABLE against the live 9p source at /mnt/turnip: deps +
                # dev tools are baked, but `turnip` resolves to the live mount (no rebuild on edit).
                turnip.env = mkUvEnv {
                  inherit pkgs;
                  name = "turnip-dev";
                  deps = w: w.deps.all;
                  editableRoot = "/mnt/turnip";
                };
                turnip.testImage = testImage;
              }
            ];
          };
        in
        {
          packages = {
            # `nix build .#vm` -> result/bin/run-turnip-vm
            vm = testVM.config.system.build.vm;
            # `nix build .#turnip` -> a venv with the `turnip` console script (runtime deps).
            turnip = mkUvEnv { inherit pkgs; };
            # + the dev group (pytest, ruff, pyright).
            turnip-test = mkUvEnv { inherit pkgs; deps = w: w.deps.all; };
            default = self.packages.${system}.turnip;
          };

          # NixOS integration tests (hermetic, CI-able): `nix build .#checks.<sys>.<name>`.
          # one gate: a single NixOS host runs the WHOLE pytest suite. Everything runs on
          # this one machine -- the `world` peer for the uplink + LAN-link scenarios is an
          # in-host netns fixture (tests/integration/conftest.py), so there is no multi-node
          # test to maintain. The node only sets up the environment + runs `pytest`.
          checks.integration = pkgs.testers.runNixOSTest {
            name = "turnip-integration";

            # turnip-test = the uv2nix env carrying both `turnip` and `pytest`. pytest runs
            # as root (the link/uplink scenarios are rootful); the podman-attach test drops
            # to the rootless owner itself for run-container.sh.
            nodes.machine = { ... }: {
              imports = [ ./nix/turnip-host.nix ];
              turnip.env = self.packages.${system}.turnip-test; # turnip + pytest on PATH
              turnip.testImage = testImage; # shared base loads it into rootless podman at boot
              virtualisation.memorySize = 3072; # podman image + container + several netns
              virtualisation.cores = 2;
              environment.etc."turnip-tests".source = ./tests/integration;
              environment.etc."turnip-run-container.sh".source = ./run-container.sh;
            };

            # The shared base loads the OCI image at boot (turnip.testImage on the node); we
            # wait for that unit, then run the suite.
            testScript = ''
              start_all()
              machine.wait_for_unit("multi-user.target")
              machine.wait_until_succeeds("test -d /run/user/1001")  # rootless runtime dir up
              machine.wait_for_unit("turnip-test-image.service")     # image in the rootless store
              machine.succeed(
                  "TURNIP_INTEGRATION=1 TURNIP_TEST_IMAGE=turnip-test:latest "
                  "TURNIP_RUNCONTAINER=/etc/turnip-run-container.sh "
                  "PYTHONDONTWRITEBYTECODE=1 pytest -p no:cacheprovider -v /etc/turnip-tests"
              )
            '';
          };

          devShells.default = pkgs.mkShell {
            packages = [
              python
              pkgs.uv
              pkgs.just # task runner (see ./justfile); `just vm` builds + boots the dev VM
              pkgs.qemu-utils # qemu-img: qcow2 info + snapshot/rollback (savevm)
            ];

            env = {
              UV_PYTHON_DOWNLOADS = "never";
              UV_PYTHON = "${python}/bin/python";
            };
          };
        };
    };
}
