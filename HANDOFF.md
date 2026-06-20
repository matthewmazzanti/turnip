# Handoff — branch `editable-dev-vm`

WIP. Builds are green; the dev-VM **runtime** path is not yet validated. (Delete this
file before merging to `dev`.)

## What this branch does
1. **Minimal OCI test image** — `flake.nix` `testImageFor` is now just `python3Minimal`
   (PATH set so bare `python3` resolves). Dropped the baked `tconnect` script.
2. **Bootstrap the connect in the test** — `test_podman_attach` runs `python3 -c
   <_CONNECT>` in the container via `harness.connect_argv(...)` (renamed from
   `_connect_argv`, now public), instead of a baked binary. `TURNIP_TCONNECT` removed.
3. **Dev VM → editable uv2nix env** — `flake.nix` `editableEnvFor` builds turnip
   *editable* against `/mnt/turnip` (via `mkEditablePyprojectOverlay` + the `editables`
   build-system step). `nix/testvm.nix` drops the nixpkgs `pyEnv` + the live-source
   `turnip` wrapper; `turnip`/`python3`/`pytest` are now that env (live source, uv.lock
   deps). `PYTHONDONTWRITEBYTECODE=1` set (ro 9p mount).
4. **Install the image into the dev VM** — `nix/testvm.nix` bakes `testImage` + a
   `turnip-test-image` boot service that `podman load`s it into homelab's rootless store;
   `TURNIP_TEST_IMAGE` + `TURNIP_RUNCONTAINER` exported so `just itest` runs the full
   suite (podman-attach included).

## State
- **Green:** `nix build .#vm` and `nix build .#checks.<sys>.integration` both build; the
  integration check (minimal image + bootstrapped connect) **passes**. Host `uv run
  pytest` + ruff + pyright clean.
- **NOT yet validated (the reason this is a branch):** the dev-VM runtime — that the
  editable `turnip` resolves `/mnt/turnip/src`, the image-load service is green, and
  `just itest` runs the full suite. Last boot: the load service failed on `newuidmap not
  in PATH`; fixed by giving it `PATH=/run/wrappers/bin:/run/current-system/sw/bin`
  (committed), but not re-run.

## To resume
1. `nix build .#vm` → boot (`just vm`, or `just vm-fresh` for a clean disk).
2. `nix/ssh-vm.sh dev 'systemctl status turnip-test-image'` → expect green; `nix/ssh-vm.sh
   homelab 'podman images'` shows `turnip-test:latest`.
3. `just itest` → expect the full suite (incl `test_podman_attach`) green. That run also
   confirms the editable env resolved the live source.

## Risks if it doesn't come up clean
- `turnip` → `ModuleNotFoundError`: the editable `.pth` resolved wrong (root vs `src`
  layout) — adjust `editableEnvFor`'s `mkEditablePyprojectOverlay { root = ... }`.
- Load service flakes on a fresh disk: first-boot race on the rootless runtime dir; the
  `until test -d /run/user/1001` guard should cover it — a `systemctl restart
  turnip-test-image` confirms it's just timing.

## Files
`flake.nix`, `nix/testvm.nix`, `tests/integration/{harness.py,test_integration.py}`,
`tests/nixos/integration.nix`, `.gitignore`.
