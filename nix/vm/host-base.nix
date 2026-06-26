# host-base.nix -- the shared base for both HOST variants (interactive.host + test.host): "a host
# that can run turnip" plus its LAN edge. Rootless podman owned by `homelab` (loginnable), the
# nft/ip toolkit turnip drives, the baked /etc/turnip/ssh-key, and the modeled LAN -- eth1 enslaved
# to a bridge (br-lan) carrying 192.168.1.1, mirroring the runNixOSTest VLAN (host .1 / world .2) so
# the hermetic check and the dev VM run identical network config. It also pre-loads the probe image
# (below). The interactive host grows from this base via ./interactive.nix (9p, mgmt NIC, dev user)
# + the go toolchain.
{ pkgs, lib, ... }:
let
  # The python netns-probe OCI image (python3 + netns/diagnostic CLIs), loaded into homelab's
  # rootless store at boot (below). Referenced by TAG, so the per-node store path doesn't matter --
  # TestPodmanRun runs `localhost/turnip-probe:latest`, never a tar.
  probeImage = import ./probe-image.nix { inherit pkgs; };
in
{
  system.stateVersion = "25.05";

  # Don't let the host firewall interfere with turnip's netns/nft work.
  networking.firewall.enable = false;

  # Rootless podman, owned by `homelab` (the runtime.user in the configs). linger =>
  # /run/user/<uid> + the pause process exist with no active login; autoSubUidGidRange
  # => the subuid/subgid range podman maps. uid is pinned so state paths
  # (/run/user/1001/turnip/...) are deterministic. Login creds so homelab is reachable on both the
  # dev host (ssh-vm.sh homelab) and the check host (driverInteractive debugging).
  virtualisation.podman.enable = true;
  users.users.homelab = {
    isNormalUser = true;
    uid = 1001;
    linger = true;
    autoSubUidGidRange = true;
    password = "homelab";
    openssh.authorizedKeys.keyFiles = [ ./testvm_key.pub ];
  };

  # The turnip-host toolkit; each host variant adds only its own tool (go on the dev host, the
  # turnip binary on the check host).
  environment.systemPackages = [
    pkgs.nftables # `nft` -- turnip drives nftables
    pkgs.iproute2 # ip/ss -- inspect links/routes
    pkgs.jq # inspect `nft -j`
    pkgs.python3 # the harness's in-netns connect probe (execs `python3` on PATH)
    pkgs.iputils # ping -- reachability checks
    pkgs.openssh # the ssh client the harness dials the world peer with
  ];

  # The throwaway test key, baked in at /etc/turnip/ssh-key for the integration harness to ssh the
  # world peer -- no manual key staging. Root-owned 0600 so ssh accepts it when run as root.
  environment.etc."turnip/ssh-key" = {
    source = ./testvm_key;
    mode = "0600";
  };

  # Pre-load the probe image into homelab's rootless store at boot, so it's a ready NAMED image for
  # the operator-path test (`podman run` the name -- you can't `podman run` a tar). Loaded at boot,
  # where homelab's user session is up, rather than from the test's sudo/ssh context. The test
  # (TestPodmanRun) assumes it's loaded and runs the tag passed as -image (TURNIP_TEST_IMAGE).
  environment.sessionVariables.TURNIP_TEST_IMAGE = "localhost/${probeImage.imageName}:${probeImage.imageTag}";
  systemd.services.turnip-test-image = {
    description = "load the python probe OCI image into homelab's rootless podman";
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
      runuser -u homelab -- \
        env XDG_RUNTIME_DIR=/run/user/1001 HOME=/home/homelab PATH="$PATH" \
          podman load -i ${probeImage}
    '';
  };

  # The LAN edge. eth1 (the qemu mcast NIC in the dev VM / the runNixOSTest VLAN NIC in the check)
  # is enslaved to br-lan, which holds the host address -- 192.168.1.1, mirroring the test net. The
  # bridge is what the L2 *link* feature enslaves a container veth into to give it a LAN IP.
  networking.useNetworkd = true;
  networking.useDHCP = false;
  # Take the LAN over from runNixOSTest's auto-assignment: eth1 carries no IP of its own (it's
  # bridged), so clear the address the test driver would otherwise put on it. No-op in the dev VM.
  networking.interfaces.eth1.ipv4.addresses = lib.mkForce [ ];
  networking.extraHosts = "192.168.1.2 world"; # resolve the world peer by name
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
    address = [ "192.168.1.1/24" ];
    networkConfig.ConfigureWithoutCarrier = true; # bring the bridge up before a peer appears
    linkConfig.RequiredForOnline = "no";
  };
}
