# world-base.nix -- the shared base for both WORLD variants (interactive.world + test.world): the
# external LAN peer. The peer-echo egress observer, sshd + the committed key authorized for root (so
# the host can dial root@world), socat (the listener + the ingress client), and a static LAN address
# -- 192.168.1.2 on eth1, mirroring the runNixOSTest VLAN (host .1 / world .2). The interactive world
# grows from this base via ./interactive.nix (9p, mgmt NIC, dev user) + the dev tooling carve-out.
{ pkgs, lib, ... }:
let
  # peer-echo: the egress masquerade observer -- a forking TCP listener on :8443 that replies with
  # the source address it sees (SOCAT_PEERADDR). An egress connect reads this back to verify the
  # source was masqueraded to the host edge (EG-2), not the container's 10.x. Wrapped in a script
  # (not an inline ExecStart) so the socat SYSTEM quoting + $-expansion survive systemd's parser.
  peerEcho = pkgs.writeShellScript "peer-echo" ''
    exec ${pkgs.socat}/bin/socat TCP-LISTEN:8443,reuseaddr,fork SYSTEM:'printf "%s" "$SOCAT_PEERADDR"'
  '';
in
{
  system.stateVersion = "25.05";
  networking.firewall.enable = false; # so the masqueraded egress / ingress connections land

  # socat: the peer-echo listener (below) + the ingress client (L4 IN-1/IN-3).
  environment.systemPackages = [ pkgs.socat ];

  systemd.services.peer-echo = {
    description = "echo the source address a connecting client presents";
    wantedBy = [ "multi-user.target" ];
    serviceConfig = {
      ExecStart = peerEcho;
      Restart = "always";
    };
  };

  # Control plane: sshd with the committed key authorized for root -- the check dials `root@world`;
  # the interactive dev VM dials `dev@world` (the dev user comes from ./interactive.nix).
  services.openssh.enable = true;
  services.openssh.settings.PermitRootLogin = "prohibit-password";
  users.users.root.openssh.authorizedKeys.keyFiles = [ ./testvm_key.pub ];

  # A static address on the shared LAN (eth1), alongside the host's br-lan (192.168.1.1). Reachable
  # from a host container's veth-bridge link too. Mirrors the test net; clears the address
  # runNixOSTest would auto-assign so this networkd config wins (no-op in the dev VM).
  networking.useNetworkd = true;
  networking.useDHCP = false;
  networking.interfaces.eth1.ipv4.addresses = lib.mkForce [ ];
  networking.extraHosts = "192.168.1.1 host"; # resolve the host by name (symmetry / convenience)
  systemd.network.networks."21-eth1-lan" = {
    matchConfig.Name = "eth1";
    address = [ "192.168.1.2/24" ];
    networkConfig.ConfigureWithoutCarrier = true;
    linkConfig.RequiredForOnline = "no";
  };
}
