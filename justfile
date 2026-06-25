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

# Run the integration suite against the running dev VMs (host drives world over the LAN). Build the
# binaries into vm/ (fixtures embedded in it.test); they're reached in the VM via the 9p mount at
# /mnt/turnip/vm/ (no scp). Extra args pass through, e.g. `just itest -test.run TestL3Egress`.
itest *args:
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o vm/turnip ./cmd/turnip
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go test -c -o vm/it.test ./test/integration
    nix/ssh-vm.sh host dev 'sudo /mnt/turnip/vm/it.test -test.v -test.parallel 8 \
        -turnip /mnt/turnip/vm/turnip -world dev@world -ssh-key /etc/turnip/ssh-key \
        -image /etc/turnip/probe-image.tar.gz {{args}}'
