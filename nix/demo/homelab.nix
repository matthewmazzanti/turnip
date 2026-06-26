# nix/demo/homelab.nix -- the single-file turnip demo: a self-contained, bootable NixOS VM that
# deploys a routed turnip fabric and three podman containers (quadlet-nix) onto it. You poke the live
# containers with `podman exec` to SEE the two things turnip does that plain podman can't -- directional
# L3 flow control and a real host-LAN link.
#
# The worked example is a homelab edge:
#
#     proxy (10.0.0.13) --tcp/80--> hass (10.0.0.12) --tcp/80--> zwave (10.0.0.11)
#         \----------------------- tcp/80 ------------------------/      hass also:
#                                                                         veth link --> br0 (the LAN)
#
#   * flows are DIRECTIONAL + default-deny: proxy->hass, proxy->zwave, hass->zwave are allowed;
#     zwave initiates NOTHING (a contained device). podman networks are all-or-nothing per network --
#     they can't express this. Each container runs an HTTP server answering "hello from <name>", so an
#     allowed hop returns that banner and a denied one times out.
#   * hass additionally hangs a veth into the VM's bridge br0, so it holds a real LAN address
#     (192.168.1.50) and reaches the bridge's LAN IPs directly -- something rootless podman can't give.
#
# This file focuses on the interesting part -- the turnip, quadlet-nix, and networkd setup. The
# uninteresting substrate (rootless podman, console/ssh access, VM sizing) lives in ./base.nix, pulled
# in via `imports`. turnipPkg/turnipLib come via specialArgs (flake.nix).
{ pkgs, lib, config, turnipPkg, turnipLib, ... }:
let
  owner = "homelab";
  # NixOS exposes every user's resolved uid in the module system, so look it up from the user `base`
  # defines -- the literal 1001 then lives in exactly one place (the user definition in ./base.nix).
  uid = config.users.users.${owner}.uid;
in
{
  imports = [ ./base.nix ];

  # ===========================================================================================
  # The focus: networkd + turnip + quadlet-nix. (Everything else is in ./base.nix.)
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

  # --- turnip: the up/down service (turnipLib.turnipService) -----------------------------------
  # turnipLib.turnipService builds the unit from the turnip pkg + the model (a Nix attrset) + the
  # system podman; the binary it bakes isn't on anyone's PATH (the demo is poked via `podman exec`,
  # not a turnip CLI). It can't know our rootless owner's uid, so we mkMerge in the user-session
  # ordering: the netns are created inside homelab's podman userns, and ordering after
  # user@<uid>.service also guarantees /run/user/<uid> exists (user-runtime-dir@ runs before it).
  systemd.services.turnip = lib.mkMerge [
    (turnipLib.turnipService {
      turnip = turnipPkg;
      podman = config.virtualisation.podman.package;
      # The turnip MODEL. A single routed network `lan`. Every container needs default=true so it can
      # reach a peer's /32 via the gateway (a /32 fabric has no on-link peers -- traffic to a sibling
      # goes up to the router). hass owns its default on eth0 (so it reaches zwave over the fabric);
      # its br0 link is a second, NON-default interface (the LAN is reached by the link's connected
      # /24, no default needed).
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
          # NOTHING -- a compromised IoT device is contained. Directional: each row is one-way (the
          # return rides conntrack), so e.g. zwave->hass is NOT implied by hass->zwave.
          flows = [
            { type = "internal"; from = "proxy"; to = "hass"; proto = "tcp"; port = 80; }
            { type = "internal"; from = "proxy"; to = "zwave"; proto = "tcp"; port = 80; }
            { type = "internal"; from = "hass"; to = "zwave"; proto = "tcp"; port = 80; }
            # NOTE: nothing flows TO proxy and zwave initiates nothing -- default-deny drops the rest.
          ];
        };
      };
    })
    {
      after = [ "user@${toString uid}.service" ];
      wants = [ "user@${toString uid}.service" ];
    }
  ];

  # --- quadlet-nix: the three containers, each attached to its turnip netns ---
  # They're identical bar the name, so a local `mkContainer` builds each: run
  # the hello server on a netshoot image, bind-mount turnip's generated
  # /etc/hosts, run rootless as homelab (rootlessConfig.uid), and order after
  # turnip `up` (the netns + hosts file must exist first). The flow matrix --
  # not the containers -- decides who can actually reach whom.
  virtualisation.quadlet.containers = let
    # turnip pins each container's runtime state under
    # /run/user/<uid>/turnip/containers/<name>; its netns + generated hosts file
    # live there. Resolve that dir once, then the netns ref + hosts mount.
    containerDir = name: "/run/user/${toString uid}/turnip/containers/${name}";

    mkContainer = name: {
      rootlessConfig.uid = uid;
      containerConfig = {
        image = "docker.io/nicolaka/netshoot:latest";
        pull = "missing";
        networks = [ "ns:${containerDir name}/netns" ];
        volumes = [
          "${containerDir name}/hosts:/etc/hosts:ro"
          "${./server.py}:/srv/server.py:ro"
        ];
        exec = [ "python3" "/srv/server.py" name ];
      };
      unitConfig = {
        After = [ "turnip.service" ];
        Requires = [ "turnip.service" ];
      };
    };
  in {
    zwave = mkContainer "zwave";
    hass = mkContainer "hass";
    proxy = mkContainer "proxy";
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
