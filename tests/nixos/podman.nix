# Hermetic NixOS integration test for the REAL container-attach contract: a podman
# container joins one of turnip's netns via run-container.sh (--network ns:<path> + the
# generated /etc/hosts bind-mount), proving the join inherits the netns address/routes
# AND that turnip's generated hosts file resolves peers by name. Policy still applies.
# `nix build .#checks.<sys>.integration-podman`.
#
# `image` is a nix-built OCI tarball (no registry pull); `tconnect` is an absolute path
# into it -- a tiny "connect to argv[1]:argv[2]" script (avoids nested shell quoting).
{ lib, turnipEnv, image, tconnect }:
let
  serve = "/etc/turnip-tests/_serve.py";
  # run run-container.sh as the rootless owner, with its runtime env.
  asHomelab = "/run/wrappers/bin/sudo -u homelab env XDG_RUNTIME_DIR=/run/user/1001 HOME=/home/homelab";
  listen = ns: port: # background a listener in container <ns>'s netns
    "${asHomelab} podman unshare nsenter "
    + "--net=/run/user/1001/turnip/containers/${ns}/netns python3 ${serve} ${port} 30 >/dev/null 2>&1 &";
  attach = ns: args: # run a podman container joined to <ns>'s netns via run-container.sh
    "${asHomelab} bash /etc/turnip-run-container.sh ${ns} turnip-test:latest -- ${tconnect} ${args}";
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
    # load the nix-built image into the rootless store (no registry).
    machine.succeed("${asHomelab} podman load -i ${image}")

    with subtest("podman attach: join netns + resolve peers via generated /etc/hosts"):
        machine.succeed("TURNIP_CONFIG=/etc/turnip-tests/configs/router.json turnip up")
        machine.succeed("${listen "hass" "443"}")
        machine.succeed("${listen "proxy" "443"}")
        machine.sleep(2)
        # a real container in zwave's netns reaches hass BY NAME (hosts file + the flow)
        machine.succeed("${attach "zwave" "hass 443"}")
        # ...but the denied peer (proxy, no flow) is dropped even from a real container
        machine.fail("${attach "zwave" "10.0.0.13 443"}")
        machine.succeed("TURNIP_CONFIG=/etc/turnip-tests/configs/router.json turnip down")
  '';
}
