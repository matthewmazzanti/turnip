# turnip dev tasks. `just` runs recipes from the repo root (where this justfile
# lives), so the VM's relative 9p mount (path=.) and ./turnip.qcow2 both land here
# regardless of where you invoke from -- the checkout-independence convention.

# List recipes.
default:
    @just --list

# Run the pure unit/golden suite (host; integration tests skip without TURNIP_INTEGRATION).
test:
    uv run pytest

# Run the integration scenarios in the running dev VM -- the fast loop. Pass pytest args,
# e.g. `just itest -k links`. needs_world / needs_image scenarios self-skip here.
# (Boot the VM first: `just vm`.)
itest *args:
    nix/ssh-vm.sh dev "sudo env TURNIP_INTEGRATION=1 PYTHONDONTWRITEBYTECODE=1 python3 -m pytest -p no:cacheprovider -v /mnt/turnip/tests/integration {{args}}"

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
