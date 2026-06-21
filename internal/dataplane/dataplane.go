// Package dataplane builds the routed L3 fabric inside the netns the bootstrap created --
// the gateway, the /32 routed veths, the routes (and, later, sysctls + the nft flow
// matrix). It operates on netns by FD: the rootful parent holds the fds (internal/netns'
// Set) and drives netlink against each via a netns-bound handle. Decoupled from config --
// the caller walks the model and passes concrete fds + names + addresses.
//
// Ports old/src/turnip/main.py: CreateGateway = create_gateway, Connect = connect.
package dataplane

import (
	"fmt"
	"net"
	"net/netip"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
)

const hostPrefix = 32 // the routed /32 model -- a real local address per node

// Gateway is a router netns's virtual gateway: a dummy device holding <Addr>/32, answered
// by the normal ARP responder (no uplink needed to make it "real").
type Gateway struct {
	IfName string     // the dummy iface name (e.g. "gw0")
	Addr   netip.Addr // the gateway address
}

// Endpoint is one container's attachment to a router: the /32 routed veth between them.
type Endpoint struct {
	RouterIf string     // router-side veth name (e.g. "vethR-zwave")
	ContIf   string     // container-side iface name (from the attachment)
	IP       netip.Addr // the container's /32 on this network
	Default  bool       // owns the container's default route (0.0.0.0/0 via the gateway)
}

// CreateGateway adds, addresses (/32), and brings up the dummy gateway in the router netns.
// The netns is freshly bootstrapped, so this just builds -- no existence checks.
func CreateGateway(routerFd int, gw Gateway) error {
	h, err := netlink.NewHandleAt(netns.NsHandle(routerFd))
	if err != nil {
		return fmt.Errorf("router netlink handle: %w", err)
	}
	defer h.Close()

	if err := h.LinkAdd(&netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: gw.IfName}}); err != nil {
		return fmt.Errorf("add gateway %s: %w", gw.IfName, err)
	}
	link, err := h.LinkByName(gw.IfName) // re-fetch for the kernel-assigned index
	if err != nil {
		return fmt.Errorf("find gateway %s: %w", gw.IfName, err)
	}
	if err := h.AddrAdd(link, &netlink.Addr{IPNet: host32(gw.Addr)}); err != nil {
		return fmt.Errorf("address gateway %s: %w", gw.Addr, err)
	}
	if err := h.LinkSetUp(link); err != nil {
		return fmt.Errorf("set gateway %s up: %w", gw.IfName, err)
	}
	return nil
}

// Connect wires one endpoint's container netns to its router with a /32 routed veth.
//
// The router end stays in the router netns; the container end is born DIRECTLY in the
// container netns (PeerNamespace = its fd, so it can't drift). Router side: the single /32
// device route that is both the forwarding entry and rp_filter's reverse-path anchor.
// Container side: /32 address, an explicit link-scope route to the gateway (nothing is
// on-link under /32), then -- only if this endpoint owns the container's default route --
// default via the gateway. Both netns are freshly bootstrapped, so this just builds.
func Connect(routerFd, contFd int, gateway netip.Addr, ep Endpoint) error {
	rh, err := netlink.NewHandleAt(netns.NsHandle(routerFd))
	if err != nil {
		return fmt.Errorf("router netlink handle: %w", err)
	}
	defer rh.Close()
	ch, err := netlink.NewHandleAt(netns.NsHandle(contFd))
	if err != nil {
		return fmt.Errorf("container netlink handle: %w", err)
	}
	defer ch.Close()

	// veth: router end in the router netns, peer (container end) born directly in the
	// container netns via PeerNamespace (IFLA_NET_NS_FD).
	veth := &netlink.Veth{
		LinkAttrs:     netlink.LinkAttrs{Name: ep.RouterIf},
		PeerName:      ep.ContIf,
		PeerNamespace: netlink.NsFd(contFd),
	}
	if err := rh.LinkAdd(veth); err != nil {
		return fmt.Errorf("add veth %s<->%s: %w", ep.RouterIf, ep.ContIf, err)
	}

	// router end: up + THE /32 device route (reach-this-container AND legit-source anchor).
	rlink, err := rh.LinkByName(ep.RouterIf)
	if err != nil {
		return fmt.Errorf("find router veth %s: %w", ep.RouterIf, err)
	}
	if err := rh.LinkSetUp(rlink); err != nil {
		return fmt.Errorf("set %s up: %w", ep.RouterIf, err)
	}
	if err := rh.RouteAdd(&netlink.Route{
		LinkIndex: rlink.Attrs().Index, Dst: host32(ep.IP), Scope: netlink.SCOPE_LINK,
	}); err != nil {
		return fmt.Errorf("router route %s dev %s: %w", ep.IP, ep.RouterIf, err)
	}

	// container end: index unknowable in advance; LinkAdd returns once the kernel made both
	// ends, so the peer is there.
	clink, err := ch.LinkByName(ep.ContIf)
	if err != nil {
		return fmt.Errorf("find container iface %s: %w", ep.ContIf, err)
	}
	if err := ch.AddrAdd(clink, &netlink.Addr{IPNet: host32(ep.IP)}); err != nil {
		return fmt.Errorf("address %s %s: %w", ep.ContIf, ep.IP, err)
	}
	if err := ch.LinkSetUp(clink); err != nil {
		return fmt.Errorf("set %s up: %w", ep.ContIf, err)
	}
	// /32 => the gateway is not on-link; pin it link-scope, then default via it iff this
	// endpoint owns the container's default route.
	if err := ch.RouteAdd(&netlink.Route{
		LinkIndex: clink.Attrs().Index, Dst: host32(gateway), Scope: netlink.SCOPE_LINK,
	}); err != nil {
		return fmt.Errorf("container gateway route %s: %w", gateway, err)
	}
	if ep.Default {
		if err := ch.RouteAdd(&netlink.Route{
			LinkIndex: clink.Attrs().Index, Gw: gateway.AsSlice(),
		}); err != nil {
			return fmt.Errorf("container default route via %s: %w", gateway, err)
		}
	}
	return nil
}

// host32 turns a netip.Addr into the <addr>/32 *net.IPNet vishvananda/netlink wants.
func host32(a netip.Addr) *net.IPNet {
	return &net.IPNet{IP: a.AsSlice(), Mask: net.CIDRMask(hostPrefix, 32)}
}
