# turnip's in-repo Nix lib: the reusable flake helpers, bundled behind one import.
#
# Wire it up once with the flake inputs, then `inherit` the pieces you need:
#
#   turnipLib = import ./lib { inherit inputs; };
#   inherit (turnipLib) mkOutputs mkUvEnv;
#
# Components:
#   mkOutputs -- describe one system's flake outputs once; transpose to the schema. (./outputs.nix)
#                Takes its own `systems` arg at the call site.
#   mkUvEnv   -- uv2nix in one call: a venv from uv.lock for the given pkgs.         (./python-env.nix)
#
# `workspaceRoot` defaults to ../. -- i.e. the repo root, since this file sits at lib/ --
# which is where uv.lock + pyproject.toml live. Override it if the workspace moves.
{ inputs, workspaceRoot ? ../. }:
let
  nixpkgs = inputs.nixpkgs;
  lib = nixpkgs.lib;
in
{
  mkOutputs = import ./outputs.nix { inherit lib nixpkgs; };
  mkUvEnv = import ./python-env.nix { inherit inputs workspaceRoot; };
}
