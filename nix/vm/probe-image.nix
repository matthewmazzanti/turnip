# probe-image.nix -- the python netns-probe OCI image, registry-free (no pull -> hermetic). One
# image, built once and shared by both consumers:
#   - the hermetic check (flake.nix): `podman load`ed + run by TestPodmanRun, which execs `python3`
#     for a REAL `podman run --network ns:<pin>` connect probe against a turnip netns.
#   - the host dev VM (interactive.host): loaded into homelab's rootless store at boot for manual
#     container poking.
# Carries python3 (socket+sys -- all the connect probe needs) + the netns/diagnostic CLIs (for the
# manual poking); PATH is set so the container can invoke tools by name.
{ pkgs }:
let
  tools = pkgs.buildEnv {
    name = "turnip-probe-tools";
    paths = [ pkgs.python3Minimal pkgs.iproute2 pkgs.iputils pkgs.nftables ];
  };
in
pkgs.dockerTools.buildLayeredImage {
  name = "turnip-probe";
  tag = "latest";
  contents = [ tools ];
  config.Env = [ "PATH=${tools}/bin" ];
}
