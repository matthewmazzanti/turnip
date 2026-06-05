{
  description = "Python project with uv";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
  };

  outputs = { nixpkgs, ... }:
    let
      systems = [ "x86_64-linux" "aarch64-linux" "x86_64-darwin" "aarch64-darwin" ];
      forAllSystems = f: nixpkgs.lib.genAttrs systems (system: f system);
    in
    {
      devShells = forAllSystems (system:
        let
          pkgs = nixpkgs.legacyPackages.${system};
          python = pkgs.python314;
        in {
          default = pkgs.mkShell {
            packages = [
              python
              pkgs.uv
            ];

            env = {
              UV_PYTHON_DOWNLOADS = "never";
              UV_PYTHON = "${python}/bin/python";
            };
          };
        });
    };
}
