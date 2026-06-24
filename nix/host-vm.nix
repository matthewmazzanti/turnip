# host-vm.nix -- the interactive HOST dev VM: the system under test. Stacks turnip-host (rootless
# podman + nft/ip) and base-vm (9p mount, ssh, mgmt NIC), then adds the dev toolchain, the
# podman-attach test image, and the modeled LAN: a single data interface (eth1) enslaved to a
# bridge (br-lan) that carries the IPs -- mirroring a bridged server. A container's veth-bridge
# link enslaves into br-lan and appears as a new IP on that LAN.
#
# Build + run:  just host   (boots qemu; persists ./turnip.qcow2; serial console, Ctrl-a x to quit)
# Drive it:     nix/ssh-vm.sh [dev|homelab] [cmd]   (host on :2222)
{ pkgs, ... }:
let
  # The container-attach test toolbox baked into a registry-free OCI image (loaded into homelab's
  # rootless podman at boot): python3 + the netns inspection CLIs.
  testTools = pkgs.buildEnv {
    name = "turnip-test-tools";
    paths = [ pkgs.python3Minimal pkgs.iproute2 pkgs.iputils pkgs.nftables ];
  };
  testImage = pkgs.dockerTools.buildLayeredImage {
    name = "turnip-test";
    tag = "latest";
    contents = [ testTools ];
    config.Env = [ "PATH=${testTools}/bin" ];
  };
in
{
  imports = [ ./turnip-host.nix ./base-vm.nix ];

  networking.hostName = "turnip";
  # Resolve the world peer by name on the LAN (the integration harness dials `world`).
  networking.extraHosts = "192.168.50.20 world";

  # Go + python3: the dev toolchain, and python3 on the host PATH is what the integration
  # harness's in-netns connect probes exec (via `turnip probe`) -- matching the check's host node.
  environment.systemPackages = [ pkgs.go pkgs.python3 ];

  # homelab (the rootless-podman owner, declared by turnip-host) gets its VM login creds here.
  users.users.homelab = {
    password = "homelab";
    openssh.authorizedKeys.keyFiles = [ ./testvm_key.pub ];
  };

  # The modeled LAN: eth1 (the qemu mcast NIC, wired at launch) is enslaved to br-lan, which holds
  # the addresses. Two IPs -- the second is the anchor for a yet-to-be-added secondary forward
  # rule. ConfigureWithoutCarrier brings the bridge up even before a peer appears on the segment.
  systemd.network.netdevs."20-br-lan".netdevConfig = {
    Name = "br-lan";
    Kind = "bridge";
  };
  systemd.network.networks."21-eth1-lan" = {
    matchConfig.Name = "eth1";
    networkConfig.Bridge = "br-lan";
  };
  systemd.network.networks."22-br-lan" = {
    matchConfig.Name = "br-lan";
    address = [ "192.168.50.10/24" "192.168.50.11/24" ];
    networkConfig.ConfigureWithoutCarrier = true;
    linkConfig.RequiredForOnline = "no";
  };

  # Host-forwarded ssh: host VM on :2222.
  virtualisation.forwardPorts = [
    { from = "host"; host.port = 2222; guest.port = 22; }
  ];

  # Load the test image into homelab's rootless store at boot, and name it in the environment.
  environment.sessionVariables.TURNIP_TEST_IMAGE = "${testImage.imageName}:${testImage.imageTag}";
  systemd.services.turnip-test-image = {
    description = "load the python test OCI image into homelab's rootless podman";
    wantedBy = [ "multi-user.target" ];
    after = [ "user@1001.service" ];
    wants = [ "user@1001.service" ];
    serviceConfig = {
      Type = "oneshot";
      RemainAfterExit = true;
    };
    script = ''
      until test -d /run/user/1001; do sleep 0.2; done
      export PATH=/run/wrappers/bin:/run/current-system/sw/bin
      runuser -u homelab -- env XDG_RUNTIME_DIR=/run/user/1001 HOME=/home/homelab PATH="$PATH" \
        podman load -i ${testImage}
    '';
  };
}
