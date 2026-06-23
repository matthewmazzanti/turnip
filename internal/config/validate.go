package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
)

// Parse unmarshals turnip.json with extra="forbid", then runs the full validation pass.
// Defaults are seeded as it decodes -- the per-type UnmarshalJSON methods (Network type=router,
// ingress flow port/listen in unmarshalFlow) set their default on the receiver, then decode JSON
// over it -- so by Validate every field is concrete. Pure -- no file/env IO (that lives in the caller).
func Parse(data []byte) (*Turnip, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields() // extra="forbid": a typo is a load error, not a silent drop
	var t Turnip
	if err := dec.Decode(&t); err != nil {
		return nil, fmt.Errorf("parse turnip.json: %w", err)
	}
	if err := t.Validate(); err != nil {
		return nil, err
	}
	return &t, nil
}

// Validate runs the whole model check: container links (field-level), each network's
// rules, then the cross-cutting container-global checks. Returns the first violation.
func (t *Turnip) Validate() error {
	for cname, c := range t.Containers {
		for _, l := range c.Links {
			if err := validateLink(cname, l); err != nil {
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
		// internal flows are router-only (checked per-flow below); edge flows are fine on a bridge.
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
		// on a bridge, attach ip is a real subnet address (not a /32)
		if n.Subnet.IsValid() && !n.Subnet.Contains(a.IP) {
			return fmt.Errorf("%s: ip %s not within subnet %s", cname, a.IP, n.Subnet)
		}
	}

	for _, fl := range n.Flows {
		if err := n.validateFlow(name, fl); err != nil {
			return err
		}
	}
	return nil
}

// validateFlow checks one flow against this network: its own field rules, that its endpoint(s)
// are attached here, and the edge prerequisites (egress/ingress need an uplink; internal needs
// a router). A type switch over the sealed Flow union -- the default fails closed.
func (n *Network) validateFlow(name string, fl Flow) error {
	attached := func(c string) error {
		if _, ok := n.Attach[c]; !ok {
			return fmt.Errorf("network %q: flow endpoint %q is not attached to this network", name, c)
		}
		return nil
	}
	ctx := fmt.Sprintf("network %q flow", name)
	switch f := fl.(type) {
	case *InternalFlow:
		if n.Type == NetworkBridge {
			return fmt.Errorf("%s: internal flow is router-only; a bridge segment has no L3 forward hop", ctx)
		}
		if err := f.validate(ctx); err != nil {
			return err
		}
		if err := attached(f.From); err != nil {
			return err
		}
		return attached(f.To)
	case *EgressFlow:
		if n.Uplink == nil {
			return fmt.Errorf("%s: egress flow needs this network's uplink", ctx)
		}
		if err := f.validate(ctx); err != nil {
			return err
		}
		return attached(f.From)
	case *IngressFlow:
		if n.Uplink == nil {
			return fmt.Errorf("%s: ingress flow needs this network's uplink", ctx)
		}
		if err := f.validate(ctx); err != nil {
			return err
		}
		return attached(f.To)
	default:
		return fmt.Errorf("%s: unhandled flow type %T", ctx, fl)
	}
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

// --- attachment ------------------------------------------------------------

func (a *Attachment) validate(ctx string) error {
	if !a.IP.Is4() {
		return fmt.Errorf("%s: ip must be an IPv4 address", ctx)
	}
	return validateIfName(ctx+" interface", a.Interface)
}

// --- flows (the per-type field rules; endpoint/edge checks are in validateFlow) ------------

func (f *InternalFlow) validate(ctx string) error {
	if !f.Proto.valid() {
		return fmt.Errorf("%s: internal proto %q invalid", ctx, f.Proto)
	}
	if !f.Port.Any {
		return portInRange(ctx+" port", f.Port.Port)
	}
	return nil
}

func (f *EgressFlow) validate(ctx string) error {
	if f.Proto.Any { // the wide "this container gets the internet" form: portless
		if f.Port != nil {
			return fmt.Errorf("%s: egress proto \"any\" carries no port; drop the 'port' field", ctx)
		}
		return nil
	}
	if len(f.Proto.List) == 0 {
		return fmt.Errorf("%s: egress flow needs a proto (a scalar, a list, or \"any\")", ctx)
	}
	portBearing := false
	for _, p := range f.Proto.List {
		if !p.valid() {
			return fmt.Errorf("%s: egress proto %q invalid", ctx, p)
		}
		if p != ProtoICMP {
			portBearing = true
		}
	}
	if portBearing && f.Port == nil {
		return fmt.Errorf("%s for %v missing 'port' (fail-closed); use a port or \"any\"", ctx, []Proto(f.Proto.List))
	}
	if f.Port != nil && !portBearing {
		return fmt.Errorf("%s: icmp egress carries no port; drop the 'port' field", ctx)
	}
	if f.Port != nil && !f.Port.Any {
		return portInRange(ctx+" egress port", f.Port.Port)
	}
	return nil
}

func (f *IngressFlow) validate(ctx string) error {
	if !f.Proto.valid() {
		return fmt.Errorf("%s: ingress proto %q invalid", ctx, f.Proto)
	}
	if f.Proto == ProtoICMP {
		return fmt.Errorf("%s: ingress (DNAT) needs a port-bearing proto: tcp or udp", ctx)
	}
	if err := portInRange(ctx+" ingress host_port", f.HostPort); err != nil {
		return err
	}
	if err := portInRange(ctx+" ingress port", f.Port); err != nil {
		return err
	}
	if f.Listen.IsValid() && !f.Listen.Is4() {
		return fmt.Errorf("%s: ingress listen must be an IPv4 address", ctx)
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

// validateLink runs a container link's field-level checks: the shared base, then the per-type
// rules. A type switch over the sealed Link sum type -- the default fails closed if a variant
// is ever added without a case here.
func validateLink(cname string, l Link) error {
	if err := l.Base().validate(cname); err != nil {
		return err
	}
	switch lk := l.(type) {
	case *VethLink:
		hasBridge, hasPeer := lk.Bridge != "", lk.Peer != ""
		if hasBridge == hasPeer {
			return fmt.Errorf(`%s: veth link %q needs exactly one of "bridge" or "peer":"host"`, cname, lk.Name)
		}
		if hasPeer && lk.Peer != "host" {
			return fmt.Errorf(`%s: veth link %q peer must be "host"`, cname, lk.Name)
		}
		if hasBridge {
			return validateIfName(cname+" bridge", lk.Bridge)
		}
		return nil
	default:
		return fmt.Errorf("%s: unhandled link type %T", cname, l)
	}
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
		}
		for _, fl := range n.Flows {
			ing, ok := fl.(*IngressFlow)
			if !ok {
				continue
			}
			key := portKey{ing.Listen.String(), ing.Proto, ing.HostPort}
			if prev, dup := ports[key]; dup {
				return fmt.Errorf("host_port collision (%s %s %d): %s vs %s",
					key.listen, key.proto, key.hostPort, ing.To, prev)
			}
			ports[key] = ing.To
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
