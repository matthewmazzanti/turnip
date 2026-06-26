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
# Networking is ONE interface: the qemu user-mode NIC (eth0) enslaved to a bridge (br0). br0 carries a
# PRIMARY ip (10.0.2.15 -- the qemu guest address, so the :2224->22 hostfwd reaches ssh/control) and
# TWO SECONDARY ips (192.168.1.1, 192.168.1.2 -- the LAN segment hass's link joins). hass reaches both.
#
# Self-contained: this file inlines the small substrate the demo needs (rootless podman owned by
# `homelab`, a minimal container image loaded at boot, the nft/ip toolkit), rather than reusing the
# 2-VM test harness's host-base. turnipPkg/turnipLib are threaded in via specialArgs (see flake.nix).
{ pkgs, lib, config, turnipPkg, turnipLib, ... }:
let
  uid = 1001;
  owner = "homelab";
  stateDir = "/run/user/${toString uid}/turnip";
  netnsOf = name: "ns:${stateDir}/containers/${name}/netns";

  # A minimal, registry-free container image: python3 only (the listener + the by-name connect). The
  # tour's in-netns inspection uses HOST tools via `turnip probe`, not the image, so nothing else is
  # needed. Built once, loaded into homelab's rootless store at boot (turnip-demo-image, below), and
  # referenced by tag with pull=never -- so the demo is fully offline.
  demoImage = pkgs.dockerTools.buildLayeredImage {
    name = "turnip-demo-img";
    tag = "latest";
    contents = [ pkgs.python3Minimal ];
    config.Env = [ "PATH=${pkgs.python3Minimal}/bin" ];
  };
  image = "localhost/turnip-demo-img:latest";
  imageUnit = "turnip-demo-image.service";

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
      pull = "never";
      networks = [ (netnsOf name) ];
      # Bind-mount turnip's generated /etc/hosts (written by `up`, under the state dir) over the
      # container's, so each container resolves exactly the peers its flows allow (the body is
      # directional -- only this container's flow targets). turnip writes it before the container
      # starts (ordered after turnip.service). ro: it's a generated artifact.
      volumes = [ "${stateDir}/containers/${name}/hosts:/etc/hosts:ro" ];
      exec = [ "python3" "-u" "-c" pyArgs ];
    };
    unitConfig = {
      After = [ "turnip.service" imageUnit ];
      Requires = [ "turnip.service" imageUnit ];
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
in
{
  system.stateVersion = "25.05";
  networking.hostName = "turnip-demo";
  networking.firewall.enable = false; # don't let the host firewall interfere with turnip's netns/nft

  # --- the substrate: rootless podman owned by homelab + the demo image -----------------------
  # linger => /run/user/1001 + the pause process exist with no login; autoSubUidGidRange => the
  # subuid/subgid range podman maps. uid pinned so state paths (/run/user/1001/turnip/...) are stable.
  virtualisation.podman.enable = true;
  users.users.${owner} = {
    isNormalUser = true;
    uid = uid;
    linger = true;
    autoSubUidGidRange = true;
    password = owner; # console-debug convenience; the demo logs in as `demo`
  };

  # Load the demo image into homelab's rootless store at boot (you can't `podman run` a tar, and
  # pull=never needs it present). Done where homelab's user session is up, not from a probe context.
  systemd.services.turnip-demo-image = {
    description = "load the demo container image into homelab's rootless podman";
    wantedBy = [ "multi-user.target" ];
    after = [ "user@${toString uid}.service" ];
    wants = [ "user@${toString uid}.service" ];
    serviceConfig = {
      Type = "oneshot";
      RemainAfterExit = true;
    };
    script = ''
      until test -d /run/user/${toString uid}; do sleep 0.2; done
      export PATH=/run/wrappers/bin:/run/current-system/sw/bin
      runuser -u ${owner} -- \
        env XDG_RUNTIME_DIR=/run/user/${toString uid} HOME=/home/${owner} PATH="$PATH" \
          podman load -i ${demoImage}
    '';
  };

  # --- layer 3: the turnip up/down service (built from the attrset) ---------------------------
  # requiresUserSession wires the real ordering: after homelab's user@1001 session + a wait on
  # /run/user/1001 (the netns are created inside homelab's podman userns). A partial path (.turnip)
  # so it coexists with the turnip-demo-image service above.
  systemd.services.turnip = (turnipLib.turnipService {
    package = turnipBin;
    name = "turnip";
    requiresUserSession = uid;
  }).systemd.services.turnip;

  # --- the containers (quadlet-nix), attached to turnip's netns -------------------------------
  # zwave + hass are flow TARGETS, so they listen on :443; proxy only initiates, so it just idles.
  virtualisation.quadlet.containers = {
    zwave = mkContainer "zwave" listen443;
    hass = mkContainer "hass" listen443;
    proxy = mkContainer "proxy" sleepForever;
  };

  # --- networking: ONE interface, a bridge with one primary + two secondary IPs ----------------
  # The qemu user-mode NIC (eth0) is enslaved to br0; br0 is the only L3 interface. PRIMARY 10.0.2.15
  # is the qemu guest address, so the :2224->22 hostfwd reaches ssh/control. The two SECONDARY
  # 192.168.1.x are the LAN segment hass's veth link joins -- hass reaches both. Static => no DHCP
  # timing, and br0 (RequiredForOnline=degraded) satisfies wait-online as soon as it has an address.
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
    address = [
      "10.0.2.15/24" # PRIMARY: control/ssh (qemu user-net guest address; the hostfwd target)
      "192.168.1.1/24" # SECONDARY: a LAN device hass can reach
      "192.168.1.2/24" # SECONDARY: a second LAN device
    ];
    networkConfig.ConfigureWithoutCarrier = true;
    linkConfig.RequiredForOnline = "degraded";
  };

  # --- the demo surface -----------------------------------------------------------------------
  environment.systemPackages = [
    turnipBin # `turnip` -> the wrapped binary (config baked)
    turnipDemo # `turnip-demo` -> the guided tour
    pkgs.nftables # `nft` -- turnip drives nftables; the tour inspects the live matrix
    pkgs.iproute2 # ip -- inspect links/routes in the probes
    pkgs.iputils # ping -- the link reachability checks
    pkgs.python3 # the tour's in-netns connect probes (run host-side via `turnip probe`)
    pkgs.tcpdump # show the routed fabric has no inter-container ARP (vs a podman bridge)
  ];

  # A console-friendly demo VM: admin `demo` user, autologin, and a banner pointing at the tour.
  users.users.demo = {
    isNormalUser = true;
    extraGroups = [ "wheel" ];
    password = "demo";
    openssh.authorizedKeys.keyFiles = [ ../vm/testvm_key.pub ];
  };
  security.sudo.wheelNeedsPassword = false;
  services.getty.autologinUser = "demo";
  users.motd = ''

    ===========================================================================
      turnip demo -- a routed podman fabric (zwave / hass / proxy) on this host.

      Run the guided tour:    turnip-demo
      Poke it by hand:        sudo turnip probe <container> -- <cmd...>
                              sudo turnip probe router:lan -- nft list table inet turnip
    ===========================================================================
  '';

  # ssh on a forwarded port (host :2224 -> guest 22), so you can drive the demo over ssh as well as
  # the serial console. The committed dev key authorizes `demo` (above).
  services.openssh.enable = true;
  virtualisation.forwardPorts = [
    { from = "host"; host.port = 2224; guest.port = 22; }
  ];

  # Self-contained single-VM sizing (no 9p, no dev tooling -- this is a standalone example).
  # graphics=false so `nix run .#demo` drops you straight onto the serial console.
  virtualisation = {
    graphics = false;
    memorySize = 2048;
    cores = 2;
    diskSize = 8192; # room for podman images
  };
}
