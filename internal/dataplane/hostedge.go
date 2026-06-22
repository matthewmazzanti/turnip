package dataplane

import (
	"fmt"
	"net"
	"net/netip"

	"git.lan/mmazzanti/turnip/internal/nftlib"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
)

// LinkPrefix is the uplink veth's point-to-point /31 (RFC 3021). Exported as the canonical
// topology constant (dataplane owns the wire prefixes; the display layer reads them here).
const LinkPrefix = 31

// Uplink is a network's host edge: a /31 veth between its router netns and the init (host)
// netns. HostIP is the init end (the router's gateway out), RouterIP the router end.
type Uplink struct {
	HostIf   string
	RouterIf string
	HostIP   netip.Addr // /31 base   -- init end
	RouterIP netip.Addr // /31 base+1 -- router end
}

// HostEdgeConnect wires the full init<->router edge -- every index-dependent netlink op the
// host edge needs. The uplink veth: host end born in the init netns, router end DIRECTLY in the
// router netns (PeerNamespace = the fd); host end addressed on the /31; router end addressed +
// up + default-routed via the host end, inside the router netns. Then, in the init netns, a /32
// route to each container via the router end (reach the container for ingress, and satisfy
// rp_filter for egress -- the reverse path to a container source is the uplink). Run from the
// init parent (root holds CAP_NET_ADMIN there, which the IFLA_NET_NS_FD move needs). The host
// sysctls + nat zone are pre-built artifacts pushed separately (WriteSysctls / nftlib.Load).
// Ports host_edge_connect + the route half of configure_host_nat.
func HostEdgeConnect(routerFd int, up Uplink, containerIPs []netip.Addr) error {
	veth := &netlink.Veth{
		LinkAttrs:     netlink.LinkAttrs{Name: up.HostIf},
		PeerName:      up.RouterIf,
		PeerNamespace: netlink.NsFd(routerFd),
	}
	if err := netlink.LinkAdd(veth); err != nil {
		return fmt.Errorf("add uplink veth %s<->%s: %w", up.HostIf, up.RouterIf, err)
	}
	host, err := netlink.LinkByName(up.HostIf)
	if err != nil {
		return fmt.Errorf("find uplink host end %s: %w", up.HostIf, err)
	}
	if err := netlink.AddrAdd(host, &netlink.Addr{IPNet: host31(up.HostIP)}); err != nil {
		return fmt.Errorf("address uplink host end %s: %w", up.HostIP, err)
	}
	if err := netlink.LinkSetUp(host); err != nil {
		return fmt.Errorf("set %s up: %w", up.HostIf, err)
	}

	rh, err := netlink.NewHandleAt(netns.NsHandle(routerFd))
	if err != nil {
		return fmt.Errorf("router netlink handle: %w", err)
	}
	defer rh.Close()
	r, err := rh.LinkByName(up.RouterIf)
	if err != nil {
		return fmt.Errorf("find uplink router end %s: %w", up.RouterIf, err)
	}
	if err := rh.AddrAdd(r, &netlink.Addr{IPNet: host31(up.RouterIP)}); err != nil {
		return fmt.Errorf("address uplink router end %s: %w", up.RouterIP, err)
	}
	if err := rh.LinkSetUp(r); err != nil {
		return fmt.Errorf("set %s up: %w", up.RouterIf, err)
	}
	if err := rh.RouteAdd(&netlink.Route{LinkIndex: r.Attrs().Index, Gw: up.HostIP.AsSlice()}); err != nil {
		return fmt.Errorf("router default route via %s: %w", up.HostIP, err)
	}

	// init-netns: a /32 route to each container via the router end. `host` is the init-side
	// veth end fetched above. (Reach the container for ingress; satisfy rp_filter for egress.)
	for _, ip := range containerIPs {
		route := &netlink.Route{LinkIndex: host.Attrs().Index, Dst: host32(ip), Gw: up.RouterIP.AsSlice()}
		if err := netlink.RouteAdd(route); err != nil {
			return fmt.Errorf("host route to %s: %w", ip, err)
		}
	}
	return nil
}

// DNAT is one published-port forward: traffic to Listen:HostPort (proto) is rewritten to
// ContIP:ContPort. An unspecified Listen (0.0.0.0) matches any host address.
type DNAT struct {
	Listen   netip.Addr
	Proto    string
	HostPort int
	ContIP   netip.Addr
	ContPort int
}

// BuildHostNFT renders the `ip turnip_host_<net>` zone in the init netns: postrouting
// masquerade for traffic forwarded IN from the uplink (egress SNAT, iif-matched -- the routed
// /32 model declares no subnet), and prerouting DNAT of each published host port to its
// container (ingress). The iif (egress) vs the DNAT's prerouting hook keep the two from
// colliding; each connection's NAT is decided on its first packet.
func BuildHostNFT(netName string, up Uplink, dnats []DNAT) nftlib.Ruleset {
	t := nftlib.Table{Family: "ip", Name: "turnip_host_" + netName}
	cmds := append(t.Reload(),
		t.Chain("postrouting", "nat", "postrouting", 100, "accept"),
		t.Rule("postrouting", nftlib.Match(nftlib.Meta("iifname"), up.HostIf), nftlib.Masquerade()),
	)
	if len(dnats) > 0 {
		cmds = append(cmds, t.Chain("prerouting", "nat", "prerouting", -100, "accept"))
		for _, d := range dnats {
			var exprs []nftlib.Node
			if d.Listen.IsValid() && !d.Listen.IsUnspecified() { // default 0.0.0.0 = any host address
				exprs = append(exprs, nftlib.Match(nftlib.Payload("ip", "daddr"), d.Listen.String()))
			}
			exprs = append(exprs,
				nftlib.Match(nftlib.Meta("l4proto"), d.Proto),
				nftlib.Match(nftlib.Payload("th", "dport"), d.HostPort),
				nftlib.Dnat(d.ContIP.String(), d.ContPort),
			)
			cmds = append(cmds, t.Rule("prerouting", exprs...))
		}
	}
	return nftlib.Rules(cmds...)
}

// TeardownHostEdge removes the init-netns host edge for a network: the host veth end (which
// dies with its peer, but the init end can survive, so delete by name idempotently) and the
// host nat zone. Best-effort + idempotent (up = down + build, and down). Ports the host-edge
// half of teardown_host_edge.
func TeardownHostEdge(netName, hostIf string) error {
	if link, err := netlink.LinkByName(hostIf); err == nil {
		if err := netlink.LinkDel(link); err != nil {
			return fmt.Errorf("remove uplink host veth %s: %w", hostIf, err)
		}
	}
	t := nftlib.Table{Family: "ip", Name: "turnip_host_" + netName}
	if err := nftlib.Load(nftlib.Rules(t.Add(), t.Delete())); err != nil {
		return fmt.Errorf("flush host nat %s: %w", netName, err)
	}
	return nil
}

func host31(a netip.Addr) *net.IPNet {
	return &net.IPNet{IP: a.AsSlice(), Mask: net.CIDRMask(LinkPrefix, 32)}
}
