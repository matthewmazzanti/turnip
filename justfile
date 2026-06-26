# turnip dev tasks. The dev-VM launch/control logic lives in nix/vm.sh (single source of truth);
# `just vm <cmd> <role>` delegates straight to it. VM state (disks, console logs, QMP sockets) lives
# in vm/ (gitignored). `just` runs from the repo root, so the VMs' 9p mount + vm/ land here.

# List recipes.
default:
    @just --list

# Delegate to nix/vm.sh: `just vm <cmd> <role>` (run|up|ready|ssh|log|qmp|snap|down|reset|pid|fresh).
vm *args:
    nix/vm.sh {{args}}

# Boot the HOST / WORLD dev VM interactively (foreground serial console; Ctrl-a x to quit).
host:
    nix/vm.sh run host
world:
    nix/vm.sh run world

# Boot the worked quadlet-nix + turnip example VM (nix/demo). Serial console; the demo user logs in
# automatically -- run `turnip-demo` for the guided tour. Ctrl-a x to quit. Disk persists in
# vm/demo.qcow2 (gitignored, like the other VMs).
demo:
    NIX_DISK_IMAGE=vm/demo.qcow2 nix run .#demo

# Boot on a fresh disk (clean slate), interactively.
host-fresh:
    nix/vm.sh fresh host
    nix/vm.sh run host
world-fresh:
    nix/vm.sh fresh world
    nix/vm.sh run world

# Bring both dev VMs up (idempotent) and wait until both accept ssh -- the env `just itest` needs.
up:
    # up returns immediately (qemu -daemonize), so both boot concurrently; then we wait on each.
    nix/vm.sh up host
    nix/vm.sh up world
    nix/vm.sh ready host
    nix/vm.sh ready world

down:
    nix/vm.sh down host || true
    nix/vm.sh down world || true

# Quick Go pass (gofmt + vet + unit tests), excluding the integration suite (which needs the dev VMs).
precommit:
    gofmt -w cmd internal
    go vet ./cmd/... ./internal/...
    go test ./cmd/... ./internal/...

# Static build (CGO off -> no libc/ld dep), so the binaries run in the dev VM straight from the 9p
# mount regardless of its nix userland. Native arch: the dev machine and the VM share the flake's
# system, so no cross-compile is needed (and none of the amd64-only assumption an explicit GOARCH bakes in).
vm_go := "CGO_ENABLED=0 go"

# Build turnip + the integration test binary (fixtures embedded) into vm/, exec'd in place via 9p.
[private]
itest-build:
    {{vm_go}} build -o vm/turnip ./cmd/turnip
    {{vm_go}} test -c -o vm/it.test ./test/integration

# Run the integration suite (boots both dev VMs first); args pass through (`just itest -test.run X`).
# TODO: `{{args}}` is unquoted (bare word-split). Switch to `set lists` + per-element `quote(args)`
# once nixpkgs ships just >= 1.54 (the flake pins 1.51; `set lists` landed in 1.54) and re-pin.
itest *args: itest-build up
    #!/usr/bin/env bash
    nix/vm.sh ssh host dev <<'EOF'
    sudo /mnt/turnip/vm/it.test \
        -test.v -test.parallel 8 \
        -turnip /mnt/turnip/vm/turnip \
        -world dev@world \
        -ssh-key /etc/turnip/ssh-key \
        -image localhost/turnip-probe:latest {{args}}
    EOF
