# Hermetic NixOS integration test (single node): builds known networks with the packaged
# turnip and asserts their observable properties with the black-box probes. The scenario
# LOGIC lives in pytest (tests/integration/, the registry in scenarios.py); this node
# does only environment setup, then runs `pytest`. `nix build .#checks.<sys>.integration`.
#
# `turnipEnv` is the uv2nix `turnip-test` env (provides both `turnip` and `pytest`). Link
# anchors are created by the test's `ensure_anchors` fixture, not declared here -- so the
# single-node scenarios are self-contained in pytest. The needs_world / needs_image
# scenarios self-skip (their env isn't set), so a plain `pytest` runs router/links/negatives.
{ lib, turnipEnv }:
{
  name = "turnip-integration";

  nodes.machine = { ... }: {
    imports = [ ../../nix/turnip-host.nix ];
    virtualisation.memorySize = 2048;
    virtualisation.cores = 2;
    environment.systemPackages = [ turnipEnv ];
    environment.etc."turnip-tests".source = ../integration;
  };

  testScript = ''
    start_all()
    machine.wait_for_unit("multi-user.target")
    machine.wait_until_succeeds("test -d /run/user/1001")  # rootless runtime dir up
    machine.succeed(
        "TURNIP_INTEGRATION=1 PYTHONDONTWRITEBYTECODE=1 "
        "pytest -p no:cacheprovider -v /etc/turnip-tests"
    )
  '';
}
