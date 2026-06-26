# tour.sh -- the body of `turnip-demo`, the guided tour run from the booted demo VM. It pokes the
# live fabric to SHOW what plain podman can't express: directional, default-deny L3 flow control; a
# flow-scoped /etc/hosts; and a real host-LAN link. Everything goes through `turnip probe <ctr> --
# <cmd>` (run a command inside a container's netns), so what you see is the actual dataplane.
#
# This file is NOT standalone -- nix/demo/homelab.nix wraps it with pkgs.writeShellApplication, which
# supplies the shebang, `set -euo pipefail`, and the inspection tools (python3/ip/ping/nft) on PATH.
# Pass --auto/-y to skip the pauses. turnip is rootful, so probes go through sudo; the wrapped binary
# is installed as `turnip` (config baked), resolved via sudo's secure_path.

# Run from a world-traversable cwd so `sudo -u homelab ...` (the podman exec below) can chdir.
cd /

bold=$'\e[1m'; red=$'\e[31m'; grn=$'\e[32m'; ylw=$'\e[33m'; dim=$'\e[2m'; rst=$'\e[0m'

AUTO=0
[ "${1:-}" = "--auto" ] || [ "${1:-}" = "-y" ] && AUTO=1

pause() { [ "$AUTO" = 1 ] && return 0; printf '\n%s' "${dim}-- press enter to continue --${rst}"; read -r _; }
section() { printf '\n%s\n%s\n' "${bold}== $* ==${rst}" "${dim}-----------------------------------------------------------------${rst}"; }

# show <ctr> <cmd...> : echo the probe, then run it inside the container's netns.
show() {
  local ctr=$1; shift
  printf '%s$ sudo turnip probe %s -- %s%s\n' "$dim" "$ctr" "$*" "$rst"
  sudo turnip probe "$ctr" -- "$@" || true
}

# rootless podman exec into a running container (as its owner, homelab).
pexec() { sudo -u homelab env XDG_RUNTIME_DIR=/run/user/1001 HOME=/home/homelab podman exec "$@"; }

# connect <ctr> <dest> <port> : TCP-connect (3s timeout) from inside <ctr>'s netns to <dest> (an IP
# or a name resolved via the container's own /etc/hosts when run through pexec). Returns the probe's
# exit code -- 0 if the router forwarded the SYN, non-zero if it dropped it.
connect() {
  sudo turnip probe "$1" -- \
    python3 -c 'import socket,sys; socket.create_connection((sys.argv[1],int(sys.argv[2])),3)' \
    "$2" "$3" >/dev/null 2>&1
}
# expect <allow|deny> <label> <ctr> <ip> <port>
expect() {
  local want=$1 label=$2 ctr=$3 ip=$4 port=$5 got
  printf '  %-34s ' "$label"
  if connect "$ctr" "$ip" "$port"; then got=allow; else got=deny; fi
  if [ "$got" = "$want" ]; then
    if [ "$want" = allow ]; then printf '%sconnected   (allowed)%s\n' "$grn" "$rst";
    else printf '%sdropped     (denied by default-deny)%s\n' "$grn" "$rst"; fi
  else
    printf '%sUNEXPECTED: got %s, wanted %s%s\n' "$red" "$got" "$want" "$rst"
  fi
}
# reach <yes|no> <label> <ctr> <ip>
reach() {
  local want=$1 label=$2 ctr=$3 ip=$4 got
  printf '  %-34s ' "$label"
  if sudo turnip probe "$ctr" -- ping -c1 -W2 "$ip" >/dev/null 2>&1; then got=yes; else got=no; fi
  if [ "$got" = "$want" ]; then
    if [ "$want" = yes ]; then printf '%sreachable%s\n' "$grn" "$rst";
    else printf '%sunreachable (no link / dead default route)%s\n' "$grn" "$rst"; fi
  else
    printf '%sUNEXPECTED: got %s, wanted %s%s\n' "$red" "$got" "$want" "$rst"
  fi
}

cat <<EOF

${bold}turnip demo${rst} -- a routed L3 podman fabric, deployed from Nix.

    proxy (10.0.0.13) ─tcp/443─> hass (10.0.0.12) ─tcp/443─> zwave (10.0.0.11)
        └──────────────────tcp/443──────────────────────────────^   hass also:
                                                                     veth link ─> br-lan ${bold}192.168.1.50/24${rst}

Three podman containers (quadlet-nix units) attach to turnip netns. Flows are ${bold}directional${rst}
and ${bold}default-deny${rst}: proxy reaches hass + zwave, hass reaches zwave, and zwave initiates
${bold}nothing${rst} (a contained IoT device). hass also has a real LAN address. Plain podman: ${ylw}neither${rst}.
EOF
pause

section "1. Directional, default-deny flow control"
echo "Each line TCP-connects to :443 from inside one container's netns. The router forwards only"
echo "what a flow permits; everything else is dropped (the denied ones hang ~3s, then time out)."
echo
expect allow "proxy -> hass:443   (flow exists)"  proxy 10.0.0.12 443
expect allow "proxy -> zwave:443  (flow exists)"  proxy 10.0.0.11 443
expect allow "hass  -> zwave:443  (flow exists)"  hass  10.0.0.11 443
expect deny  "zwave -> hass:443   (no flow)"      zwave 10.0.0.12 443
expect deny  "hass  -> proxy:443  (no reverse)"   hass  10.0.0.13 443
echo
echo "${ylw}podman can't do this:${rst} a podman network is one open trust domain -- every container in"
echo "it can reach every other. There's no per-pair, per-port, directional deny."
pause

section "2. See the policy that enforces it"
echo "The forward matrix lives as nftables IN the router netns (removing the netns is the whole"
echo "teardown). Here it is, live:"
echo
show router:lan nft list table inet turnip
pause

section "3. A flow-scoped /etc/hosts, bind-mounted into each container"
echo "turnip generates each container an /etc/hosts listing ONLY the peers its flows allow"
echo "(directional), and the demo bind-mounts it in. proxy reaches hass + zwave, so it gets both:"
echo
printf '%s$ podman exec proxy cat /etc/hosts%s\n' "$dim" "$rst"
pexec proxy python3 -c 'print(open("/etc/hosts").read().rstrip())' 2>&1 || true
echo
echo "zwave initiates nothing, so its hosts has no peers. Names resolve inside the container --"
echo "proxy connects to hass ${bold}by name${rst}:"
printf '%s$ podman exec proxy python3 -c "...connect((\"hass\",443))"%s ' "$dim" "$rst"
if pexec proxy python3 -c 'import socket; socket.create_connection(("hass",443),3)' 2>/dev/null; then
  printf '%sconnected (name resolved via the mounted hosts file)%s\n' "$grn" "$rst"
else
  printf '%sname/connect failed%s\n' "$red" "$rst"
fi
pause

section "4. A real host-LAN link (the L2 escape)"
echo "hass has a second interface: a veth into the host bridge br-lan. So it holds a real LAN IP"
echo "and reaches the host directly -- outside every router's policy."
echo
show hass ip -br addr
echo
reach yes "hass  -> host 192.168.1.1 (has link)"  hass  192.168.1.1
reach no  "zwave -> host 192.168.1.1 (no link)"   zwave 192.168.1.1
echo
echo "${ylw}podman can't do this:${rst} a rootless podman container can't be handed a routable address on"
echo "the host's LAN segment; turnip's veth link bridges it straight onto br-lan."
pause

section "5. Routed, not bridged (no shared L2)"
echo "Each container's only neighbour is the router; it reaches peers by their /32 via the"
echo "gateway. There is no shared broadcast domain, so no inter-container ARP to spoof."
echo
show zwave ip route
echo
printf '%sDone.%s Explore by hand:  sudo turnip probe <container> -- <cmd...>\n' "$bold" "$rst"
printf '       containers: zwave hass proxy   routers: router:lan\n'
