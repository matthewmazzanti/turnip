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

# Run the integration suite against the running dev VMs (host drives world over the LAN). Build
# the binaries, stage to host, run with the baked key + the 9p-mounted fixtures. Extra args pass
# through, e.g. `just itest -test.run TestL3Egress`.
itest *args:
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o vm/turnip ./cmd/turnip
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go test -c -o vm/it.test ./test/integration
    scp -i nix/testvm_key -P 2222 -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
        vm/turnip vm/it.test dev@localhost:/tmp/
    nix/ssh-vm.sh host dev 'sudo /tmp/it.test -test.v -test.parallel 8 \
        -turnip /tmp/turnip -fixtures /mnt/turnip/test/integration/fixtures \
        -world dev@world -ssh-key /etc/turnip/ssh-key {{args}}'
