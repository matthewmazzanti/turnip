# NixOS dev VM for turnip -- the interactive rootful sandbox for the Go rewrite.
#
# A persistent qemu VM (boots under KVM) with the turnip repo mounted (9p, READ-ONLY) at
# /mnt/turnip. The flake's testVM stacks the layers (turnip-host -> this), so this module
# is ONLY the VM-specific bits: the 9p mount, ssh/console access, login users, and the Go
# toolchain (below). Rootless podman + nft/ip come from the turnip-host base.
#
# Build + run:
#   nix build .#vm
#   ./result/bin/run-turnip-vm          # boots qemu; persists ./turnip.qcow2
#
# Drive it:
#   ssh -i nix/testvm_key -p 2222 dev@localhost        # admin (sudo)
#   ssh -i nix/testvm_key -p 2222 homelab@localhost    # the rootless run user
#
# The 9p mount is READ-ONLY, so build out-of-tree, e.g.:
#   cp -r /mnt/turnip/<pkg> /tmp/b && cd /tmp/b && CGO_ENABLED=0 go build -o /tmp/turnip .
#
# A minimal OCI image (turnip-test:latest, name in $TURNIP_TEST_IMAGE) is loaded into
# homelab's rootless podman at boot, for the real container-attach test:
#   podman run --rm --network ns:/run/user/1001/turnip/containers/<c>/netns \
#     "$TURNIP_TEST_IMAGE" ip -br addr
# It's registry-free + root-owned (no large subuid range needed -- a pulled busybox fails
# the unpack on homelab's small autoSubUidGidRange; this image loads clean).
{ pkgs, ... }:
let
  # The container-attach test toolbox: python3 (scripted connect over a link), plus the
  # netns inspection/diagnostic CLIs (ip/ss, ping, nft). buildEnv symlinks every bin into
  # one /bin so a single PATH entry resolves them all.
  testTools = pkgs.buildEnv {
    name = "turnip-test-tools";
    paths = [
      pkgs.python3Minimal
      pkgs.iproute2 # ip, ss
      pkgs.iputils # ping
      pkgs.nftables # nft
    ];
  };
  # A registry-free OCI image for the container-attach test.
  testImage = pkgs.dockerTools.buildLayeredImage {
    name = "turnip-test";
    tag = "latest";
    contents = [ testTools ];
    config.Env = [ "PATH=${testTools}/bin" ];
  };
in
{
  networking.hostName = "turnip";

  # Go toolchain for building/running the rootful rewrite spike inside the VM
  # (spike/go-netns-bootstrap, live-mounted at /mnt/turnip/spike). VM-only: the
  # spike needs real root + podman, which is what this sandbox is for. Sources are
  # read-only (9p), so build out-of-tree, e.g. `go build -o /tmp/spike .` with
  # GOCACHE/GOPATH under $HOME (their defaults are already writable).
  environment.systemPackages = [ pkgs.go ];

  # Load the python test image into homelab's rootless store at boot, and name it in the
  # environment (derived from the image so it can't drift). A oneshot rather than a baked
  # layer because rootless podman's store lives under the user's $XDG_RUNTIME_DIR/$HOME,
  # which only exists once `homelab`'s user instance (linger) is up.
  environment.sessionVariables.TURNIP_TEST_IMAGE = "${testImage.imageName}:${testImage.imageTag}";
  systemd.services.turnip-test-image = {
    description = "load the python test OCI image into homelab's rootless podman";
    wantedBy = [ "multi-user.target" ];
    after = [ "user@1001.service" ];
    wants = [ "user@1001.service" ];
    serviceConfig = {
      Type = "oneshot";
      RemainAfterExit = true;
    };
    # rootless podman needs the setuid newuidmap/newgidmap (/run/wrappers/bin) + podman
    # itself (/run/current-system/sw/bin) -- the system PATH a unit otherwise lacks.
    script = ''
      until test -d /run/user/1001; do sleep 0.2; done
      export PATH=/run/wrappers/bin:/run/current-system/sw/bin
      runuser -u homelab -- env XDG_RUNTIME_DIR=/run/user/1001 HOME=/home/homelab PATH="$PATH" \
        podman load -i ${testImage}
    '';
  };

  # Admin user for console/ssh debugging.
  users.users.dev = {
    isNormalUser = true;
    extraGroups = [ "wheel" ];
    password = "dev"; # throwaway VM: console login dev/dev
    openssh.authorizedKeys.keyFiles = [ ./testvm_key.pub ];
  };
  # The shared module defines `homelab` (rootless podman owner); add VM login creds.
  users.users.homelab = {
    password = "homelab";
    openssh.authorizedKeys.keyFiles = [ ./testvm_key.pub ];
  };

  security.sudo.wheelNeedsPassword = false;
  services.openssh.enable = true;

  # Keep dhcpcd off the spare NIC (eth1, added below): it must stay unconfigured so it never
  # gains a default route -- a phys link refuses the default-route NIC, and an idle eth1 is
  # exactly what a borrowable physical device looks like. eth0 stays the uplink.
  networking.dhcpcd.denyInterfaces = [ "eth1" ];

  virtualisation = {
    memorySize = 2048;
    cores = 2;
    graphics = false; # serial console in the terminal; headless + ssh otherwise
    diskSize = 8192; # MB; room for podman images
    # A second virtio NIC (eth1) so phys links have a REAL-driver device to borrow -- one
    # the kernel auto-returns to init on netns teardown. A dummy/veth/vlan is virtual and
    # gets DELETED instead of returned, so it can't exercise the borrow/return doctrine; a
    # virtio-net device is non-netns-local and moves back. Its slirp net (10.9.0.0/24, no
    # default offered to the guest since dhcpcd ignores eth1) just keeps qemu happy.
    qemu.options = [
      "-netdev user,id=spare0,net=10.9.0.0/24"
      "-device virtio-net-pci,netdev=spare0,mac=52:54:00:12:34:57"
    ];
    forwardPorts = [
      {
        from = "host";
        host.port = 2222;
        guest.port = 22;
      }
    ];
    # Live-mount the host repo at /mnt/turnip over 9p (tag `turnip`), READ-ONLY.
    #
    # We declare only the GUEST mount here; the host side is supplied at LAUNCH by
    # `just vm` (QEMU_OPTS with an ABSOLUTE path). NOT `sharedDirectories.source`,
    # because the NixOS run script `cd`s into a temp dir before exec'ing qemu: a baked
    # relative "." would resolve there, and an absolute path would hardcode one
    # checkout. Injecting $PWD from the repo root at launch is correct + checkout-free.
    fileSystems."/mnt/turnip" = {
      device = "turnip"; # 9p mount tag; host path injected via QEMU_OPTS at launch
      fsType = "9p";
      neededForBoot = false;
      options = [
        "trans=virtio"
        "version=9p2000.L"
        "msize=16384"
        "x-systemd.requires=modprobe@9pnet_virtio.service"
        "ro"
        "nofail"
      ];
    };
  };
}
