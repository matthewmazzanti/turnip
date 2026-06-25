# peer-echo.nix -- the egress masquerade observer, as a shareable NixOS module: a forking TCP
# listener on :8443 that replies with the source address it sees (SOCAT_PEERADDR). An egress
# connect reads this back to verify the source was masqueraded to the host edge (EG-2), not the
# container's 10.x. Imported by both the world dev VM and the hermetic check's world node (the
# interactive.world / test.world stacks in nix/vm/default.nix) so the two stay in lockstep.
#
# The socat command is wrapped in a script (not an inline ExecStart) so the SYSTEM quoting +
# $-expansion survive -- systemd's ExecStart parser would otherwise mangle them.
{ pkgs, ... }:
let
  peerEcho = pkgs.writeShellScript "peer-echo" ''
    exec ${pkgs.socat}/bin/socat TCP-LISTEN:8443,reuseaddr,fork SYSTEM:'printf "%s" "$SOCAT_PEERADDR"'
  '';
in
{
  systemd.services.peer-echo = {
    description = "echo the source address a connecting client presents";
    wantedBy = [ "multi-user.target" ];
    serviceConfig = {
      ExecStart = peerEcho;
      Restart = "always";
    };
  };
}
