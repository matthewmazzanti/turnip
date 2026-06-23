package dataplane

import (
	"fmt"
	"net"
	"net/netip"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
)

// LinkSpec is one container veth link -- a host netdev hole into a container netns, the
// deliberate L2 trust escape (outside every router and its nft policy). Kind selects the
// anchor; the rest are the fields it needs. veth is virtual (owned, reaped with the netns).
type LinkSpec struct {
	Container string // for error messages
	Kind      string // "veth-bridge" | "veth-host"
	Name      string // container-side iface name

	HostIf string // veth host-side end name (veth-*)
	Bridge string // host bridge to enslave the veth host end (veth-bridge)

	Address netip.Prefix   // a real on-subnet address (not a /32)
	Gateway netip.Addr     // optional; default route via it iff Default
	Routes  []netip.Prefix // static routes
	Mac     string
	MTU     int
	Default bool // owns the container's default route
}

// ValidateLinkAnchors checks each link's host-side anchor in the live init netns (exists, right
// kind) BEFORE any netns is built, so a bad anchor fails fast. Anchors are BORROWED -- we only
// check, never create. Reads the kernel, so it runs in the preflight phase (after the pure
// buildPlan), not in lowering.
func ValidateLinkAnchors(specs []LinkSpec) error {
	for _, s := range specs {
		if err := validateAnchor(s); err != nil {
			return err
		}
	}
	return nil
}

func validateAnchor(s LinkSpec) error {
	switch s.Kind {
	case "veth-bridge":
		br, err := netlink.LinkByName(s.Bridge)
		if err != nil {
			return fmt.Errorf("%s: link bridge %q not found in host netns", s.Container, s.Bridge)
		}
		if br.Type() != "bridge" {
			return fmt.Errorf("%s: link anchor %q is kind %q, not a bridge", s.Container, s.Bridge, br.Type())
		}
	case "veth-host":
		// the root netns is always present -- no anchor
	}
	return nil
}

// LinkConnect creates the veth into the container netns and configures the in-container half.
// The peer is born directly in the container netns (IFLA_NET_NS_FD), the host end is enslaved
// to the bridge (veth-bridge) or left in the root netns (veth-host), then configured over the
// netns fd. Run from the init parent.
func LinkConnect(contFd int, s LinkSpec) error {
	switch s.Kind {
	case "veth-bridge", "veth-host":
		veth := &netlink.Veth{
			LinkAttrs:     netlink.LinkAttrs{Name: s.HostIf},
			PeerName:      s.Name,
			PeerNamespace: netlink.NsFd(contFd),
		}
		if err := netlink.LinkAdd(veth); err != nil {
			return fmt.Errorf("%s: add link veth %s: %w", s.Container, s.Name, err)
		}
		host, err := netlink.LinkByName(s.HostIf)
		if err != nil {
			return fmt.Errorf("%s: find link host end %s: %w", s.Container, s.HostIf, err)
		}
		if s.Kind == "veth-bridge" {
			br, err := netlink.LinkByName(s.Bridge)
			if err != nil {
				return fmt.Errorf("%s: find bridge %s: %w", s.Container, s.Bridge, err)
			}
			if err := netlink.LinkSetMaster(host, br); err != nil {
				return fmt.Errorf("%s: enslave %s to %s: %w", s.Container, s.HostIf, s.Bridge, err)
			}
		}
		if err := netlink.LinkSetUp(host); err != nil {
			return fmt.Errorf("%s: set %s up: %w", s.Container, s.HostIf, err)
		}
	default:
		return fmt.Errorf("%s: unknown link kind %q", s.Container, s.Kind)
	}

	return configureLinkIface(contFd, s)
}

// configureLinkIface is the in-container half: mac/mtu, the on-subnet address, up, static
// routes, and default-via-gateway iff this link owns the default route.
func configureLinkIface(contFd int, s LinkSpec) error {
	h, err := netlink.NewHandleAt(netns.NsHandle(contFd))
	if err != nil {
		return fmt.Errorf("%s: container netlink handle: %w", s.Container, err)
	}
	defer h.Close()

	link, err := h.LinkByName(s.Name)
	if err != nil {
		return fmt.Errorf("%s: find %s in container: %w", s.Container, s.Name, err)
	}
	if s.Mac != "" {
		mac, err := net.ParseMAC(s.Mac)
		if err != nil {
			return fmt.Errorf("%s: bad mac %q: %w", s.Container, s.Mac, err)
		}
		if err := h.LinkSetHardwareAddr(link, mac); err != nil {
			return fmt.Errorf("%s: set mac: %w", s.Container, err)
		}
	}
	if s.MTU != 0 {
		if err := h.LinkSetMTU(link, s.MTU); err != nil {
			return fmt.Errorf("%s: set mtu: %w", s.Container, err)
		}
	}
	if err := h.AddrAdd(link, &netlink.Addr{IPNet: prefixToIPNet(s.Address)}); err != nil {
		return fmt.Errorf("%s: address %s %s: %w", s.Container, s.Name, s.Address, err)
	}
	if err := h.LinkSetUp(link); err != nil {
		return fmt.Errorf("%s: set %s up: %w", s.Container, s.Name, err)
	}
	for _, rt := range s.Routes {
		if err := h.RouteAdd(&netlink.Route{LinkIndex: link.Attrs().Index, Dst: prefixToIPNet(rt)}); err != nil {
			return fmt.Errorf("%s: route %s: %w", s.Container, rt, err)
		}
	}
	if s.Default && s.Gateway.IsValid() {
		if err := h.RouteAdd(&netlink.Route{LinkIndex: link.Attrs().Index, Gw: s.Gateway.AsSlice()}); err != nil {
			return fmt.Errorf("%s: default route via %s: %w", s.Container, s.Gateway, err)
		}
	}
	return nil
}

// --- helpers ---------------------------------------------------------------

func prefixToIPNet(p netip.Prefix) *net.IPNet {
	return &net.IPNet{IP: p.Addr().AsSlice(), Mask: net.CIDRMask(p.Bits(), p.Addr().BitLen())}
}
