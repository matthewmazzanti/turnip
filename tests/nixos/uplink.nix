# Hermetic NixOS integration test for turnip's host edge + LAN links -- the parts that
# need a real off-box peer. Two nodes (`host` runs turnip, `world` is a plain peer)
# share two VLANs (eth1=192.168.1.0/24, eth2=192.168.2.0/24). `world` runs a persistent
# listener on :8888. `nix build .#checks.<sys>.integration-uplink`.
{ lib, turnipEnv }:
let
  addr = a: [ { address = a; prefixLength = 24; } ];
in
{
  name = "turnip-integration-uplink";

  nodes.host = { ... }: {
    imports = [ ../../nix/turnip-host.nix ];
    virtualisation.memorySize = 2048;
    virtualisation.cores = 2;
    virtualisation.vlans = [ 1 2 ];
    networking.interfaces.eth1.ipv4.addresses = addr "192.168.1.1";
    networking.interfaces.eth2.ipv4.addresses = addr "192.168.2.1";
    environment.systemPackages = [ turnipEnv ];
    environment.etc."turnip-tests".source = ../integration;
  };

  nodes.world = { pkgs, ... }: {
    virtualisation.vlans = [ 1 2 ];
    networking.firewall.enable = false;
    networking.interfaces.eth1.ipv4.addresses = addr "192.168.1.2";
    networking.interfaces.eth2.ipv4.addresses = addr "192.168.2.2";
    environment.systemPackages = [ turnipEnv pkgs.iproute2 ];
    environment.etc."turnip-tests".source = ../integration;
    # A persistent TCP listener on :8888 (all addresses) -- the egress + LAN-link target.
    systemd.services.world-listener = {
      wantedBy = [ "multi-user.target" ];
      after = [ "network.target" ];
      serviceConfig.ExecStart = "${turnipEnv}/bin/python /etc/turnip-tests/_serve.py 8888";
    };
  };

  testScript = ''
    start_all()
    host.wait_for_unit("multi-user.target")
    world.wait_for_unit("multi-user.target")
    host.wait_until_succeeds("test -d /run/user/1001")
    world.wait_until_succeeds("systemctl is-active world-listener.service")
    # the LAN is up + the listener is reachable from the host before turnip runs
    host.wait_until_succeeds("python3 /etc/turnip-tests/_connect.py 192.168.1.2 8888 2")

    # the needs_world scenarios (uplink egress + macvlan/ipvlan LAN reachability) live in
    # the pytest registry; run them on the host (world is the off-box peer they reach).
    host.succeed(
        "TURNIP_INTEGRATION=1 TURNIP_WORLD=1 PYTHONDONTWRITEBYTECODE=1 "
        "pytest -p no:cacheprovider -v -m needs_world /etc/turnip-tests"
    )

    # ingress DNAT: world -> host:8080 -> svc:80. The svc-side listener lives in the
    # container netns (a transient unit so it outlives the start command); world is the
    # external client.
    with subtest("uplink ingress DNAT (world -> host:8080 -> svc:80)"):
        host.succeed("TURNIP_CONFIG=/etc/turnip-tests/configs/uplink.json turnip up")
        # background a listener in svc's netns. NOT systemd-run: a transient unit gets a
        # minimal PATH (no sudo wrapper); a backgrounded machine.succeed shell has the
        # full login PATH. _serve.py self-expires, so a stray child can't leak.
        host.succeed(
            "/run/wrappers/bin/sudo -u homelab env XDG_RUNTIME_DIR=/run/user/1001 "
            "HOME=/home/homelab podman unshare nsenter "
            "--net=/run/user/1001/turnip/containers/svc/netns "
            "python3 /etc/turnip-tests/_serve.py 80 30 >/dev/null 2>&1 &"
        )
        host.sleep(2)
        world.succeed("python3 /etc/turnip-tests/_connect.py 192.168.1.1 8080 3")  # DNAT lands
        world.fail("python3 /etc/turnip-tests/_connect.py 192.168.1.1 9999 2")  # unpublished
        host.succeed("TURNIP_CONFIG=/etc/turnip-tests/configs/uplink.json turnip down")
  '';
}
