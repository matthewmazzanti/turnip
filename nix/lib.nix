# mkOutputs: a small flake-output helper.
#
# Turnip's flake outputs split cleanly in two:
#   - system-INDEPENDENT ones (nixosConfigurations, overlays, nixosModules, ...) that
#     live at the top level verbatim, and
#   - system-DEPENDENT ones (packages, checks, devShells, ...) that the flake output
#     schema requires to be keyed by `system`: packages.<system>.<name>.
#
# Writing the per-system ones by hand means wrapping every category in `forAllSystems`
# and repeating `pkgs = nixpkgs.legacyPackages.${system}` everywhere. `mkOutputs` lets
# you instead describe one system's outputs once; it replicates + transposes them into
# the schema shape, threading in `system` and the matching `pkgs`.
#
#   mkOutputs { nonSystem ? { }, perSystem }
#     nonSystem :: attrs                      -- merged into the result verbatim
#                                                (optional; defaults to no top-level outputs)
#     perSystem :: { system, pkgs } -> attrs  -- evaluated once per system, then
#                                                transposed so each output category is
#                                                keyed by system.
#
# Example:
#   mkOutputs {
#     nonSystem.nixosConfigurations.foo = ...;
#     perSystem = { system, pkgs }: {
#       packages.default = pkgs.hello;
#       devShells.default = pkgs.mkShell { };
#     };
#   }
#   => {
#        nixosConfigurations.foo  = ...;            # verbatim
#        packages.<system>.default  = ...;          # transposed per system
#        devShells.<system>.default = ...;
#      }
{ lib, nixpkgs, systems }:
let
  forAllSystems = lib.genAttrs systems;
in
{ nonSystem ? { }, perSystem }:
let
  # perSystem evaluated for every supported system:
  #   bySystem :: { <system> = { <category> = { <name> = drv; }; }; }
  bySystem = forAllSystems (system:
    perSystem {
      inherit system;
      pkgs = nixpkgs.legacyPackages.${system};
    });

  # The output categories produced (packages, checks, devShells, ...). perSystem is the
  # same function for every system, so its key set is uniform across `bySystem`; the
  # union is merely defensive.
  categories = lib.unique (lib.concatMap lib.attrNames (lib.attrValues bySystem));

  # Transpose { <system> -> <category> -> v } into { <category> -> <system> -> v } --
  # the shape the flake output schema wants.
  perSystemOutputs = lib.genAttrs categories (category:
    forAllSystems (system: bySystem.${system}.${category}));
in
# recursiveUpdate (not //) so a system-independent output can still slot a sibling into
# a per-system category tree if ever needed; in practice the two halves are disjoint.
lib.recursiveUpdate nonSystem perSystemOutputs
