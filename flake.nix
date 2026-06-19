{
  description = "turnip: a persistent rootless container network for podman";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";

    # uv2nix: build turnip + its locked deps straight from uv.lock (real nix
    # packaging, replacing the old "run the live source" wrapper). pyproject.nix is
    # the lock/metadata layer; build-system-pkgs supplies build backends (hatchling).
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
      forAllSystems = f: nixpkgs.lib.genAttrs systems (system: f system);
      lib = nixpkgs.lib;

      # The uv2nix workspace: parse uv.lock + pyproject.toml at the repo root.
      workspace = uv2nix.lib.workspace.loadWorkspace { workspaceRoot = ./.; };
      # Prefer prebuilt wheels (pydantic-core ships a cp314 manylinux wheel; pyroute2
      # is pure-python) so the build needs no compiler / rust toolchain.
      pyprojectOverlay = workspace.mkPyprojectOverlay { sourcePreference = "wheel"; };

      # Per-system python package set: nixpkgs python314 + the build-systems overlay +
      # our locked-deps overlay.
      pythonSetFor = system:
        let
          pkgs = nixpkgs.legacyPackages.${system};
        in
        (pkgs.callPackage pyproject-nix.build.packages { python = pkgs.python314; }).overrideScope
          (lib.composeManyExtensions [
            pyproject-build-systems.overlays.default
            pyprojectOverlay
          ]);

      # A self-contained venv (bin/turnip, bin/python, ...) for the chosen dep groups.
      # `default` = runtime only; `all` = + the dev group (pytest etc.) for tests.
      mkEnv = system: deps: (pythonSetFor system).mkVirtualEnv "turnip-env" deps;
    in
    {
      packages = forAllSystems (system:
        let
          # The turnip dev VM: a NixOS system importing the qemu-vm module (so
          # config.system.build.vm exists) plus testvm.nix. It deliberately runs the
          # LIVE 9p-mounted source (no rebuild on edit) -- the hermetic packaged env
          # is for the integration tests, not the interactive dev box.
          testVM = nixpkgs.lib.nixosSystem {
            inherit system;
            modules = [
              "${nixpkgs}/nixos/modules/virtualisation/qemu-vm.nix"
              ./nix/testvm.nix
            ];
          };
        in
        {
          # `nix build .#vm` -> result/bin/run-turnip-vm
          vm = testVM.config.system.build.vm;
          # `nix build .#turnip` -> a venv with the `turnip` console script.
          turnip = mkEnv system workspace.deps.default;
          turnip-test = mkEnv system workspace.deps.all;
          default = self.packages.${system}.turnip;
        });

      # NixOS integration tests (hermetic, CI-able): `nix build .#checks.<sys>.<name>`.
      checks = forAllSystems (system:
        let
          pkgs = nixpkgs.legacyPackages.${system};
          # a tiny "connect to argv[1]:argv[2], exit 0/1" baked into the test image.
          tconnect = pkgs.writeScriptBin "tconnect" ''
            #!${pkgs.python3Minimal}/bin/python3
            import socket, sys
            socket.create_connection((sys.argv[1], int(sys.argv[2])), timeout=3).close()
          '';
        in
        {
          integration = pkgs.testers.runNixOSTest (import ./tests/nixos/integration.nix {
            inherit lib;
            turnipEnv = self.packages.${system}.turnip;
          });
          integration-uplink = pkgs.testers.runNixOSTest (import ./tests/nixos/uplink.nix {
            inherit lib;
            turnipEnv = self.packages.${system}.turnip;
          });
          integration-podman = pkgs.testers.runNixOSTest (import ./tests/nixos/podman.nix {
            inherit lib;
            turnipEnv = self.packages.${system}.turnip;
            # a registry-free OCI image (python3 only) + a tiny connect-by-host:port
            # script inside it, referenced by absolute path to dodge shell quoting.
            image = pkgs.dockerTools.buildLayeredImage {
              name = "turnip-test";
              tag = "latest";
              contents = [ pkgs.python3Minimal tconnect ];
            };
            tconnect = "${tconnect}/bin/tconnect";
          });
        });

      devShells = forAllSystems (system:
        let
          pkgs = nixpkgs.legacyPackages.${system};
          python = pkgs.python314;
        in
        {
          default = pkgs.mkShell {
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
        });
    };
}
