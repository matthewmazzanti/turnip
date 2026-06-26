# nix/demo/demo-script.nix -- `turnip-demo`, the guided tour run from the booted demo VM. It pokes
# the live fabric to SHOW the two things plain podman can't do: directional, default-deny L3 flow
# control, and a real host-LAN link. Everything is driven through `turnip probe <ctr> -- <cmd>`
# (which runs a command inside a container's netns), so what you see is the actual dataplane.
#
# Returns a package exposing `turnip-demo` (pass --auto/-y to skip the pauses).
{ pkgs, lib, turnipBin }:
let
  turnip = "${turnipBin}/bin/turnip";
in
pkgs.writeShellApplication {
  name = "turnip-demo";
  runtimeInputs = [ pkgs.python3 pkgs.iproute2 pkgs.iputils pkgs.nftables ];
  text = ''
    # turnip is rootful, so every probe goes through sudo. Absolute store path => no PATH/secure_path
    # surprises.
    TURNIP=${lib.escapeShellArg turnip}

    bold=$'\e[1m'; red=$'\e[31m'; grn=$'\e[32m'; ylw=$'\e[33m'; dim=$'\e[2m'; rst=$'\e[0m'

    AUTO=0
    [ "''${1:-}" = "--auto" ] || [ "''${1:-}" = "-y" ] && AUTO=1

    pause() { [ "$AUTO" = 1 ] && return 0; printf '\n%s' "''${dim}-- press enter to continue --''${rst}"; read -r _; }
    section() { printf '\n%s\n%s\n' "''${bold}== $* ==''${rst}" "''${dim}-----------------------------------------------------------------''${rst}"; }
    # show <ctr> <cmd...> : echo the probe, then run it inside the container's netns.
    show() {
      local ctr=$1; shift
      printf '%s$ sudo turnip probe %s -- %s%s\n' "$dim" "$ctr" "$*" "$rst"
      sudo "$TURNIP" probe "$ctr" -- "$@" || true
    }

    # connect <ctr> <ip> <port> : TCP-connect (3s timeout) from inside <ctr>'s netns. Returns the
    # probe's own exit code -- 0 if the router forwarded the SYN, non-zero if it dropped it.
    connect() {
      sudo "$TURNIP" probe "$1" -- \
        python3 -c 'import socket,sys; socket.create_connection((sys.argv[1],int(sys.argv[2])),3)' \
        "$2" "$3" >/dev/null 2>&1
    }
    # expect <allow|deny> <label> <ctr> <ip> <port>
    expect() {
      local want=$1 label=$2 ctr=$3 ip=$4 port=$5
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
      local want=$1 label=$2 ctr=$3 ip=$4
      printf '  %-34s ' "$label"
      if sudo "$TURNIP" probe "$ctr" -- ping -c1 -W2 "$ip" >/dev/null 2>&1; then got=yes; else got=no; fi
      if [ "$got" = "$want" ]; then
        if [ "$want" = yes ]; then printf '%sreachable%s\n' "$grn" "$rst";
        else printf '%sunreachable (no link / dead default route)%s\n' "$grn" "$rst"; fi
      else
        printf '%sUNEXPECTED: got %s, wanted %s%s\n' "$red" "$got" "$want" "$rst"
      fi
    }

    cat <<EOF

    ''${bold}turnip demo''${rst} -- a routed L3 podman fabric, deployed from Nix.

        zwave (10.0.0.11) --tcp/443--> hass (10.0.0.12) --tcp/443--> proxy (10.0.0.13)
                                        │
                                        └─ veth link ─> br-lan ''${bold}192.168.1.50/24''${rst} (the host LAN)

    Three podman containers (quadlet-nix units) attach to turnip netns. Flows are ''${bold}directional''${rst}
    and ''${bold}default-deny''${rst}: only zwave->hass and hass->proxy are allowed. hass also hangs a veth into
    the host bridge, so it has a real LAN address. Plain podman can express ''${ylw}neither''${rst}.
    EOF
    pause

    section "1. Directional, default-deny flow control"
    echo "Each line TCP-connects to :443 from inside one container's netns. The router forwards"
    echo "only what a flow permits; everything else is dropped (you'll see the denied ones hang ~3s)."
    echo
    expect allow "zwave -> hass:443   (flow exists)"  zwave 10.0.0.12 443
    expect deny  "zwave -> proxy:443  (no flow)"      zwave 10.0.0.13 443
    expect allow "hass  -> proxy:443  (flow exists)"  hass  10.0.0.13 443
    expect deny  "proxy -> hass:443   (no reverse)"   proxy 10.0.0.12 443
    echo
    echo "''${ylw}podman can't do this:''${rst} a podman network is one open trust domain -- every container in"
    echo "it can reach every other. There's no per-pair, per-port, directional deny."
    pause

    section "2. See the policy that enforces it"
    echo "The forward matrix lives as nftables IN the router netns (removing the netns is the whole"
    echo "teardown). Here it is, live:"
    echo
    show router:lan nft list table inet turnip
    pause

    section "3. A real host-LAN link (the L2 escape)"
    echo "hass has a second interface: a veth into the host bridge br-lan. So it holds a real LAN IP"
    echo "and reaches the host directly -- outside every router's policy."
    echo
    show hass ip -br addr
    echo
    reach yes "hass  -> host 192.168.1.1 (has link)"  hass  192.168.1.1
    reach no  "zwave -> host 192.168.1.1 (no link)"   zwave 192.168.1.1
    echo
    echo "''${ylw}podman can't do this:''${rst} a rootless podman container can't be handed a routable address on"
    echo "the host's LAN segment; turnip's veth link bridges it straight onto br-lan."
    pause

    section "4. Routed, not bridged (no shared L2)"
    echo "Each container's only neighbour is the router; it reaches peers by their /32 via the"
    echo "gateway. There is no shared broadcast domain, so no inter-container ARP to spoof."
    echo
    show zwave ip route
    echo
    printf '%sDone.%s Explore by hand:  sudo turnip probe <container> -- <cmd...>\n' "$bold" "$rst"
    printf '       containers: zwave hass proxy   routers: router:lan\n'
  '';
}
