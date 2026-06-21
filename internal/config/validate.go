package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/netip"
	"sort"
)

// Parse is the model_validate equivalent: unmarshal turnip.json with extra="forbid",
// fill in the defaults, then run the full validation pass. Pure -- no file/env IO (that
// lives in the caller).
func Parse(data []byte) (*Turnip, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields() // extra="forbid": a typo is a load error, not a silent drop
	var t Turnip
	if err := dec.Decode(&t); err != nil {
		return nil, fmt.Errorf("parse turnip.json: %w", err)
	}
	t.setDefaults()
	if err := t.Validate(); err != nil {
		return nil, err
	}
	return &t, nil
}

// setDefaults fills the non-zero defaults JSON omission leaves as Go zero values: a
// network's type (router), a link's mode (macvlan bridge / ipvlan l2), an ingress rule's
// container port (= host_port) and listen address (0.0.0.0). Done before Validate so the
// rules see concrete values. (Uplink.NAT defaults via its accessor, not here.)
func (t *Turnip) setDefaults() {
	for name, n := range t.Networks {
		if n.Type == "" {
			n.Type = NetworkRouter
		}
		for cn, a := range n.Attach {
			for i := range a.Ingress {
				ing := &a.Ingress[i]
				if ing.Port == 0 {
					ing.Port = ing.HostPort
				}
				if !ing.Listen.IsValid() {
					ing.Listen = netip.AddrFrom4([4]byte{0, 0, 0, 0})
				}
			}
			n.Attach[cn] = a
		}
		t.Networks[name] = n
	}
	for _, c := range t.Containers {
		for _, l := range c.Links {
			switch lk := l.(type) {
			case *MacvlanLink:
				if lk.Mode == "" {
					lk.Mode = MacvlanBridge
				}
			case *IpvlanLink:
				if lk.Mode == "" {
					lk.Mode = IpvlanL2
				}
			}
		}
	}
}

// Validate runs the whole model check: container links (field-level), each network's
// rules, then the cross-cutting container-global checks. Returns the first violation.
func (t *Turnip) Validate() error {
	for cname, c := range t.Containers {
		for _, l := range c.Links {
			if err := l.validate(cname); err != nil {
				return err
			}
		}
	}
	for name, n := range t.Networks {
		if err := n.validate(name); err != nil {
			return err
		}
	}
	return t.crossCutting()
}

// --- network ---------------------------------------------------------------

func (n *Network) validate(name string) error {
	if n.Type != NetworkRouter && n.Type != NetworkBridge {
		return fmt.Errorf("network %q: type %q must be 'router' or 'bridge'", name, n.Type)
	}

	// type-structural rules (before field checks, so an absent gateway_if reads as the
	// router "requires 'gateway_if'", not a generic 1-15 length complaint).
	switch n.Type {
	case NetworkRouter:
		if n.Subnet.IsValid() {
			return fmt.Errorf("network %q: subnet is forbidden on a router network (/32 everywhere)", name)
		}
		if n.GatewayIf == "" {
			return fmt.Errorf("network %q: router network requires 'gateway_if' (the gateway iface)", name)
		}
	case NetworkBridge:
		if !n.Subnet.IsValid() {
			return fmt.Errorf("network %q: bridge network requires 'subnet'", name)
		}
		if len(n.Flows) > 0 {
			return fmt.Errorf("network %q: flows is router-only; a bridge segment has no L3 forward hop", name)
		}
		if !n.Subnet.Contains(n.Gateway) {
			return fmt.Errorf("network %q: bridge gateway %s not within subnet %s", name, n.Gateway, n.Subnet)
		}
	}

	if !n.Gateway.Is4() {
		return fmt.Errorf("network %q: gateway must be an IPv4 address", name)
	}
	if n.GatewayIf != "" {
		if err := validateIfName(fmt.Sprintf("network %q gateway_if", name), n.GatewayIf); err != nil {
			return err
		}
	}
	if n.Uplink != nil {
		if err := n.Uplink.validate(fmt.Sprintf("network %q uplink", name)); err != nil {
			return err
		}
	}

	for cname, a := range n.Attach {
		ctx := fmt.Sprintf("network %q attach %q", name, cname)
		if err := a.validate(ctx); err != nil {
			return err
		}
		// the edge rides the uplink: no uplink, no path out -> the rule is meaningless
		if a.wantsEdge() && n.Uplink == nil {
			return fmt.Errorf("%s: egress/ingress needs this network's uplink", cname)
		}
		// on a bridge, attach ip is a real subnet address (not a /32)
		if n.Subnet.IsValid() && !n.Subnet.Contains(a.IP) {
			return fmt.Errorf("%s: ip %s not within subnet %s", cname, a.IP, n.Subnet)
		}
	}

	for i := range n.Flows {
		f := n.Flows[i]
		if err := f.validate(fmt.Sprintf("network %q flow", name)); err != nil {
			return err
		}
		for _, end := range []string{f.From, f.To} {
			if _, ok := n.Attach[end]; !ok {
				return fmt.Errorf("network %q: flow endpoint %q is not attached to this network", name, end)
			}
		}
	}
	return nil
}

func (u *Uplink) validate(ctx string) error {
	if err := validateIfName(ctx+" host_if", u.HostIf); err != nil {
		return err
	}
	if err := validateIfName(ctx+" router_if", u.RouterIf); err != nil {
		return err
	}
	if !u.Link.Is4() {
		return fmt.Errorf("%s: link must be an IPv4 address", ctx)
	}
	// the /31 prefix is locked, so only the base is configured; require it even so
	// {base, base+1} is a well-formed /31 with one canonical spelling.
	if u.Link.As4()[3]&1 == 1 {
		return fmt.Errorf("%s: link %s must be the even base of a /31 (ends are base, base+1)", ctx, u.Link)
	}
	return nil
}

// --- attachment + edges ----------------------------------------------------

func (a *Attachment) validate(ctx string) error {
	if !a.IP.Is4() {
		return fmt.Errorf("%s: ip must be an IPv4 address", ctx)
	}
	if err := validateIfName(ctx+" interface", a.Interface); err != nil {
		return err
	}
	for i := range a.Egress.Rules {
		if err := a.Egress.Rules[i].validate(ctx); err != nil {
			return err
		}
	}
	for i := range a.Ingress {
		if err := a.Ingress[i].validate(ctx); err != nil {
			return err
		}
	}
	return nil
}

func (r *EgressRule) validate(ctx string) error {
	portBearing := false
	for _, p := range r.Proto {
		if !p.valid() {
			return fmt.Errorf("%s: egress proto %q invalid", ctx, p)
		}
		if p != ProtoICMP {
			portBearing = true
		}
	}
	if portBearing && r.Port == nil {
		return fmt.Errorf("%s: egress rule for %v missing 'port' (fail-closed); use a port or \"any\"", ctx, []Proto(r.Proto))
	}
	if r.Port != nil && !portBearing {
		return fmt.Errorf("%s: icmp egress carries no port; drop the 'port' field", ctx)
	}
	if r.Port != nil && !r.Port.Any {
		return portInRange(ctx+" egress port", r.Port.Port)
	}
	return nil
}

func (r *IngressRule) validate(ctx string) error {
	if !r.Proto.valid() {
		return fmt.Errorf("%s: ingress proto %q invalid", ctx, r.Proto)
	}
	if r.Proto == ProtoICMP {
		return fmt.Errorf("%s: ingress (DNAT) needs a port-bearing proto: tcp or udp", ctx)
	}
	if err := portInRange(ctx+" ingress host_port", r.HostPort); err != nil {
		return err
	}
	if err := portInRange(ctx+" ingress port", r.Port); err != nil {
		return err
	}
	if r.Listen.IsValid() && !r.Listen.Is4() {
		return fmt.Errorf("%s: ingress listen must be an IPv4 address", ctx)
	}
	return nil
}

func (f *Flow) validate(ctx string) error {
	if !f.Proto.valid() {
		return fmt.Errorf("%s: proto %q invalid", ctx, f.Proto)
	}
	if !f.Port.Any {
		return portInRange(ctx+" port", f.Port.Port)
	}
	return nil
}

// --- container links -------------------------------------------------------

func (b *LinkBase) validate(cname string) error {
	if err := validateIfName(cname+" link name", b.Name); err != nil {
		return err
	}
	if !b.Address.IsValid() || !b.Address.Addr().Is4() {
		return fmt.Errorf("%s: link %q address must be an IPv4 CIDR", cname, b.Name)
	}
	if b.Gateway.IsValid() && !b.Gateway.Is4() {
		return fmt.Errorf("%s: link %q gateway must be an IPv4 address", cname, b.Name)
	}
	for _, rt := range b.Routes {
		if !rt.Addr().Is4() {
			return fmt.Errorf("%s: link %q route %s must be IPv4", cname, b.Name, rt)
		}
	}
	if b.Mtu != nil && (*b.Mtu < 68 || *b.Mtu > 65535) {
		return fmt.Errorf("%s: link %q mtu %d out of range (68-65535)", cname, b.Name, *b.Mtu)
	}
	return nil
}

func (l *VethLink) validate(cname string) error {
	if err := l.LinkBase.validate(cname); err != nil {
		return err
	}
	hasBridge, hasPeer := l.Bridge != "", l.Peer != ""
	if hasBridge == hasPeer {
		return fmt.Errorf(`%s: veth link %q needs exactly one of "bridge" or "peer":"host"`, cname, l.Name)
	}
	if hasPeer && l.Peer != "host" {
		return fmt.Errorf(`%s: veth link %q peer must be "host"`, cname, l.Name)
	}
	if hasBridge {
		return validateIfName(cname+" bridge", l.Bridge)
	}
	return nil
}

func (l *MacvlanLink) validate(cname string) error {
	if err := l.LinkBase.validate(cname); err != nil {
		return err
	}
	if !l.Mode.valid() {
		return fmt.Errorf("%s: link %q macvlan mode %q invalid", cname, l.Name, l.Mode)
	}
	return validateIfName(cname+" parent", l.Parent)
}

func (l *IpvlanLink) validate(cname string) error {
	if err := l.LinkBase.validate(cname); err != nil {
		return err
	}
	if !l.Mode.valid() {
		return fmt.Errorf("%s: link %q ipvlan mode %q invalid", cname, l.Name, l.Mode)
	}
	return validateIfName(cname+" parent", l.Parent)
}

func (l *PhysLink) validate(cname string) error {
	if err := l.LinkBase.validate(cname); err != nil {
		return err
	}
	return validateIfName(cname+" dev", l.Dev)
}

// --- cross-cutting (container-global) --------------------------------------

// crossCutting holds the checks no single network can see: per-container interface-name
// uniqueness + exactly-one-default (the price of the network-centric layout), host_port
// collisions across every uplink, and that an attach names a declared container.
func (t *Turnip) crossCutting() error {
	type iface struct {
		name string
		def  bool
	}
	ifaces := map[string][]iface{}
	for c := range t.Containers {
		ifaces[c] = nil
	}
	for cname, c := range t.Containers {
		for _, l := range c.Links {
			b := l.Base()
			ifaces[cname] = append(ifaces[cname], iface{b.Name, b.Default})
		}
	}

	// host_port is a single host-wide resource: collision check spans every uplink,
	// keyed by (listen, proto, host_port).
	type portKey struct {
		listen   string
		proto    Proto
		hostPort int
	}
	ports := map[portKey]string{}
	for net, n := range t.Networks {
		for cname, a := range n.Attach {
			if _, ok := t.Containers[cname]; !ok {
				return fmt.Errorf("network %q: attaches unknown container %q", net, cname)
			}
			ifaces[cname] = append(ifaces[cname], iface{a.Interface, a.Default})
			for _, ing := range a.Ingress {
				key := portKey{ing.Listen.String(), ing.Proto, ing.HostPort}
				if prev, dup := ports[key]; dup {
					return fmt.Errorf("host_port collision (%s %s %d): %s vs %s",
						key.listen, key.proto, key.hostPort, cname, prev)
				}
				ports[key] = cname
			}
		}
	}

	for cname, lst := range ifaces {
		counts := map[string]int{}
		for _, f := range lst {
			counts[f.name]++
		}
		var dupes []string
		for nm, c := range counts {
			if c > 1 {
				dupes = append(dupes, nm)
			}
		}
		if len(dupes) > 0 {
			sort.Strings(dupes)
			return fmt.Errorf("%s: duplicate interface name(s) %v", cname, dupes)
		}
		ndefault := 0
		for _, f := range lst {
			if f.def {
				ndefault++
			}
		}
		if ndefault > 1 {
			return fmt.Errorf("%s: %d interfaces marked default; pick one", cname, ndefault)
		}
		if ndefault == 0 && len(lst) > 1 {
			return fmt.Errorf("%s: %d interfaces and none marked default; pick one", cname, len(lst))
		}
	}
	return nil
}

// --- helpers ---------------------------------------------------------------

func validateIfName(label, name string) error {
	if len(name) < 1 || len(name) > ifnameMax {
		return fmt.Errorf("%s: interface name %q must be 1-15 chars", label, name)
	}
	return nil
}

func portInRange(label string, p int) error {
	if p < 1 || p > 65535 {
		return fmt.Errorf("%s %d out of range (1-65535)", label, p)
	}
	return nil
}
