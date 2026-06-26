# nix/demo/homelab.nix -- the single-file turnip demo: a self-contained, bootable NixOS VM that
# deploys a routed turnip fabric and three podman containers (quadlet-nix) onto it, then hands you
# tooling to SEE the two things turnip does that plain podman can't -- directional L3 flow control and
# a real host-LAN link.
#
# The worked example is a homelab edge:
#
#     proxy (10.0.0.13) --tcp/443--> hass (10.0.0.12) --tcp/443--> zwave (10.0.0.11)
#         \------------------------ tcp/443 -----------------------/      hass also:
#                                                                         veth link --> br0 (the LAN)
#
#   * flows are DIRECTIONAL + default-deny: proxy->hass, proxy->zwave, hass->zwave are allowed;
#     zwave initiates NOTHING (a contained device). podman networks are all-or-nothing per network --
#     they can't express this.
#   * hass additionally hangs a veth into the VM's bridge br0, so it holds a real LAN address
#     (192.168.1.50) and reaches the bridge's LAN IPs directly -- something rootless podman can't give.
#
# This file is deliberately self-contained. To keep the focus on the interesting part -- the turnip,
# quadlet-nix, and networkd setup, in the module body at the bottom -- the uninteresting substrate
# (rootless podman, the boot image load, the toolkit, the console/ssh access, VM sizing) is lifted
# into an inline `base` module in the `let` and pulled in via `imports`. turnipPkg/turnipLib are
# threaded in via specialArgs (see flake.nix).
{ pkgs, lib, config, turnipPkg, turnipLib, ... }:
let
  uid = 1001;
  owner = "homelab";
  stateDir = "/run/user/${toString uid}/turnip";
  netnsOf = name: "ns:${stateDir}/containers/${name}/netns";

  # A standard OCI image, pulled straight from a registry -- no nix-built image and no boot-time
  # `podman load`. podman pulls it on first container start (so the VM needs egress -- see br0 below)
  # and caches it in homelab's store. python:3-alpine is small and ships python3 (the listener needs
  # only `socket`); musl reads /etc/hosts, so the bind-mounted peer names still resolve.
  image = "docker.io/library/python:3-alpine";

  # --- the turnip MODEL (authored as Nix; toJSON'd by turnipLib) --------------------------------
  #
  # A single routed network `lan`. Every container needs default=true so it can reach a peer's /32
  # via the gateway (a /32 fabric has no on-link peers -- traffic to a sibling goes up to the router).
  # hass owns its default on eth0 (so it reaches zwave over the fabric); its br0 link is a second,
  # NON-default interface (the LAN is reached by the link's connected /24, no default needed).
  turnipConfig = {
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

  # --- the listeners the containers run -------------------------------------------------------
  #
  # A trivial accept-and-close TCP server on :443 (uses only `socket`, so python3Minimal suffices).
  # A connect probe to an ALLOWED peer completes the handshake; a DENIED one is dropped at the router
  # and times out. Container root holds CAP_NET_BIND_SERVICE, so binding :443 is fine.
  listen443 = lib.concatStringsSep "; " [
    "import socket"
    "s=socket.socket()"
    "s.setsockopt(socket.SOL_SOCKET,socket.SO_REUSEADDR,1)"
    ''s.bind(("0.0.0.0",443))''
    "s.listen(16)"
    "[c.close() for c in iter(lambda: s.accept()[0], None)]"
  ];
  sleepForever = "import time; time.sleep(10**9)";

  # One quadlet container attached to its turnip netns. rootlessConfig.uid runs it as homelab (the
  # netns owner) under system systemd; we hand-wire the rootless env (XDG_RUNTIME_DIR/HOME) since
  # that mode doesn't set it. Ordered after turnip `up` (the netns must exist) + the image load.
  mkContainer = name: pyArgs: {
    autoStart = true;
    rootlessConfig.uid = uid;
    containerConfig = {
      inherit image;
      pull = "missing"; # pull once on first start, then use the cached copy
      networks = [ (netnsOf name) ];
      # Bind-mount turnip's generated /etc/hosts (written by `up`, under the state dir) over the
      # container's, so each container resolves exactly the peers its flows allow (the body is
      # directional -- only this container's flow targets). turnip writes it before the container
      # starts (ordered after turnip.service). ro: it's a generated artifact.
      volumes = [ "${stateDir}/containers/${name}/hosts:/etc/hosts:ro" ];
      exec = [ "python3" "-u" "-c" pyArgs ];
    };
    unitConfig = {
      After = [ "turnip.service" ];
      Requires = [ "turnip.service" ];
    };
    serviceConfig.Environment = [
      "XDG_RUNTIME_DIR=/run/user/${toString uid}"
      "HOME=/home/${owner}"
    ];
  };

  # The wrapped turnip binary (layer 2: attrset -> toJSON -> --config baked). Installed as `turnip`
  # so `sudo turnip up|down|probe ...` from the console all use the demo's model. podman is the
  # SYSTEM package so the rootless newuidmap/newgidmap wrappers line up.
  turnipBin = turnipLib.turnipWithConfig {
    config = turnipConfig;
    name = "turnip";
    turnip = turnipPkg;
    podman = config.virtualisation.podman.package;
  };

  # The guided tour: the script body lives in ./tour.sh (an independent, shellcheck-clean file);
  # here we just wrap it into a `turnip-demo` command with its inspection tools on PATH. It drives
  # the live fabric through `sudo turnip ...` (turnipBin is installed as `turnip`, below).
  turnipDemo = pkgs.writeShellApplication {
    name = "turnip-demo";
    runtimeInputs = [ pkgs.python3 pkgs.iproute2 pkgs.iputils pkgs.nftables ];
    text = builtins.readFile ./tour.sh;
  };

  # --- base: the uninteresting substrate, inline so the file stays self-contained --------------
  #
  # Everything here is plumbing the demo needs but isn't ABOUT: rootless podman + its owner, the boot
  # image load, the host-side toolkit, the console/ssh access, and VM sizing. It's an inline module
  # (closes over the `let` above, takes pkgs from module args) pulled in via `imports`, so the body
  # below can be just the turnip + quadlet + networkd setup.
  base = { pkgs, ... }: {
    system.stateVersion = "25.05";
    networking.hostName = "turnip-demo";
    networking.firewall.enable = false; # don't let the host firewall interfere with turnip's netns/nft

    # Rootless podman owned by homelab: linger => /run/user/1001 + the pause process exist with no
    # login; autoSubUidGidRange => the subuid/subgid range podman maps; uid pinned so state paths
    # (/run/user/1001/turnip/...) are stable.
    virtualisation.podman.enable = true;
    users.users.${owner} = {
      isNormalUser = true;
      uid = uid;
      linger = true;
      autoSubUidGidRange = true;
      password = owner; # console-debug convenience; the demo logs in as `demo`
    };

    # DNS, so rootless podman can resolve the registry to pull the image. resolved picks up the DNS
    # server br0's DHCP lease hands out (slirp's proxy); /etc/resolv.conf -> the resolved stub.
    services.resolved.enable = true;

    # The host-side toolkit `turnip probe` execs inside a netns (the binary itself bakes nft+podman).
    environment.systemPackages = [
      pkgs.nftables # nft -- inspect the live flow matrix
      pkgs.iproute2 # ip -- inspect links/routes
      pkgs.iputils # ping -- link reachability checks
      pkgs.python3 # the in-netns connect probes
      pkgs.tcpdump # show the routed fabric has no inter-container ARP
    ];

    # Console-friendly: admin `demo` user (autologin, passwordless sudo), a banner, and ssh on a
    # forwarded port (host :2224 -> guest 22) so the demo is drivable over ssh too. The committed dev
    # key authorizes `demo`. graphics=false so `nix run .#demo` lands on the serial console.
    users.users.demo = {
      isNormalUser = true;
      extraGroups = [ "wheel" ];
      password = "demo";
      openssh.authorizedKeys.keyFiles = [ ../vm/testvm_key.pub ];
    };
    security.sudo.wheelNeedsPassword = false;
    services.getty.autologinUser = "demo";
    services.openssh.enable = true;
    virtualisation.forwardPorts = [{ from = "host"; host.port = 2224; guest.port = 22; }];
    virtualisation = {
      graphics = false;
      memorySize = 2048;
      cores = 2;
      diskSize = 8192; # room for podman images
    };
    users.motd = ''

      ===========================================================================
        turnip demo -- a routed podman fabric (zwave / hass / proxy) on this host.

        Run the guided tour:    turnip-demo
        Poke it by hand:        sudo turnip probe <container> -- <cmd...>
                                sudo turnip probe router:lan -- nft list table inet turnip
      ===========================================================================
    '';
  };
in
{
  imports = [ base ];

  # ===========================================================================================
  # The focus: turnip + quadlet-nix + networkd. (Everything else is in `base`, above.)
  # ===========================================================================================

  # --- turnip: lower the model to an up/down service (layer 3 of nix/lib/turnip.nix) ----------
  # requiresUserSession wires the real ordering: after homelab's user@1001 session + a wait on
  # /run/user/1001 (the netns are created inside homelab's podman userns).
  systemd.services.turnip = (turnipLib.turnipService {
    package = turnipBin;
    name = "turnip";
    requiresUserSession = uid;
  }).systemd.services.turnip;

  # `turnip` (config baked) + the guided tour, on PATH.
  environment.systemPackages = [ turnipBin turnipDemo ];

  # --- quadlet-nix: the three containers, each attached to its turnip netns --------------------
  # zwave + hass are flow TARGETS, so they listen on :443; proxy only initiates, so it just idles.
  virtualisation.quadlet.containers = {
    zwave = mkContainer "zwave" listen443;
    hass = mkContainer "hass" listen443;
    proxy = mkContainer "proxy" sleepForever;
  };

  # --- networkd: ONE interface, a bridge with one primary + two secondary IPs ------------------
  # The qemu user-mode NIC (eth0) is enslaved to br0; br0 is the only L3 interface. The PRIMARY comes
  # from DHCP -- slirp hands out 10.0.2.15 (the :2224->22 hostfwd target, so ssh/control works) plus a
  # default route + DNS, which the containers need to PULL their image. The two SECONDARY 192.168.1.x
  # are the LAN segment hass's veth link joins -- hass reaches both. RequiredForOnline=routable holds
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
}
