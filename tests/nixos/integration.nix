# The single hermetic integration gate: one NixOS host runs the WHOLE pytest suite
# (`nix build .#checks.<sys>.integration`). Everything runs on this one machine -- the
# `world` peer for the uplink + LAN-link scenarios is an in-host netns fixture
# (tests/integration/conftest.py), and link anchors are created by the `anchors` fixture
# -- so there is no multi-node test to maintain. The node only sets up the environment
# (the packaged turnip-test env, the test files, the OCI image for the podman-attach
# test) and runs `pytest`.
#
# `turnipEnv` is the uv2nix `turnip-test` env (both `turnip` and `pytest`). pytest runs
# as root (the link/uplink scenarios are rootful); the podman-attach test drops to the
# rootless owner itself for run-container.sh.
{ lib, turnipEnv, image }:
let
  asHomelab = "sudo -u homelab env XDG_RUNTIME_DIR=/run/user/1001 HOME=/home/homelab";
in
{
  name = "turnip-integration";

  nodes.machine = { ... }: {
    imports = [ ../../nix/turnip-host.nix ];
    virtualisation.memorySize = 3072; # podman image + container + several netns
    virtualisation.cores = 2;
    environment.systemPackages = [ turnipEnv ];
    environment.etc."turnip-tests".source = ../integration;
    environment.etc."turnip-run-container.sh".source = ../../run-container.sh;
  };

  testScript = ''
    start_all()
    machine.wait_for_unit("multi-user.target")
    machine.wait_until_succeeds("test -d /run/user/1001")  # rootless runtime dir up
    machine.succeed("${asHomelab} podman load -i ${image}")  # registry-free, rootless store
    machine.succeed(
        "TURNIP_INTEGRATION=1 TURNIP_TEST_IMAGE=turnip-test:latest "
        "TURNIP_RUNCONTAINER=/etc/turnip-run-container.sh "
        "PYTHONDONTWRITEBYTECODE=1 pytest -p no:cacheprovider -v /etc/turnip-tests"
    )
  '';
}
