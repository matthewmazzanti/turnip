# turnip dev tasks. `just` runs recipes from the repo root (where this justfile lives), so the
# VMs' relative 9p mount (path=$PWD) and the ./*.qcow2 disks land here regardless of where you
# invoke from -- the checkout-independence convention.

# List recipes.
default:
    @just --list

# Boot a dev VM: build its run-*-vm and exec it with the 9p repo mount (absolute $PWD, injected
# here before the run script cd's to its temp dir) + a shared-LAN NIC on a qemu mcast socket --
# both VMs join the same group, forming one L2 segment (eth1 in the guest). MAC differs per VM.
# Serial console; Ctrl-a x to quit.
_boot attr disk mac:
    QEMU_OPTS="-virtfs local,path=$PWD,security_model=mapped-xattr,mount_tag=turnip -netdev socket,id=lan,mcast=230.0.0.1:1234 -device virtio-net-pci,netdev=lan,mac={{mac}}" \
    NIX_DISK_IMAGE="$PWD/{{disk}}" \
    exec "$(nix build --no-link --print-out-paths .#{{attr}})"/bin/run-*-vm

# Boot the HOST dev VM (the system under test) on ssh :2222, disk ./turnip.qcow2.
host:
    just _boot host turnip.qcow2 52:54:00:00:50:10

# Boot the WORLD dev VM (the LAN peer) on ssh :2223, disk ./turnip-world.qcow2.
world:
    just _boot world turnip-world.qcow2 52:54:00:00:50:20

# Boot the host VM on a fresh disk (clean slate).
host-fresh:
    rm -f turnip.qcow2
    just host

# Boot the world VM on a fresh disk (clean slate).
world-fresh:
    rm -f turnip-world.qcow2
    just world
