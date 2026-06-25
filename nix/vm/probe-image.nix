# probe-image.nix -- builds the python netns-probe OCI image, registry-free (no pull -> hermetic).
# The payload for a REAL `podman run --network ns:<pin>` against a turnip netns (the operator path,
# vs the `turnip probe` shortcut the harness uses). python3Minimal has socket+sys, all the connect
# probe needs; PATH is set so the container can invoke tools by name.
#
# One builder, two consumers:
#   - the hermetic check (flake.nix): the minimal image, `podman load`ed + run by TestPodmanRun.
#   - the host dev VM (interactive.host in default.nix): + the netns/diagnostic CLIs for poking inside
#     a container, loaded into homelab's rootless store at boot.
#
#   mkProbeImage { pkgs, name ? "turnip-probe", extraTools ? [] } -> a layered image derivation
{ pkgs, name ? "turnip-probe", extraTools ? [ ] }:
let
  tools = pkgs.buildEnv {
    name = "${name}-tools";
    paths = [ pkgs.python3Minimal ] ++ extraTools;
  };
in
pkgs.dockerTools.buildLayeredImage {
  inherit name;
  tag = "latest";
  contents = [ tools ];
  config.Env = [ "PATH=${tools}/bin" ];
}
