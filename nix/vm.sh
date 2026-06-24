#!/usr/bin/env bash
# vm.sh -- the single source of dev-VM launch + control logic (the justfile wraps this). All VM
# state -- disks, console logs, QMP sockets -- lives in vm/ (gitignored). No virt-manager/libvirt:
# headless VMs run qemu detached with the serial console to a logfile and a QMP control socket, so
# scripts/agents can drive them across sessions.
#
#   nix/vm.sh run   host|world      foreground interactive boot (serial console; Ctrl-a x to quit)
#   nix/vm.sh up    host|world      headless detached boot (console -> vm/<role>.log; QMP -> vm/<role>.sock)
#   nix/vm.sh log   host|world [n]  tail the console log (default 40 lines)
#   nix/vm.sh qmp   host|world '<json>'   send one QMP command
#   nix/vm.sh stop  host|world      graceful ACPI powerdown
#   nix/vm.sh reset host|world      reboot (QMP system_reset)
#   nix/vm.sh pid   host|world      the qemu pid (by disk), or empty
#   nix/vm.sh ready host|world      poll ssh until the VM accepts a login
#   nix/vm.sh fresh host|world      delete the disk (next boot is a clean slate)
#
# Headless detail: the run script is -nographic (serial+monitor mux'd on stdio), so a plain
# backgrounded launch dies on monitor EOF -- `up` parks `sleep infinity` on qemu's stdin and uses
# the QMP socket as the real control channel. The LAN is a UDP point-to-point socket on loopback
# (eth1): the two VMs cross-send (each `udp=` the other's `localaddr=`), deterministic for two
# local instances unlike a mcast socket.
set -euo pipefail
root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
state="$root/vm"

cmd=${1:?usage: vm.sh <run|up|log|qmp|stop|reset|pid|fresh> <host|world> [arg]}
role=${2:?role: host|world}
case "$role" in
  host)  port=2222; mac=52:54:00:00:50:10; lan="udp=127.0.0.1:52001,localaddr=127.0.0.1:52000" ;;
  world) port=2223; mac=52:54:00:00:50:20; lan="udp=127.0.0.1:52000,localaddr=127.0.0.1:52001" ;;
  *) echo "unknown role: $role (want host|world)" >&2; exit 1 ;;
esac
disk="$state/$role.qcow2"
log="$state/$role.log"
sock="$state/$role.sock"
mkdir -p "$state"

qopts="-virtfs local,path=$root,security_model=mapped-xattr,mount_tag=turnip"
qopts+=" -netdev socket,id=lan,$lan -device virtio-net-pci,netdev=lan,mac=$mac"

runbin() { ls "$(nix build --no-link --print-out-paths "$root#$role")"/bin/run-*-vm; }
# qmp: negotiate capabilities, then send the one command; replies ignored (fire-and-forget).
qmp() { printf '%s\n%s\n' '{"execute":"qmp_capabilities"}' "$1" | socat -t1 - "UNIX-CONNECT:$sock" >/dev/null 2>&1; }

case "$cmd" in
  run)
    QEMU_OPTS="$qopts" NIX_DISK_IMAGE="$disk" exec "$(runbin)" ;;
  up)
    bin=$(runbin); rm -f "$sock"
    QEMU_OPTS="$qopts -qmp unix:$sock,server,nowait" NIX_DISK_IMAGE="$disk" \
      setsid bash -c "sleep infinity | exec '$bin'" >"$log" 2>&1 </dev/null &
    disown || true
    echo "$role: booting detached -> $log (control: nix/vm.sh {log,stop,reset} $role)" ;;
  log)   tail -n "${3:-40}" "$log" ;;
  qmp)   qmp "${3:?json arg}" ;;
  pid)   pgrep -f "file=$disk" || true ;;
  ready) # poll ssh until the VM accepts a login (for scripted waits after `up`)
    for _ in $(seq 1 40); do
      if timeout 6 ssh -i "$root/nix/testvm_key" -p "$port" -o StrictHostKeyChecking=no \
           -o UserKnownHostsFile=/dev/null -o ConnectTimeout=4 dev@localhost true 2>/dev/null; then
        echo "$role: ready"; exit 0
      fi
      sleep 2
    done
    echo "$role: not ready after timeout" >&2; exit 1 ;;
  stop)  qmp '{"execute":"system_powerdown"}'; echo "$role: ACPI powerdown sent" ;;
  reset) qmp '{"execute":"system_reset"}'; echo "$role: reset sent" ;;
  fresh) rm -f "$disk"; echo "$role: disk removed ($disk)" ;;
  *) echo "unknown cmd: $cmd (want run|up|log|qmp|stop|reset|pid|fresh)" >&2; exit 1 ;;
esac
