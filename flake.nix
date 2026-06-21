{
  description = "turnip: a persistent rootless container network for podman";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
  };

  # Outputs rebuilt on ./nix/lib (mkOutputs): the dev VM + the Go devShell. The Python
  # implementation -- and its uv2nix packaging + hermetic pytest integration check -- is
  # parked under ./old during the Go rewrite; re-add a Go package + integration check here
  # once the port has them.
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
            vendorHash = "sha256-8PyDTxkco+b7r1/+JY8gC+gdfc/VzQwi49jqqcM67Q4=";
            subPackages = [ "cmd/turnip" ];
            meta.mainProgram = "turnip";
          };
        in
        {
          packages = {
            inherit turnip;
            default = turnip; # `nix build` -> the turnip binary
            vm = testVM.config.system.build.vm; # `nix build .#vm` -> result/bin/run-turnip-vm
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
