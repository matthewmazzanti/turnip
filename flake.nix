{
  description = "Python project with uv";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
  };

  outputs = { self, nixpkgs, ... }:
    let
      # turnip is inherently Linux-only (netns / nftables / user namespaces), so
      # there is no darwin support -- not even a devShell.
      systems = [ "x86_64-linux" "aarch64-linux" ];
      forAllSystems = f: nixpkgs.lib.genAttrs systems (system: f system);
    in
    {
      # `nix build .#vm` -> result/bin/run-turnip-vm (resolves to the host arch)
      packages = forAllSystems (system: let 
        # The turnip dev VM: a NixOS system importing the qemu-vm module (so
        # config.system.build.vm exists) plus our testvm.nix. Per-system, so
        # each Linux arch builds + runs its own VM; build.vm exposes
        # `run-turnipvm-vm`.
        testVM = nixpkgs.lib.nixosSystem {
          inherit system;
          modules = [
            "${nixpkgs}/nixos/modules/virtualisation/qemu-vm.nix"
            ./nix/testvm.nix
          ];
        };
      in {
        vm = testVM.config.system.build.vm;
      });

      devShells = forAllSystems (system:
        let
          pkgs = nixpkgs.legacyPackages.${system};
          python = pkgs.python314;
        in {
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
