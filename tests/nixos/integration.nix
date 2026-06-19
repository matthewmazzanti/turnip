# Hermetic NixOS integration test for turnip: boot a fresh host, build a known
# network with the packaged `turnip`, and assert its observable properties with the
# black-box probes (tests/integration/probe.py) -- run via `nix build .#checks.<sys>.integration`.
#
# The node imports the shared substrate (nix/turnip-host.nix) and installs the
# uv2nix-built turnip env (passed in as `turnipEnv`, which also provides `python3`).
# The probe code + scenario configs are baked in at /etc/turnip-tests.
#
# Each scenario is a config (the network to build) + a hand-authored expectation
# script (the properties it must have). The expectations are external ground truth,
# never derived from turnip's own model -- see tests/integration/probe.py.
{ lib, turnipEnv }:
{
  name = "turnip-integration";

  nodes.machine = { ... }: {
    imports = [ ../../nix/turnip-host.nix ];
    virtualisation.memorySize = 2048;
    virtualisation.cores = 2;
    # The packaged turnip (uv2nix) -- `turnip` + a `python3` carrying its deps.
    environment.systemPackages = [ turnipEnv ];
    # Probe toolkit + scenarios, baked in (no source mount -- this is hermetic).
    environment.etc."turnip-tests".source = ../integration;
  };

  testScript = ''
    start_all()
    machine.wait_for_unit("multi-user.target")
    # rootless podman's runtime dir (homelab lingers) must be up before turnip runs
    machine.wait_until_succeeds("test -d /run/user/1001")

    def scenario(name, config):
        cfg = f"/etc/turnip-tests/configs/{config}"
        with subtest(name):
            machine.succeed(f"TURNIP_CONFIG={cfg} turnip up")
            machine.succeed(
                f"PYTHONPATH=/etc/turnip-tests python3 /etc/turnip-tests/{name}.py"
            )
            machine.succeed(f"TURNIP_CONFIG={cfg} turnip down")

    # routed network + directional flow matrix: addresses, default routes, and
    # allow/deny reachability against live listeners.
    scenario("scenario_router", "router.json")
  '';
}
