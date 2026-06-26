# nix/demo/homelab.nix -- the single-file turnip demo: a bootable NixOS VM that deploys a routed
# turnip fabric and three podman containers (quadlet-nix) onto it, then hands you tooling to SEE the
# two things turnip does that plain podman can't -- directional L3 flow control and a real host-LAN
# link.
#
# The worked example mirrors the README's hass homelab:
#
#     zwave (10.0.0.11) --tcp/443--> hass (10.0.0.12) --tcp/443--> proxy (10.0.0.13)
#                                     |
#                                     +-- veth link --> br-lan 192.168.1.50/24 (the host LAN)
#
#   * flows are DIRECTIONAL + default-deny: zwave->hass and hass->proxy are allowed; zwave->proxy
#     is DROPPED (no flow). podman networks are all-or-nothing per network -- they can't express this.
#   * hass additionally hangs a veth into the host bridge br-lan, so it holds a real 192.168.1.x LAN
#     address and can reach the host directly -- something rootless podman can't give a container.
#
# Layering: the turnip MODEL is a Nix attrset (turnipConfig) -> turnipLib.turnipService toJSON's it
# and drives `up`/`down`; the CONTAINERS are quadlet-nix units that attach to turnip's pinned netns
# (Network=ns:<path>) and run a trivial listener so an ALLOWED flow connects and a DENIED one times
# out. See turnip-demo (the guided tour) for what to run once it's booted.
#
# Reuses nix/vm/host-base.nix for the substrate (rootless podman owned by `homelab` uid 1001, the
# br-lan host edge, the nft/ip toolkit, and the pre-loaded `turnip-probe` image) so this file only
# carries the demo itself. turnipPkg/turnipLib are threaded in via specialArgs (see flake.nix).
{ pkgs, lib, config, turnipPkg, turnipLib, ... }:
let
  uid = 1001; # homelab, from host-base
  owner = "homelab";
  stateDir = "/run/user/${toString uid}/turnip";
  netnsOf = name: "ns:${stateDir}/containers/${name}/netns";

  # The probe image host-base bakes + loads into homelab's rootless store at boot. Carries
  # python3Minimal (socket) + the netns CLIs; we run it with `pull=never` so the demo stays offline.
  image = "localhost/turnip-probe:latest";

  # --- the turnip MODEL (authored as Nix; toJSON'd by turnipLib) --------------------------------
  #
  # A single routed network `lan`. Every container needs default=true so it can reach a peer's /32
  # via the gateway (a /32 fabric has no on-link peers -- traffic to a sibling goes up to the router).
  # hass owns its default on eth0 (so it reaches proxy over the fabric); its br-lan link is a second,
  # NON-default interface (the LAN is reached by the link's connected /24, no default needed).
  turnipConfig = {
    runtime.user = owner;
    containers = {
      zwave = { };
      hass.links = [
        # The L2 escape: a veth from hass's netns into the host bridge br-lan, giving hass a real LAN
        # IP. Outside every router's nft policy -- a deliberate trust grant (see docs/CONFIG-SKETCH.md).
        { type = "veth"; bridge = "br-lan"; name = "eth2"; address = "192.168.1.50/24"; }
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
      After = [ "turnip.service" "turnip-test-image.service" ];
      Requires = [ "turnip.service" "turnip-test-image.service" ];
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
  imports = [
    ../vm/host-base.nix
  ];

  networking.hostName = "turnip-demo";

  # --- layer 3: the turnip up/down service (built from the attrset) ---------------------------
  # requiresUserSession wires the real ordering: after homelab's user@1001 session + a wait on
  # /run/user/1001 (the netns are created inside homelab's podman userns).
  systemd.services = (turnipLib.turnipService {
    package = turnipBin;
    name = "turnip";
    requiresUserSession = uid;
  }).systemd.services;

  # --- the containers (quadlet-nix), attached to turnip's netns -------------------------------
  # zwave + hass are flow TARGETS, so they listen on :443; proxy only initiates, so it just idles.
  virtualisation.quadlet.containers = {
    zwave = mkContainer "zwave" listen443;
    hass = mkContainer "hass" listen443;
    proxy = mkContainer "proxy" sleepForever;
  };

  # --- the demo surface -----------------------------------------------------------------------
  environment.systemPackages = [
    turnipBin # `turnip` -> the wrapped binary (config baked)
    turnipDemo # `turnip-demo` -> the guided tour
    pkgs.tcpdump # show the routed fabric has no inter-container ARP (vs a podman bridge)
  ];

  # A console-friendly demo VM: admin `demo` user, autologin, and a banner pointing at the tour.
  users.users.demo = {
    isNormalUser = true;
    extraGroups = [ "wheel" ];
    password = "demo";
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

  # Optional out-of-band access: a qemu user-mode mgmt NIC (eth0) + sshd on a forwarded port, so you
  # can `ssh demo@localhost -p 2224` instead of only using the serial console. The modeled LAN is
  # br-lan on eth1 (host-base); eth0 is management only. The committed dev key authorizes `demo`.
  systemd.network.networks."10-mgmt" = {
    matchConfig.Name = "eth0";
    networkConfig.DHCP = "yes";
    # NB: leave RequiredForOnline at its default (required). eth0 is the only managed link that's
    # actually routable (slirp DHCP); br-lan is RequiredForOnline=no (host-base) and carrier-less in
    # this single-NIC VM. If eth0 were also =no, the wait-online candidate set would be EMPTY and
    # systemd-networkd-wait-online would block its full 120s timeout, parking network-online.target
    # (and the containers + multi-user.target behind it). Keeping eth0 required => it completes in s.
  };
  services.openssh.enable = true;
  users.users.demo.openssh.authorizedKeys.keyFiles = [ ../vm/testvm_key.pub ];
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
