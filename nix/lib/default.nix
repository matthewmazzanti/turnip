# turnip's in-repo Nix lib: the reusable flake helpers, bundled behind one import.
#
# Wire it up once with the flake inputs, then `inherit` the pieces you need:
#
#   turnipLib = import ./nix/lib { inherit inputs; };
#   inherit (turnipLib) mkOutputs;
#
# Components:
#   mkOutputs    -- describe one system's flake outputs once; transpose to the schema. (./outputs.nix)
#                   Takes its own `systems` arg at the call site.
#   mkTurnipLib  -- the layered "wrap turnip for Nix" helpers (turnipWithConfigFile /
#                   turnipWithConfig / turnipService). Per-system (needs pkgs + the turnip pkg), so
#                   it's constructed inside perSystem. (./turnip.nix)
{ inputs }:
let
  nixpkgs = inputs.nixpkgs;
  lib = nixpkgs.lib;
in
{
  mkOutputs = import ./outputs.nix { inherit lib nixpkgs; };
  mkTurnipLib = { pkgs, turnip }: import ./turnip.nix { inherit pkgs turnip; };
}
