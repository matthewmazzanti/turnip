# world-vm.nix -- the interactive WORLD dev VM: the external LAN peer for egress/ingress/veth-link
# exploration. Stacks base-vm (9p mount, ssh, mgmt NIC) only -- no podman, no turnip; it's a dumb
# peer. A static address on the shared LAN (eth1) puts it alongside the host's br-lan, plus a
# peer-echo listener (the egress masquerade observer).
#
# Build + run:  just world   (boots qemu; persists ./turnip-world.qcow2; serial console)
# Drive it:     nix/ssh-vm.sh world [dev] [cmd]   (world on :2223)
{ pkgs, ... }:
let
  # The egress masquerade observer: a forking listener on :8443 replying with the source it sees
  # (SOCAT_PEERADDR). Wrapped in a script (not an inline ExecStart) so the socat SYSTEM quoting +
  # $-expansion survive -- systemd's ExecStart parser would otherwise mangle them.
  peerEcho = pkgs.writeShellScript "peer-echo" ''
    exec ${pkgs.socat}/bin/socat TCP-LISTEN:8443,reuseaddr,fork SYSTEM:'printf "%s" "$SOCAT_PEERADDR"'
  '';
in
{
  imports = [ ./base-vm.nix ];

  networking.hostName = "turnip-world";
  # Resolve the host by name on the LAN (symmetry / convenience).
  networking.extraHosts = "192.168.50.10 host";

  # Peer tooling: socat (the peer-echo listener) + the netns/diagnostic CLIs for poking the LAN.
  environment.systemPackages = [ pkgs.socat pkgs.python3 pkgs.iproute2 pkgs.iputils ];

  # The shared LAN: a static address on eth1 (the qemu mcast NIC), alongside the host's br-lan
  # (192.168.50.10/.11). Reachable from a host container's veth-bridge link too.
  systemd.network.networks."21-eth1-lan" = {
    matchConfig.Name = "eth1";
    address = [ "192.168.50.20/24" ];
    networkConfig.ConfigureWithoutCarrier = true;
    linkConfig.RequiredForOnline = "no";
  };

  # Host-forwarded ssh: world VM on :2223.
  virtualisation.forwardPorts = [
    { from = "host"; host.port = 2223; guest.port = 22; }
  ];

  # peer-echo: a forking TCP listener on :8443 that replies with the source address it sees
  # (SOCAT_PEERADDR) -- the same egress masquerade observer the hermetic check uses.
  systemd.services.peer-echo = {
    description = "echo the source address a connecting client presents";
    wantedBy = [ "multi-user.target" ];
    serviceConfig = {
      ExecStart = peerEcho;
      Restart = "always";
    };
  };
}
