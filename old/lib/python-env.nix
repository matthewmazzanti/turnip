# uv2nix, in one call.
#
# `mkUvEnv { pkgs }` -> a self-contained venv (bin/turnip, bin/python, site-packages)
# built from uv.lock. This file is the WHOLE uv2nix core -- four steps, nothing else.
# Editable installs, the dev VM, etc. are layers built ON TOP of this, elsewhere.
#
# Bundled by lib/default.nix as `mkUvEnv`, then called per-system:
#   mkUvEnv { inherit pkgs; }                       # runtime deps only
#   mkUvEnv { inherit pkgs; deps = w: w.deps.all; } # + the dev group (pytest, ruff, ...)
#
# Pass `editableRoot` to install turnip EDITABLE against a live source tree (the dev VM's
# 9p mount): deps are baked, but `turnip` itself resolves to that path -- no rebuild on edit.
#   mkUvEnv { inherit pkgs; deps = w: w.deps.all; editableRoot = "/mnt/turnip"; }
{ inputs, workspaceRoot }:
let
  inherit (inputs) nixpkgs pyproject-nix uv2nix pyproject-build-systems;
  lib = nixpkgs.lib;

  # (1) Parse uv.lock + pyproject.toml. System-independent, so do it once. Gives us the
  #     dep selections (deps.default / deps.all) and the overlay constructors below.
  workspace = uv2nix.lib.workspace.loadWorkspace { inherit workspaceRoot; };

  # (2) Lock -> a nixpkgs overlay where every locked package is a buildable attr.
  #     `wheel` = prefer prebuilt wheels over compiling sdists (no rust for pydantic-core).
  overlay = workspace.mkPyprojectOverlay { sourcePreference = "wheel"; };
in
{ pkgs
, python ? pkgs.python314
, name ? "turnip-env"
, deps ? (w: w.deps.default)
, editableRoot ? null  # path-as-string for an editable turnip install; null = baked copy
}:
let
  # When editableRoot is set, two extra overlays turn turnip into an editable install:
  #   - mkEditablePyprojectOverlay: `turnip` gets a .pth pointing at editableRoot/src instead
  #     of a store copy. The path is a string -- it needn't exist at build time, only at runtime.
  #   - the inline overlay: hatchling's editable build imports `editables`, so add it to
  #     turnip's build inputs (the standard uv2nix editable step).
  editableOverlays = lib.optionals (editableRoot != null) [
    (workspace.mkEditablePyprojectOverlay { root = editableRoot; })
    (final: prev: {
      turnip = prev.turnip.overrideAttrs (old: {
        nativeBuildInputs = old.nativeBuildInputs ++ final.resolveBuildSystem { editables = [ ]; };
      });
    })
  ];

  # (3) Assemble the Python package set: pyproject.nix's base scope (seeded with `python`)
  #     + build backends (hatchling et al., NOT in uv.lock) + our locked-deps overlay
  #     (+ the editable overlays, when requested).
  pythonSet =
    (pkgs.callPackage pyproject-nix.build.packages { inherit python; }).overrideScope
      (lib.composeManyExtensions ([
        pyproject-build-systems.overlays.default
        overlay
      ] ++ editableOverlays));
in
# (4) Materialize a venv for the chosen dependency closure.
pythonSet.mkVirtualEnv name (deps workspace)
