#!/usr/bin/env bash
# vm.sh -- the single source of dev-VM launch + control logic (the justfile wraps this). All VM
# state -- disks, console logs, QMP sockets -- lives in vm/ (gitignored). No virt-manager/libvirt:
# headless VMs run qemu detached with the serial console to a logfile and a QMP control socket, so
# scripts/agents can drive them across sessions.
#
#   nix/vm.sh run    host|world     foreground interactive boot (serial console; Ctrl-a x to quit)
#   nix/vm.sh up     host|world     headless detached boot (console -> vm/<role>.log; QMP -> vm/<role>.sock)
#   nix/vm.sh ready  host|world     poll ssh until the VM accepts a login (compose with up to boot+wait)
#   nix/vm.sh log    host|world [n] tail the console log (default 40 lines)
#   nix/vm.sh qmp   host|world '<cmd>'    one qmp-shell command (e.g. query-status)
#   nix/vm.sh snap  host|world save|restore <name>   qcow2 internal snapshot (checkpoint/rollback)
#   nix/vm.sh stop  host|world      graceful ACPI powerdown
#   nix/vm.sh reset host|world      reboot (QMP system_reset)
#   nix/vm.sh pid   host|world      the qemu pid (by disk), or empty
#   nix/vm.sh fresh host|world      delete the disk (next boot is a clean slate)
#
# Detach detail: the VM is built with virtualisation.graphics=true, so the generated runner has no
# -nographic -- which lets `up` use qemu's own -daemonize (it forks, sets up the QMP socket, and the
# parent exits; no sleep/setsid/disown). The console/monitor are kept off stdio there (-serial file,
# -monitor none, control via QMP); `run` instead asks for -serial mon:stdio to get the interactive
# console. The LAN is a UDP point-to-point socket on loopback (eth1): the two VMs cross-send (each
# `udp=` the other's `localaddr=`), deterministic for two local instances unlike a mcast socket.
set -euo pipefail
root=$(git -C "$(dirname "${BASH_SOURCE[0]}")" rev-parse --show-toplevel)
state="$root/vm"

cmd=${1:?usage: vm.sh <run|up|ready|log|qmp|snap|stop|reset|pid|fresh> <host|world> [arg]}
role=${2:?role: host|world}
shift 2 # drop cmd + role; any remaining "$@" belong to the command (log lines, snap name, ...)
case "$role" in
  host)  mac=52:54:00:00:01:01; lan="udp=127.0.0.1:52001,localaddr=127.0.0.1:52000" ;;
  world) mac=52:54:00:00:01:02; lan="udp=127.0.0.1:52000,localaddr=127.0.0.1:52001" ;;
  *) echo "unknown role: $role (want host|world)" >&2; exit 1 ;;
esac
disk="$state/$role.qcow2"
log="$state/$role.log"
sock="$state/$role.sock"
mkdir -p "$state"

qopts="-virtfs local,path=$root,security_model=mapped-xattr,mount_tag=turnip"
qopts+=" -netdev socket,id=lan,$lan -device virtio-net-pci,netdev=lan,mac=$mac"

function runbin() {
    ls "$(nix build --no-link --print-out-paths "$root#$role")"/bin/run-*-vm
}

# qmp sends one command via qmp-shell (the canned client: negotiates capabilities, friendly
# `command arg=val` syntax). qmp-shell comes from the flake devShell (direnv).
function qmp() {
    echo "$1" | qmp-shell "$sock"
}

# running: true if a qemu is already live for this role's disk -- the run/up boot guard (two qemus
# on one qcow2 corrupt it), and what `pid` reports.
function running() {
    pgrep -f "file=$disk" >/dev/null 2>&1
}

case "$cmd" in
  run)
    # foreground interactive boot -- idempotent: refuse if one's already live (don't share a qcow2).
    # -serial mon:stdio puts the serial console + monitor on the terminal (Ctrl-a c monitor, x quit).
    if running; then
      echo "$role: already running -- connect with 'nix/ssh-vm.sh $role', or 'nix/vm.sh stop $role' first" >&2
      exit 1
    fi
    QEMU_OPTS="$qopts -display none -serial mon:stdio" NIX_DISK_IMAGE="$disk" exec "$(runbin)"
    ;;
  up)
    # headless detached boot -- idempotent: no-op if one's already live. qemu -daemonize self-forks
    # (parent returns once the QMP socket is up); console -> the log file, control -> the QMP socket.
    if running; then
      echo "$role: already up"
      exit 0
    fi
    rm -f "$sock"
    QEMU_OPTS="$qopts -qmp unix:$sock,server,nowait -display none -serial file:$log -monitor none -daemonize" \
      NIX_DISK_IMAGE="$disk" "$(runbin)"
    echo "$role: booting detached -> $log (control: nix/vm.sh {log,stop,reset} $role)"
    ;;
  ready)
    # poll ssh until the VM accepts a login (for scripted waits after `up`); reuse ssh-vm.sh
    for _ in $(seq 1 40); do
      if timeout 6 "$root/nix/ssh-vm.sh" "$role" dev true 2>/dev/null; then
        echo "$role: ready"
        exit 0
      fi
      sleep 2
    done
    echo "$role: not ready after timeout" >&2
    exit 1
    ;;
  log)
    tail -n "${1:-40}" "$log"
    ;;
  qmp)
    qmp "${1:?qmp-shell command, e.g. query-status}"
    ;;
  pid)
    pgrep -f "file=$disk" || true
    ;;
  snap)
    # snap <role> save|restore <name> -- qcow2 internal snapshot via the human monitor
    case "${1:?save|restore}" in
      save)
        qmp "human-monitor-command command-line=\"savevm ${2:?snapshot name}\""
        echo "$role: saved snapshot '$2'"
        ;;
      restore)
        qmp "human-monitor-command command-line=\"loadvm ${2:?snapshot name}\""
        echo "$role: restored snapshot '$2'"
        ;;
      *)
        echo "snap: want save|restore" >&2
        exit 1
        ;;
    esac
    ;;
  stop)
    qmp 'system_powerdown'
    echo "$role: ACPI powerdown sent"
    ;;
  reset)
    qmp 'system_reset'
    echo "$role: reset sent"
    ;;
  fresh)
    rm -f "$disk"
    echo "$role: disk removed ($disk)"
    ;;
  *)
    echo "unknown cmd: $cmd (want run|up|ready|log|qmp|snap|stop|reset|pid|fresh)" >&2
    exit 1
    ;;
esac
