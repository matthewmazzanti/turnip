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

          # turnip installed EDITABLE against the live 9p source at /mnt/turnip: deps + dev
          # tools are baked, but `turnip` itself resolves to the live mount (no rebuild on edit).
          editableEnv = mkUvEnv {
            inherit pkgs;
            name = "turnip-dev";
            deps = w: w.deps.all;
            editableRoot = "/mnt/turnip";
          };

          # The dev VM: a NixOS system (qemu-vm module + testvm.nix) running the editable env.
          # The test OCI image comes from the shared base (nix/turnip-host.nix), not threaded here.
          testVM = lib.nixosSystem {
            inherit system;
            specialArgs = { turnipEnv = editableEnv; };
            modules = [
              "${nixpkgs}/nixos/modules/virtualisation/qemu-vm.nix"
              ./nix/testvm.nix
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
          checks = {
            # one gate: a single NixOS host runs the whole pytest suite (the `world` peer is
            # an in-host netns fixture, so no multi-node test is needed).
            integration = pkgs.testers.runNixOSTest (import ./tests/nixos/integration.nix {
              inherit lib;
              turnipEnv = self.packages.${system}.turnip-test;
            });
          };

          devShells.default = pkgs.mkShell {
            packages = [
              python
              pkgs.uv
              pkgs.just # task runner (see ./justfile)
              self.packages.${system}.vm # the dev VM: `run-turnip-vm`
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
