# tour.sh -- the body of `turnip-demo` (wrapped by writeShellApplication in homelab.nix). It runs as
# homelab (the rootless podman owner), so it pokes the live containers with plain `podman exec`: curl
# between them (the flow matrix) and ip/ping inside them (the link + routed topology). Each container
# runs an HTTP server answering "hello from <name>", so an allowed hop returns that banner and a
# denied one times out. Pass --auto/-y to skip the pauses.
#
# This file is NOT standalone -- writeShellApplication supplies the shebang, `set -euo pipefail`, and
# podman on PATH.

bold=$'\e[1m'; red=$'\e[31m'; grn=$'\e[32m'; ylw=$'\e[33m'; dim=$'\e[2m'; rst=$'\e[0m'

AUTO=0
[ "${1:-}" = "--auto" ] || [ "${1:-}" = "-y" ] && AUTO=1

pause() { [ "$AUTO" = 1 ] && return 0; printf '\n%s' "${dim}-- press enter to continue --${rst}"; read -r _; }
section() { printf '\n%s\n%s\n' "${bold}== $* ==${rst}" "${dim}-----------------------------------------------------------------${rst}"; }

# show <ctr> <cmd...> : echo the command, then run it inside the container.
show() {
  local ctr=$1; shift
  printf '%s$ podman exec %s %s%s\n' "$dim" "$ctr" "$*" "$rst"
  podman exec "$ctr" "$@" || true
}

# fetch <ctr> <peer> : curl the peer's HTTP server from inside <ctr> (by NAME, via the mounted
# /etc/hosts). Prints the body (the peer's "hello from <peer>") on success, nothing on a drop/timeout.
fetch() { podman exec "$1" curl -s --max-time 3 "http://$2:443/" 2>/dev/null; }

# expect <allow|deny> <label> <ctr> <peer>
expect() {
  local want=$1 label=$2 ctr=$3 peer=$4 body
  printf '  %-30s ' "$label"
  # `|| true`: a denied hop makes curl time out (exit 28); under `set -e` that would kill the tour.
  body=$(fetch "$ctr" "$peer") || true
  if [ -n "$body" ]; then
    if [ "$want" = allow ]; then printf '%s%s%s\n' "$grn" "$body" "$rst";
    else printf '%sUNEXPECTED: connected (%s)%s\n' "$red" "$body" "$rst"; fi
  else
    if [ "$want" = deny ]; then printf '%sdropped (default-deny)%s\n' "$grn" "$rst";
    else printf '%sUNEXPECTED: no response%s\n' "$red" "$rst"; fi
  fi
}

# reach <yes|no> <label> <ctr> <ip> <port> : can <ctr> open a TCP connection to <ip>:<port>? (A TCP
# probe, not ping -- ICMP would need NET_RAW, which podman's default cap set drops.)
reach() {
  local want=$1 label=$2 ctr=$3 ip=$4 port=$5 got
  printf '  %-30s ' "$label"
  if podman exec "$ctr" nc -z -w2 "$ip" "$port" >/dev/null 2>&1; then got=yes; else got=no; fi
  if [ "$got" = "$want" ]; then
    if [ "$want" = yes ]; then printf '%sreachable%s\n' "$grn" "$rst";
    else printf '%sunreachable (no link)%s\n' "$grn" "$rst"; fi
  else
    printf '%sUNEXPECTED: got %s%s\n' "$red" "$got" "$rst"
  fi
}

cat <<EOF

${bold}turnip demo${rst} -- a routed L3 podman fabric, deployed from Nix.

    proxy (10.0.0.13) ─tcp/443─> hass (10.0.0.12) ─tcp/443─> zwave (10.0.0.11)
        └──────────────────tcp/443──────────────────────────────^   hass also:
                                                                     veth link ─> br0 ${bold}192.168.1.50/24${rst}

Three netshoot containers (quadlet-nix units) attach to turnip netns; each runs an HTTP server
answering ${bold}"hello from <name>"${rst}. We poke them with \`podman exec ... curl\`. Flows are
${bold}directional${rst} + ${bold}default-deny${rst}: proxy->hass, proxy->zwave, hass->zwave are allowed; zwave
initiates nothing. hass also has a real LAN address. Plain podman can express ${ylw}neither${rst}.
EOF
pause

section "1. Directional, default-deny flow control"
echo "Each line curls a peer's HTTP server from inside one container (by NAME, via the mounted"
echo "/etc/hosts). An allowed hop returns the peer's banner; a denied hop is dropped (~3s timeout)."
echo
expect allow "proxy -> hass   (flow exists)"  proxy hass
expect allow "proxy -> zwave  (flow exists)"  proxy zwave
expect allow "hass  -> zwave  (flow exists)"  hass  zwave
expect deny  "zwave -> hass   (no flow)"      zwave hass
expect deny  "hass  -> proxy  (no reverse)"   hass  proxy
echo
echo "${ylw}podman can't do this:${rst} a podman network is one open trust domain -- every container in"
echo "it can reach every other. There's no per-pair, per-port, directional deny."
pause

section "2. A flow-scoped /etc/hosts, bind-mounted into each container"
echo "turnip generates each container an /etc/hosts with ONLY the peers its flows allow (directional)"
echo "-- which is why the curls above could use names. proxy reaches hass + zwave, so it gets both:"
echo
show proxy cat /etc/hosts
echo
echo "zwave initiates nothing, so its hosts lists no peers."
pause

section "3. A real host-LAN link (the L2 escape)"
echo "hass has a second interface: a veth into the VM bridge br0. So it holds a real LAN IP and"
echo "reaches the host on the LAN directly -- outside every router's policy. The host carries both"
echo "192.168.1.1 and .2 on br0; we hit its sshd (:22) on each."
echo
show hass ip -br addr
echo
reach yes "hass  -> 192.168.1.1:22 (has link)" hass  192.168.1.1 22
reach yes "hass  -> 192.168.1.2:22 (has link)" hass  192.168.1.2 22
reach no  "zwave -> 192.168.1.1:22 (no link)"  zwave 192.168.1.1 22
echo
echo "${ylw}podman can't do this:${rst} a rootless podman container can't be handed a routable address on"
echo "the host's LAN segment; turnip's veth link bridges it straight onto br0."
pause

section "4. Routed, not bridged (no shared L2)"
echo "Each container's only neighbour is the router; it reaches peers by their /32 via the gateway."
echo "There is no shared broadcast domain, so no inter-container ARP to spoof."
echo
show zwave ip route
echo
printf '%sDone.%s Poke it by hand:  podman exec <container> curl http://<peer>:443\n' "$bold" "$rst"
printf '       containers: zwave hass proxy\n'
