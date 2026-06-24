#!/usr/bin/env bash
# vm.sh -- launch + control the dev VMs HEADLESS (no virt-manager / libvirt): qemu detached via
# setsid, with the serial console to a logfile and a QMP control socket, so scripts/agents can
# drive them across sessions. Interactive foreground boots stay in the justfile (`just host` /
# `just world`); this is the managed variant.
#
#   nix/vm.sh up    host|world      boot detached (console -> .vm-<role>.log; QMP -> .vm-<role>.sock)
#   nix/vm.sh log   host|world [n]  tail the console log (default 40 lines)
#   nix/vm.sh qmp   host|world '<json>'   send one QMP command
#   nix/vm.sh stop  host|world      graceful ACPI powerdown
#   nix/vm.sh reset host|world      reboot (QMP system_reset)
#   nix/vm.sh pid   host|world      the qemu pid (by disk), or empty
#
# Why detached + stdin parked: the run script is `-nographic` (serial+monitor mux'd on stdio), so
# a plain backgrounded launch gets monitor EOF and dies. A parked `sleep infinity` on qemu's stdin
# keeps it open; the QMP socket is the real control channel.
set -euo pipefail
root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)

cmd=${1:?usage: vm.sh <up|log|qmp|stop|reset|pid> <host|world> [arg]}
role=${2:?role: host|world}
case "$role" in
  host)  disk=turnip.qcow2;       mac=52:54:00:00:50:10; lan="udp=127.0.0.1:52001,localaddr=127.0.0.1:52000" ;;
  world) disk=turnip-world.qcow2; mac=52:54:00:00:50:20; lan="udp=127.0.0.1:52000,localaddr=127.0.0.1:52001" ;;
  *) echo "unknown role: $role (want host|world)" >&2; exit 1 ;;
esac
log="$root/.vm-$role.log"
sock="$root/.vm-$role.sock"

# qmp: negotiate capabilities, then send the one command; replies are ignored (fire-and-forget).
qmp() { printf '%s\n%s\n' '{"execute":"qmp_capabilities"}' "$1" | socat -t1 - "UNIX-CONNECT:$sock" >/dev/null 2>&1; }

case "$cmd" in
  up)
    run=$(ls "$(nix build --no-link --print-out-paths "$root#$role")"/bin/run-*-vm)
    rm -f "$sock"
    QEMU_OPTS="-virtfs local,path=$root,security_model=mapped-xattr,mount_tag=turnip -netdev socket,id=lan,$lan -device virtio-net-pci,netdev=lan,mac=$mac -qmp unix:$sock,server,nowait" \
    NIX_DISK_IMAGE="$root/$disk" \
    setsid bash -c "sleep infinity | exec '$run'" >"$log" 2>&1 </dev/null &
    disown || true
    echo "$role: booting detached -> $log (control: nix/vm.sh {log,stop,reset} $role)"
    ;;
  log)   tail -n "${3:-40}" "$log" ;;
  qmp)   qmp "${3:?json arg}" ;;
  pid)   pgrep -f "file=$root/$disk" || true ;;
  stop)  qmp '{"execute":"system_powerdown"}'; echo "$role: ACPI powerdown sent" ;;
  reset) qmp '{"execute":"system_reset"}'; echo "$role: reset sent" ;;
  *) echo "unknown cmd: $cmd (want up|log|qmp|stop|reset|pid)" >&2; exit 1 ;;
esac
