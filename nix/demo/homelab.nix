# nix/demo/homelab.nix -- the single-file turnip demo: a self-contained, bootable NixOS VM that
# deploys a routed turnip fabric and three podman containers (quadlet-nix) onto it. You poke the live
# containers with `podman exec` to SEE the two things turnip does that plain podman can't -- directional
# L3 flow control and a real host-LAN link.
#
# The worked example is a homelab edge:
#
#     proxy (10.0.0.13) --tcp/443--> hass (10.0.0.12) --tcp/443--> zwave (10.0.0.11)
#         \------------------------ tcp/443 -----------------------/      hass also:
#                                                                         veth link --> br0 (the LAN)
#
#   * flows are DIRECTIONAL + default-deny: proxy->hass, proxy->zwave, hass->zwave are allowed;
#     zwave initiates NOTHING (a contained device). podman networks are all-or-nothing per network --
#     they can't express this. Each container runs an HTTP server answering "hello from <name>", so an
#     allowed hop returns that banner and a denied one times out.
#   * hass additionally hangs a veth into the VM's bridge br0, so it holds a real LAN address
#     (192.168.1.50) and reaches the bridge's LAN IPs directly -- something rootless podman can't give.
#
# This file is deliberately self-contained. To keep the focus on the interesting part -- the turnip,
# quadlet-nix, and networkd setup, in the module body at the bottom -- the uninteresting substrate
# (rootless podman, console/ssh access, VM sizing) is lifted into an inline `base` module, defined
# first in the `let` and pulled in via `imports`. turnipPkg/turnipLib come via specialArgs (flake.nix).
{ pkgs, lib, config, turnipPkg, turnipLib, ... }:
let
  uid = 1001;
  owner = "homelab";

  # --- base: the uninteresting substrate, inline so the file stays self-contained --------------
  #
  # Everything here is plumbing the demo needs but isn't ABOUT: rootless podman + its owner, console
  # autologin + ssh, DNS for the image pull, VM sizing. It's an inline module (closes over the `let`,
  # takes pkgs from module args) pulled in via `imports`, so the body below can be just the turnip +
  # quadlet + networkd setup.
  base = { pkgs, ... }: {
    system.stateVersion = "25.05";
    networking.hostName = "turnip-demo";
    networking.firewall.enable = false; # don't let the host firewall interfere with turnip's netns/nft

    # Rootless podman owned by homelab: linger => /run/user/1001 + the pause process exist with no
    # login; autoSubUidGidRange => the subuid/subgid range podman maps; uid pinned so state paths
    # (/run/user/1001/turnip/...) are stable. The demo runs AS homelab (autologin + the dev key over
    # ssh) so `podman exec` into the containers needs no sudo.
    virtualisation.podman.enable = true;
    users.users.${owner} = {
      isNormalUser = true;
      uid = uid;
      linger = true;
      autoSubUidGidRange = true;
      password = owner;
      openssh.authorizedKeys.keyFiles = [ ../vm/testvm_key.pub ];
    };
    services.getty.autologinUser = owner;
    services.openssh.enable = true;

    # DNS, so rootless podman can resolve the registry to pull the image. resolved picks up the DNS
    # server br0's DHCP lease hands out (slirp's proxy); /etc/resolv.conf -> the resolved stub.
    services.resolved.enable = true;

    # ssh on a forwarded port (host :2224 -> guest 22). graphics=false so `nix run .#demo` lands on
    # the serial console.
    virtualisation.forwardPorts = [{ from = "host"; host.port = 2224; guest.port = 22; }];
    virtualisation = {
      graphics = false;
      memorySize = 2048;
      cores = 2;
      diskSize = 8192; # room for the netshoot image
    };
    users.motd = ''

      ===========================================================================
        turnip demo -- a routed podman fabric (zwave / hass / proxy) on this host.

        Run the guided tour:    turnip-demo
        Poke it by hand:        podman exec <container> curl http://<peer>:443
                                podman exec <container> ip -br addr
      ===========================================================================
    '';
  };

in
{
  imports = [ base ];

  # ===========================================================================================
  # The focus: networkd + turnip + quadlet-nix. (Everything else is in `base`, above.)
  # ===========================================================================================

  # --- networkd: ONE interface, a bridge with one primary + two secondary IPs ------------------
  # The qemu user-mode NIC (eth0) is enslaved to br0; br0 is the only L3 interface. The PRIMARY comes
  # from DHCP -- slirp hands out 10.0.2.15 (the :2224->22 hostfwd target, so ssh works) plus a default
  # route + DNS, which the containers need to PULL their image. The two SECONDARY 192.168.1.x are the
  # LAN segment hass's veth link joins -- hass reaches both. RequiredForOnline=routable holds
  # network-online.target (hence the container pulls) until egress is actually up.
  networking.useNetworkd = true;
  networking.useDHCP = false;
  systemd.network.netdevs."20-br0".netdevConfig = {
    Name = "br0";
    Kind = "bridge";
  };
  systemd.network.networks."21-eth0-port" = {
    matchConfig.Name = "eth0";
    networkConfig.Bridge = "br0";
    linkConfig.RequiredForOnline = "no"; # a bridge port; its own onlineness is irrelevant
  };
  systemd.network.networks."22-br0" = {
    matchConfig.Name = "br0";
    networkConfig.DHCP = "yes"; # PRIMARY 10.0.2.15 + default route + DNS, from slirp
    address = [
      "192.168.1.1/24" # SECONDARY: a LAN device hass can reach
      "192.168.1.2/24" # SECONDARY: a second LAN device
    ];
    networkConfig.ConfigureWithoutCarrier = true;
    linkConfig.RequiredForOnline = "routable";
  };

  # --- turnip: the up/down service, inlined ---------------------------------------------------
  # The model is baked into a `turnip` binary (turnipWithConfig) scoped to THIS service -- it isn't on
  # anyone's PATH; the demo is poked via `podman exec`, not a turnip CLI. The netns are created inside
  # homelab's podman userns, so order after that user session (which also guarantees /run/user/1001
  # exists -- user-runtime-dir@1001 is ordered before user@1001).
  systemd.services.turnip =
    let
      # The turnip MODEL, authored as Nix (turnipWithConfig toJSON's it in the service below). A single
      # routed network `lan`. Every container needs default=true so it can reach a peer's /32 via the
      # gateway (a /32 fabric has no on-link peers -- traffic to a sibling goes up to the router). hass
      # owns its default on eth0 (so it reaches zwave over the fabric); its br0 link is a second,
      # NON-default interface (the LAN is reached by the link's connected /24, no default needed).
      turnipBin = turnipLib.turnipWithConfig {
        turnip = turnipPkg;
        podman = config.virtualisation.podman.package;
        config = {
          runtime.user = owner;
          containers = {
            zwave = { };
            hass.links = [
              # The L2 escape: a veth from hass's netns into the VM bridge br0, giving hass a real LAN IP.
              # Outside every router's nft policy -- a deliberate trust grant (see docs/CONFIG-SKETCH.md).
              { type = "veth"; bridge = "br0"; name = "eth2"; address = "192.168.1.50/24"; }
            ];
            proxy = { };
          };
          networks.lan = {
            gateway = "10.0.0.1";
            gateway_if = "fabric0";
            attach = {
              zwave = { ip = "10.0.0.11"; interface = "eth0"; default = true; };
              hass = { ip = "10.0.0.12"; interface = "eth0"; default = true; };
              proxy = { ip = "10.0.0.13"; interface = "eth0"; default = true; };
            };
            # The real homelab policy. proxy (the ingress reverse-proxy) reaches the hub and the device
            # controller; the hub reaches the device controller; the device controller (zwave) initiates
            # NOTHING -- a compromised IoT device is contained. Directional: each row is one-way (the return
            # rides conntrack), so e.g. zwave->hass is NOT implied by hass->zwave.
            flows = [
              { type = "internal"; from = "proxy"; to = "hass"; proto = "tcp"; port = 443; }
              { type = "internal"; from = "proxy"; to = "zwave"; proto = "tcp"; port = 443; }
              { type = "internal"; from = "hass"; to = "zwave"; proto = "tcp"; port = 443; }
              # NOTE: nothing flows TO proxy and zwave initiates nothing -- default-deny drops the rest.
            ];
          };
        };
      };
      exe = lib.getExe turnipBin;
    in
    {
      description = "turnip routed container network";
      wantedBy = [ "multi-user.target" ];
      after = [ "network.target" "podman.service" "user@${toString uid}.service" ];
      wants = [ "user@${toString uid}.service" ];
      path = [ pkgs.nftables config.virtualisation.podman.package ];
      serviceConfig = {
        Type = "oneshot";
        RemainAfterExit = true;
        ExecStart = "${exe} up";
        ExecStop = "${exe} down";
      };
    };

  # --- quadlet-nix: the three containers, each attached to its turnip netns --------------------
  # Spelled out per container (rather than a helper) to keep the quadlet-nix
  # surface visible. They're identical bar the name: each runs the hello server,
  # mounts turnip's generated /etc/hosts, runs rootless as homelab
  # (rootlessConfig.uid), and orders after turnip `up` (the netns + hosts file
  # must exist first). The flow matrix -- not the containers -- decides who can
  # actually reach whom.
  virtualisation.quadlet.containers = let
    stateDir = "/run/user/${toString uid}/turnip";
    netnsOf = name: "ns:${stateDir}/containers/${name}/netns";
    image = "docker.io/nicolaka/netshoot:latest";
    # The hello server (./server.py), bind-mounted in and run as `python3 /srv/server.py <name>` --
    # so the name is argv[1], with no Nix templating into the source and no -c escaping.
    serverMount = "${./server.py}:/srv/server.py:ro";
  in {
    zwave = {
      autoStart = true;
      rootlessConfig.uid = uid;
      containerConfig = {
        inherit image;
        pull = "missing";
        networks = [ (netnsOf "zwave") ];
        volumes = [ "${stateDir}/containers/zwave/hosts:/etc/hosts:ro" serverMount ];
        exec = [ "python3" "/srv/server.py" "zwave" ];
      };
      unitConfig = {
        After = [ "turnip.service" ];
        Requires = [ "turnip.service" ];
      };
    };

    hass = {
      autoStart = true;
      rootlessConfig.uid = uid;
      containerConfig = {
        inherit image;
        pull = "missing";
        networks = [ (netnsOf "hass") ];
        volumes = [ "${stateDir}/containers/hass/hosts:/etc/hosts:ro" serverMount ];
        exec = [ "python3" "/srv/server.py" "hass" ];
      };
      unitConfig = {
        After = [ "turnip.service" ];
        Requires = [ "turnip.service" ];
      };
    };

    proxy = {
      autoStart = true;
      rootlessConfig.uid = uid;
      containerConfig = {
        inherit image;
        pull = "missing";
        networks = [ (netnsOf "proxy") ];
        volumes = [ "${stateDir}/containers/proxy/hosts:/etc/hosts:ro" serverMount ];
        exec = [ "python3" "/srv/server.py" "proxy" ];
      };
      unitConfig = {
        After = [ "turnip.service" ];
        Requires = [ "turnip.service" ];
      };
    };
  };

  # The guided tour, on PATH (run as homelab). No turnip CLI is installed.
  environment.systemPackages = let
    # The guided tour: the script body lives in ./tour.sh (an independent,
    # shellcheck-clean file). It runs as homelab and pokes the live containers
    # with plain `podman exec` (no turnip CLI involved).
    turnipDemo = pkgs.writeShellApplication {
      name = "turnip-demo";
      runtimeInputs = [ config.virtualisation.podman.package pkgs.coreutils ];
      text = builtins.readFile ./tour.sh;
    };
  in [ turnipDemo ];
}
