package dataplane

import (
	"fmt"
	"net"
	"net/netip"
	"os"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
)

// LinkSpec is one container link -- a host netdev hole into a container netns, the
// deliberate L2 trust escape (outside every router and its nft policy). Kind selects the
// mechanism; the rest are the fields it needs. Ownership is implied by Kind: veth/macvlan/
// ipvlan are virtual (owned, reaped with the netns); phys is borrowed (returned to init).
type LinkSpec struct {
	Container string // for error messages
	Kind      string // "veth-bridge" | "veth-host" | "macvlan" | "ipvlan" | "phys"
	Name      string // container-side iface name

	HostIf string // veth host-side end name (veth-*)
	Bridge string // host bridge to enslave the veth host end (veth-bridge)
	Parent string // macvlan/ipvlan parent
	Mode   string // macvlan/ipvlan mode
	Dev    string // phys host device to move in

	Address netip.Prefix   // a real on-subnet address (not a /32)
	Gateway netip.Addr     // optional; default route via it iff Default
	Routes  []netip.Prefix // static routes
	Mac     string
	MTU     int
	Default bool // owns the container's default route
}

// ValidateLinkConflicts is the PURE half of link validation: cross-spec invariants that need
// no host. Today: no host device may be asked to be both a macvlan and an ipvlan master -- a
// parent is one flavor XOR the other. Pure (no netlink), so it units without root and belongs
// in the planning phase (buildPlan). The host-side anchor checks are ValidateLinkAnchors.
func ValidateLinkConflicts(specs []LinkSpec) error {
	flavor := map[string]string{}
	for _, s := range specs {
		if s.Kind != "macvlan" && s.Kind != "ipvlan" {
			continue
		}
		if prev, ok := flavor[s.Parent]; ok && prev != s.Kind {
			return fmt.Errorf("parent %q: macvlan and ipvlan cannot share a parent device (%s vs %s)",
				s.Parent, prev, s.Kind)
		}
		flavor[s.Parent] = s.Kind
	}
	return nil
}

// ValidateLinkAnchors is the IO half: it checks each link's host-side anchor in the live init
// netns (exists, right kind, not wireless/primary) BEFORE any netns is built, so a bad anchor
// fails fast. Anchors are BORROWED -- we only check, never create. Reads the kernel, so it runs
// in the preflight phase (after the pure buildPlan), not in lowering. Ports validate_link_anchors.
func ValidateLinkAnchors(specs []LinkSpec) error {
	if len(specs) == 0 {
		return nil
	}
	defOifs, err := defaultRouteOifs()
	if err != nil {
		return fmt.Errorf("read default routes: %w", err)
	}
	for _, s := range specs {
		if err := validateAnchor(s, defOifs); err != nil {
			return err
		}
	}
	return nil
}

func validateAnchor(s LinkSpec, defOifs map[int]bool) error {
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
	case "macvlan":
		if _, err := netlink.LinkByName(s.Parent); err != nil {
			return fmt.Errorf("%s: macvlan parent %q not found in host netns", s.Container, s.Parent)
		}
		if isWireless(s.Parent) {
			return fmt.Errorf("%s: macvlan parent %q is wireless; use ipvlan", s.Container, s.Parent)
		}
	case "ipvlan":
		if _, err := netlink.LinkByName(s.Parent); err != nil {
			return fmt.Errorf("%s: ipvlan parent %q not found in host netns", s.Container, s.Parent)
		}
	case "phys":
		dev, err := netlink.LinkByName(s.Dev)
		if err != nil {
			return fmt.Errorf("%s: phys dev %q not found in host netns", s.Container, s.Dev)
		}
		if defOifs[dev.Attrs().Index] {
			return fmt.Errorf("%s: phys dev %q is the host's primary (default-route) NIC; "+
				"refusing to move it into a container", s.Container, s.Dev)
		}
	}
	return nil
}

// LinkConnect moves a host netdev into the container netns and configures the in-container
// half. The host-side mechanism differs by Kind -- own (create) vs borrow (move). The new
// device is born/moved directly into the container netns (IFLA_NET_NS_FD), then configured
// over the netns fd. Ports link_connect. Run from the init parent.
func LinkConnect(contFd int, s LinkSpec) error {
	entryName := s.Name // the device's name on entry; phys keeps its host name (below)

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
	case "macvlan":
		parent, err := netlink.LinkByName(s.Parent)
		if err != nil {
			return fmt.Errorf("%s: find macvlan parent %s: %w", s.Container, s.Parent, err)
		}
		mv := &netlink.Macvlan{
			LinkAttrs: netlink.LinkAttrs{Name: s.Name, ParentIndex: parent.Attrs().Index, Namespace: netlink.NsFd(contFd)},
			Mode:      macvlanMode(s.Mode),
		}
		if err := netlink.LinkAdd(mv); err != nil {
			return fmt.Errorf("%s: add macvlan %s: %w", s.Container, s.Name, err)
		}
	case "ipvlan":
		parent, err := netlink.LinkByName(s.Parent)
		if err != nil {
			return fmt.Errorf("%s: find ipvlan parent %s: %w", s.Container, s.Parent, err)
		}
		iv := &netlink.IPVlan{
			LinkAttrs: netlink.LinkAttrs{Name: s.Name, ParentIndex: parent.Attrs().Index, Namespace: netlink.NsFd(contFd)},
			Mode:      ipvlanMode(s.Mode),
		}
		if err := netlink.LinkAdd(iv); err != nil {
			return fmt.Errorf("%s: add ipvlan %s: %w", s.Container, s.Name, err)
		}
	case "phys":
		entryName = s.Dev // moved in under its host name; configureLinkIface renames it
		dev, err := netlink.LinkByName(s.Dev)
		if err != nil {
			return fmt.Errorf("%s: find phys dev %s: %w", s.Container, s.Dev, err)
		}
		if err := netlink.LinkSetNsFd(dev, contFd); err != nil {
			return fmt.Errorf("%s: move phys %s into container: %w", s.Container, s.Dev, err)
		}
	default:
		return fmt.Errorf("%s: unknown link kind %q", s.Container, s.Kind)
	}

	return configureLinkIface(contFd, entryName, s)
}

// configureLinkIface is the uniform in-container half: rename (phys), mac/mtu, the on-subnet
// address, up, static routes, and default-via-gateway iff this link owns the default route.
func configureLinkIface(contFd int, entryName string, s LinkSpec) error {
	h, err := netlink.NewHandleAt(netns.NsHandle(contFd))
	if err != nil {
		return fmt.Errorf("%s: container netlink handle: %w", s.Container, err)
	}
	defer h.Close()

	link, err := h.LinkByName(entryName)
	if err != nil {
		return fmt.Errorf("%s: find %s in container: %w", s.Container, entryName, err)
	}
	if entryName != s.Name { // phys: arrives named after its host dev; rename (down, so ok)
		if err := h.LinkSetName(link, s.Name); err != nil {
			return fmt.Errorf("%s: rename %s -> %s: %w", s.Container, entryName, s.Name, err)
		}
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

// --- anchor helpers (init netns) -------------------------------------------

func defaultRouteOifs() (map[int]bool, error) {
	routes, err := netlink.RouteList(nil, netlink.FAMILY_V4)
	if err != nil {
		return nil, err
	}
	oifs := map[int]bool{}
	for _, r := range routes {
		if r.Dst == nil || (r.Dst.IP.IsUnspecified() && prefixOnes(r.Dst) == 0) {
			oifs[r.LinkIndex] = true
		}
	}
	return oifs, nil
}

func prefixOnes(n *net.IPNet) int { ones, _ := n.Mask.Size(); return ones }

// isWireless reports whether ifname is a wireless netdev (it exposes .../wireless). macvlan
// can't bridge onto a wireless parent (the AP drops the extra MACs).
func isWireless(ifname string) bool {
	_, err := os.Stat("/sys/class/net/" + ifname + "/wireless")
	return err == nil
}

func macvlanMode(m string) netlink.MacvlanMode {
	switch m {
	case "private":
		return netlink.MACVLAN_MODE_PRIVATE
	case "vepa":
		return netlink.MACVLAN_MODE_VEPA
	case "passthru":
		return netlink.MACVLAN_MODE_PASSTHRU
	default:
		return netlink.MACVLAN_MODE_BRIDGE
	}
}

func ipvlanMode(m string) netlink.IPVlanMode {
	switch m {
	case "l3":
		return netlink.IPVLAN_MODE_L3
	case "l3s":
		return netlink.IPVLAN_MODE_L3S
	default:
		return netlink.IPVLAN_MODE_L2
	}
}

func prefixToIPNet(p netip.Prefix) *net.IPNet {
	return &net.IPNet{IP: p.Addr().AsSlice(), Mask: net.CIDRMask(p.Bits(), p.Addr().BitLen())}
}
