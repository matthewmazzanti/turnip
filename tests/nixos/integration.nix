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

  nodes.machine = { pkgs, ... }: {
    imports = [ ../../nix/turnip-host.nix ];
    virtualisation.memorySize = 2048;
    virtualisation.cores = 2;
    # The packaged turnip (uv2nix) -- `turnip` + a `python3` carrying its deps.
    environment.systemPackages = [ turnipEnv ];
    # Probe toolkit + scenarios, baked in (no source mount -- this is hermetic).
    environment.etc."turnip-tests".source = ../integration;

    # Borrowed link anchors, provided declaratively (turnip validates, never creates):
    # an empty host bridge (the veth->bridge segment; STP off so ports forward at once)
    # and a dummy NIC standing in for a movable physical device (the phys move).
    networking.bridges."br-lan".interfaces = [ ];
    systemd.services.turnip-test-anchors = {
      wantedBy = [ "multi-user.target" ];
      after = [ "network.target" ];
      path = [ pkgs.iproute2 ];
      serviceConfig = { Type = "oneshot"; RemainAfterExit = true; };
      script = ''
        ip link add net-phys type dummy || true
        ip link set net-phys up
        ip link set br-lan up
      '';
    };
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

    # container links (veth->bridge / veth->host / phys): the L2 trust escape,
    # outside every router's nft policy.
    machine.wait_until_succeeds("ip link show net-phys")  # anchor service settled
    scenario("scenario_links", "links.json")

    # bad configs must fail fast at validate_link_anchors, before building anything.
    with subtest("negative: anchor / validation rejects"):
        for cfg in ["neg_badbridge.json", "neg_physprimary.json", "neg_coexist.json"]:
            machine.fail(f"TURNIP_CONFIG=/etc/turnip-tests/configs/{cfg} turnip up")
  '';
}
