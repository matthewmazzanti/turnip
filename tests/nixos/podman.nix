# Hermetic NixOS test for the real container-attach contract (single node + a nix-built
# OCI image). The scenario logic is `test_podman_attach` (tests/integration/), gated by
# the `needs_image` marker; this node loads the image, exports the paths it needs, and
# runs that test as the rootless owner (the router network + podman attach are rootless).
# `nix build .#checks.<sys>.integration-podman`.
{ lib, turnipEnv, image, tconnect }:
let
  # run as the rootless owner with its runtime env (the attach + router net are rootless).
  asHomelab = "sudo -u homelab env XDG_RUNTIME_DIR=/run/user/1001 HOME=/home/homelab";
in
{
  name = "turnip-integration-podman";

  nodes.machine = { ... }: {
    imports = [ ../../nix/turnip-host.nix ];
    virtualisation.memorySize = 3072; # podman image + container
    virtualisation.cores = 2;
    environment.systemPackages = [ turnipEnv ];
    environment.etc."turnip-tests".source = ../integration;
    environment.etc."turnip-run-container.sh".source = ../../run-container.sh;
  };

  testScript = ''
    start_all()
    machine.wait_for_unit("multi-user.target")
    machine.wait_until_succeeds("test -d /run/user/1001")
    machine.succeed("${asHomelab} podman load -i ${image}")  # registry-free load
    machine.succeed(
        "${asHomelab} TURNIP_INTEGRATION=1 TURNIP_TEST_IMAGE=turnip-test:latest "
        "TURNIP_TCONNECT=${tconnect} TURNIP_RUNCONTAINER=/etc/turnip-run-container.sh "
        "PYTHONDONTWRITEBYTECODE=1 pytest -p no:cacheprovider -v -m needs_image /etc/turnip-tests"
    )
  '';
}
