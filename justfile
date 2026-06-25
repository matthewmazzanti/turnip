# turnip dev tasks. The dev-VM launch/control logic lives in nix/vm.sh (single source of truth);
# these recipes are thin wrappers. VM state (disks, console logs, QMP sockets) lives in vm/
# (gitignored). `just` runs from the repo root, so the VMs' 9p mount + vm/ land here.

# List recipes.
default:
    @just --list

# Boot the HOST / WORLD dev VM interactively (foreground serial console; Ctrl-a x to quit).
host:
    nix/vm.sh run host
world:
    nix/vm.sh run world

# Boot on a fresh disk (clean slate), interactively.
host-fresh:
    nix/vm.sh fresh host
    nix/vm.sh run host
world-fresh:
    nix/vm.sh fresh world
    nix/vm.sh run world

# Headless control (detached; managed across sessions). role = host|world.
up role:
    nix/vm.sh up {{role}}
stop role:
    nix/vm.sh stop {{role}}
reset role:
    nix/vm.sh reset {{role}}
vmlog role:
    nix/vm.sh log {{role}}

# Static build (CGO off -> no libc/ld dep), so the binaries run in the dev VM straight from the 9p
# mount regardless of its nix userland. Native arch: the dev machine and the VM share the flake's
# system, so no cross-compile is needed (and none of the amd64-only assumption an explicit GOARCH bakes in).
vm_go := "CGO_ENABLED=0 go"

# Build turnip + the integration test binary (fixtures embedded) into vm/. Visible in the VM at
# /mnt/turnip/vm/ via the 9p mount, so `just itest` execs them in place -- no scp.
[private]
itest-build:
    {{vm_go}} build -o vm/turnip ./cmd/turnip
    {{vm_go}} test -c -o vm/it.test ./test/integration

# Run the integration suite against the running dev VMs; args pass through (`just itest -test.run X`).
# The suite runs on the host node (drives world over the LAN) via ssh reading the heredoc on stdin.
itest *args: itest-build
    #!/usr/bin/env bash
    nix/ssh-vm.sh host dev <<'EOF'
    sudo /mnt/turnip/vm/it.test -test.v -test.parallel 8 \
        -turnip /mnt/turnip/vm/turnip \
        -world dev@world -ssh-key /etc/turnip/ssh-key \
        -image /etc/turnip/probe-image.tar.gz {{args}}
    EOF
