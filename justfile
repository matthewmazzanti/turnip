# turnip dev tasks. `just` runs recipes from the repo root (where this justfile
# lives), so the VM's relative 9p mount (path=.) and ./turnip.qcow2 both land here
# regardless of where you invoke from -- the checkout-independence convention.

# List recipes.
default:
    @just --list

# Boot the dev VM: build `.#vm` and exec its run-turnip-vm. Mounts THIS repo
# (read-only, 9p tag `turnip`) by injecting its absolute path -- captured here
# from the repo root, before the run script cd's to its temp dir, which is why a
# baked relative path can't work. Serial console; Ctrl-a x to quit.
vm:
    QEMU_OPTS="-virtfs local,path=$PWD,security_model=mapped-xattr,mount_tag=turnip" \
    exec "$(nix build --no-link --print-out-paths .#vm)/bin/run-turnip-vm"

# Boot the VM on a fresh disk (drops ./turnip.qcow2 first -- clean slate).
vm-fresh:
    rm -f turnip.qcow2
    just vm
