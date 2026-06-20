{
  description = "turnip: a persistent rootless container network for podman";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";

    # uv2nix: build turnip + its locked deps straight from uv.lock -> the packaged env
    # used by `nix build .#turnip` and the integration test node. (The interactive dev VM
    # still runs the live 9p source.) pyproject.nix is the lock/metadata layer;
    # build-system-pkgs supplies build backends (hatchling).
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

  outputs = { self, nixpkgs, pyproject-nix, uv2nix, pyproject-build-systems, ... }:
    let
      # turnip is inherently Linux-only (netns / nftables / user namespaces), so
      # there is no darwin support -- not even a devShell.
      systems = [ "x86_64-linux" "aarch64-linux" ];
      lib = nixpkgs.lib;

      # mkOutputs nonSystem perSystem: describe one system's outputs once and have them
      # replicated + transposed into the flake schema (packages.<system>.<name>, ...).
      mkOutputs = import ./nix/lib.nix { inherit lib nixpkgs systems; };

      # The uv2nix workspace: parse uv.lock + pyproject.toml at the repo root. The
      # workspace and the wheel-preferring overlay are system-independent, so they live
      # up here; everything downstream that needs a concrete `pkgs` is built inside
      # `perSystem` below, closing over its `system`/`pkgs`.
      workspace = uv2nix.lib.workspace.loadWorkspace { workspaceRoot = ./.; };
      # Prefer prebuilt wheels (pydantic-core ships a cp314 manylinux wheel; pyroute2
      # is pure-python) so the build needs no compiler / rust toolchain.
      pyprojectOverlay = workspace.mkPyprojectOverlay { sourcePreference = "wheel"; };
    in
    # No `nonSystem` outputs yet (the dev VM is exposed per-system via
    # packages.<system>.vm). nixosConfigurations/overlays/etc. would slot in there.
    mkOutputs {
      perSystem = { system, pkgs }:
        let
          python = pkgs.python314;

          # Per-system python package set: nixpkgs python314 + the build-systems overlay +
          # our locked-deps overlay.
          pythonSet =
            (pkgs.callPackage pyproject-nix.build.packages { inherit python; }).overrideScope
              (lib.composeManyExtensions [
                pyproject-build-systems.overlays.default
                pyprojectOverlay
              ]);

          # A self-contained venv (bin/turnip, bin/python, ...) for the chosen dep groups.
          # `default` = runtime only; `all` = + the dev group (pytest etc.) for tests.
          mkEnv = deps: pythonSet.mkVirtualEnv "turnip-env" deps;

          # The dev VM's env: turnip installed EDITABLE against /mnt/turnip (the runtime 9p
          # mount), so `turnip`/`python`/`pytest` run the LIVE source with uv.lock-pinned deps
          # -- one env, no separate wrapper, no nixpkgs/lock dep skew. The editable .pth holds
          # the root as a string, so it needn't exist at build time, only at runtime.
          editableEnv =
            let
              editable = workspace.mkEditablePyprojectOverlay { root = "/mnt/turnip"; };
              # hatchling's editable build imports `editables`, so add it to turnip's build
              # inputs (the standard uv2nix editable step).
              addEditables = final: prev: {
                turnip = prev.turnip.overrideAttrs (old: {
                  nativeBuildInputs = old.nativeBuildInputs ++ final.resolveBuildSystem { editables = [ ]; };
                });
              };
              pset = pythonSet.overrideScope
                (lib.composeManyExtensions [ editable addEditables ]);
            in
            pset.mkVirtualEnv "turnip-dev" workspace.deps.all;

          # The turnip dev VM: a NixOS system importing the qemu-vm module (so
          # config.system.build.vm exists) plus testvm.nix. It runs turnip EDITABLE against
          # the live 9p source (turnipEnv); the test OCI image (for `just itest`'s podman
          # attach) is defined on the shared base, not threaded in here.
          testVM = lib.nixosSystem {
            inherit system;
            specialArgs = {
              turnipEnv = editableEnv;
            };
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
            # `nix build .#turnip` -> a venv with the `turnip` console script.
            turnip = mkEnv workspace.deps.default;
            turnip-test = mkEnv workspace.deps.all;
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
