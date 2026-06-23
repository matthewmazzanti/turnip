// Package config is the declarative turnip model: the typed loader + validator for
// turnip.json (see docs/CONFIG-SKETCH.md). This is the model ONLY -- WHO exists, WHO
// may talk, WHAT crosses the edge -- never the mechanism that secures it.
//
// Three entities, three scopes:
//   - container   (containers.<name>)              identity + router-independent links
//   - network     (networks.<name>)                a router/bridge netns: gateway, uplink, flows
//   - attachment  (networks.<name>.attach.<name>)  container x network: ip, interface, default
//
// All policy is a flow: intra-network reachability, egress to the internet, and inbound
// port-forwards are rows in the per-network flows list, discriminated by type.
//
// Governing rule (default-deny): omission must never widen. Breadth is opt-in and visible
// -- an explicit proto list, port = "any", a deliberate proto = "any" egress -- never the
// result of a dropped field. A missing proto/port on a scoped flow is a load error, not a wildcard.
//
// Loading: Parse([]byte) is the pure entry point (unmarshal with extra="forbid" + defaults +
// Validate); reading $TURNIP_CONFIG / the file lives in the caller (cmd/turnip), so this
// package has no IO.
package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/netip"
)

// IFNAMSIZ is 15: any kernel-facing interface name must fit, or the device can't be
// created. The loader rejects over-long names rather than letting the kernel truncate
// them into a silent collision.
const ifnameMax = 15

// --- enums (string-typed; members ARE their wire string, like the Python StrEnums) ---

// Proto is a layer-4 protocol expressible as an nft element. ICMP is portless.
type Proto string

const (
	ProtoTCP  Proto = "tcp"
	ProtoUDP  Proto = "udp"
	ProtoICMP Proto = "icmp"
)

func (p Proto) valid() bool {
	switch p {
	case ProtoTCP, ProtoUDP, ProtoICMP:
		return true
	}
	return false
}

// NetworkType: a routed /32 default-deny network, or a shared-L2 bridge (deferred).
type NetworkType string

const (
	NetworkRouter NetworkType = "router"
	NetworkBridge NetworkType = "bridge"
)

// LinkType discriminates the container-link union. Only veth exists today; the field is
// kept as a discriminator for forward-compat (macvlan/ipvlan/phys were trimmed -- see
// docs/CONFIG-SKETCH.md).
type LinkType string

const (
	LinkVeth LinkType = "veth"
)

// --- scalar unions ---------------------------------------------------------

// PortPattern is a concrete port (1-65535) or the explicit wide token "any". There is no
// implicit wildcard; the range is checked in Validate so the error carries context.
type PortPattern struct {
	Any  bool
	Port int // meaningful when !Any
}

func (p *PortPattern) UnmarshalJSON(b []byte) error {
	if string(b) == `"any"` {
		p.Any = true
		return nil
	}
	var n int
	if err := json.Unmarshal(b, &n); err != nil {
		return fmt.Errorf(`port must be an integer 1-65535 or "any"`)
	}
	p.Port = n
	return nil
}

// protoList accepts the scalar "tcp" sugar as well as ["udp", "tcp"].
type protoList []Proto

func (pl *protoList) UnmarshalJSON(b []byte) error {
	if len(b) > 0 && b[0] == '"' {
		var p Proto
		if err := json.Unmarshal(b, &p); err != nil {
			return err
		}
		*pl = protoList{p}
		return nil
	}
	var arr []Proto
	if err := json.Unmarshal(b, &arr); err != nil {
		return err
	}
	*pl = arr
	return nil
}

// egressProto is an egress flow's proto field: the wide "any" token (all L4 protocols,
// portless -- the deliberate "this container gets the internet" form) or a concrete proto
// scalar/list. "any" must stand alone; it never mixes into the list.
type egressProto struct {
	Any  bool
	List []Proto
}

func (e *egressProto) UnmarshalJSON(b []byte) error {
	if string(b) == `"any"` {
		e.Any = true
		return nil
	}
	var pl protoList
	if err := pl.UnmarshalJSON(b); err != nil {
		return err
	}
	e.List = pl
	return nil
}

// --- runtime ---------------------------------------------------------------

// Runtime is the execution environment (the WHERE, separate from the model). All fields
// optional; the environment-dependent defaults are filled in by the caller (the resolve
// step that owns the env + passwd-db reads), not here.
type Runtime struct {
	User     string `json:"user"`
	StateDir string `json:"state_dir"`
	Nft      string `json:"nft"`
	Podman   string `json:"podman"`
}

// --- the flow union: all policy is a flow ---------------------------------

// FlowType discriminates the flow union. Every policy edge -- intra-network reachability,
// leaving for the internet, inbound port-forwards -- is a flow row, keyed by type. The
// non-container end (the internet) is implied by the type, never a magic endpoint name.
type FlowType string

const (
	FlowInternal FlowType = "internal" // container -> container, this network
	FlowEgress   FlowType = "egress"   // container -> the internet, via the uplink
	FlowIngress  FlowType = "ingress"  // the internet -> container (DNAT), via the uplink
)

// Flow is a sealed sum type (discriminated on "type"): one of the concrete structs below.
// Each carries a `Type` field only so strict decode accepts the "type" key; it's the
// discriminator (read once in unmarshalFlow), never read after dispatch. Behavior is
// dispatched by type switch at use-sites (validate.go, plan.go), not by methods.
type Flow interface{ isFlow() }

// InternalFlow is a who-may-initiate-to-whom edge in a router network's forward chain.
// Endpoints are container names attached to THIS network. Directional: From may initiate to
// To on (proto, port), and only that -- the return path rides conntrack, so no reverse entry.
type InternalFlow struct {
	Type  FlowType    `json:"type"`
	From  string      `json:"from"`
	To    string      `json:"to"`
	Proto Proto       `json:"proto"`
	Port  PortPattern `json:"port"`
}

// EgressFlow lets From initiate out the uplink to the internet. Proto may be the wide "any"
// (portless) or a concrete scalar/list; a port-bearing proto requires Port (a dropped port
// is a load error, never a wildcard) -- except ICMP, which is portless.
type EgressFlow struct {
	Type  FlowType     `json:"type"`
	From  string       `json:"from"`
	Proto egressProto  `json:"proto"`
	Port  *PortPattern `json:"port"`
}

// IngressFlow is one internet->container DNAT mapping. It carries TWO ports because it does
// DNAT: HostPort is published on the host edge, Port is the container port (defaults to
// HostPort -- the one widening-safe default). Matched on dest, not source (the client IP is
// a wildcard after DNAT).
type IngressFlow struct {
	Type     FlowType   `json:"type"`
	To       string     `json:"to"`
	Proto    Proto      `json:"proto"`
	HostPort int        `json:"host_port"`
	Port     int        `json:"port"`   // 0 = unset -> defaults to HostPort (in unmarshalFlow)
	Listen   netip.Addr `json:"listen"` // host address the DNAT listens on; default 0.0.0.0
}

func (*InternalFlow) isFlow() {}
func (*EgressFlow) isFlow()   {}
func (*IngressFlow) isFlow()  {}

// unmarshalFlow decodes one flow, dispatching on "type". Stays strict (extra keys rejected).
// Ingress seeds its defaults (Listen 0.0.0.0; Port mirrors HostPort when omitted) after the
// decode, since the decode can't seed a port derived from a sibling field (HostPort).
func unmarshalFlow(b []byte) (Flow, error) {
	var disc struct {
		Type FlowType `json:"type"`
	}
	if err := json.Unmarshal(b, &disc); err != nil {
		return nil, err
	}
	switch disc.Type {
	case FlowInternal:
		var f InternalFlow
		return &f, strictUnmarshal(b, &f)
	case FlowEgress:
		var f EgressFlow
		return &f, strictUnmarshal(b, &f)
	case FlowIngress:
		f := IngressFlow{Listen: netip.AddrFrom4([4]byte{0, 0, 0, 0})}
		if err := strictUnmarshal(b, &f); err != nil {
			return nil, err
		}
		if f.Port == 0 { // omitted -> the published host port
			f.Port = f.HostPort
		}
		return &f, nil
	default:
		return nil, fmt.Errorf("flow type %q must be one of internal/egress/ingress", disc.Type)
	}
}

// --- the attachment (container x network) ---------------------------------

// Attachment is a container's membership in one network, keyed by container under the
// network (so the (network, container) pair is unique by construction). It is pure placement
// now -- every who-may-talk decision is a flow.
type Attachment struct {
	IP        netip.Addr `json:"ip"`
	Interface string     `json:"interface"`
	Default   bool       `json:"default"` // owns the container's 0.0.0.0/0 route
}

// --- container links (holes into host networking) -------------------------

// LinkBase carries the fields every container link shares.
type LinkBase struct {
	Name    string         `json:"name"`    // container-side iface; unique within the container
	Address netip.Prefix   `json:"address"` // a static "<host>/<prefix>" (IPv4Interface)
	Gateway netip.Addr     `json:"gateway"` // optional; zero = unset
	Routes  []netip.Prefix `json:"routes"`
	Mac     string         `json:"mac"`
	Mtu     *int           `json:"mtu"`
	Default bool           `json:"default"` // owns the container default route
}

// Link is a sealed sum type (discriminated on "type"), kept as a union for forward-compat
// though veth is the only variant today. veth is virtual => owned (created from its anchor,
// reaped with the netns). Behavior is dispatched by type switch at use-sites (validateLink in
// validate.go, buildLinkSpec in cmd), not by methods. Base() exposes the shared fields. The
// `Type` field exists only so strict decode accepts the "type" key (read once in unmarshalLink).
type Link interface {
	isLink()
	Base() *LinkBase
}

// VethLink: a veth into a host bridge (Bridge) or the root netns (Peer == "host").
type VethLink struct {
	LinkBase
	Type   LinkType `json:"type"`
	Bridge string   `json:"bridge"`
	Peer   string   `json:"peer"` // "host" or ""
}

func (l *VethLink) Base() *LinkBase { return &l.LinkBase }

// isLink seals the sum type: only this package's structs satisfy Link.
func (*VethLink) isLink() {}

// --- container -------------------------------------------------------------

// Container is an identity; Links are router-independent host-network holes.
type Container struct {
	Links []Link
}

// UnmarshalJSON decodes a container (only `links`), dispatching each link by its "type".
// A custom unmarshaler is needed for the discriminated union; it stays strict (extra keys
// rejected) like the rest of the model.
func (c *Container) UnmarshalJSON(b []byte) error {
	var raw struct {
		Links []json.RawMessage `json:"links"`
	}
	if err := strictUnmarshal(b, &raw); err != nil {
		return err
	}
	for _, lr := range raw.Links {
		link, err := unmarshalLink(lr)
		if err != nil {
			return err
		}
		c.Links = append(c.Links, link)
	}
	return nil
}

func unmarshalLink(b []byte) (Link, error) {
	var disc struct {
		Type LinkType `json:"type"`
	}
	if err := json.Unmarshal(b, &disc); err != nil {
		return nil, err
	}
	switch disc.Type {
	case LinkVeth:
		var l VethLink
		return &l, strictUnmarshal(b, &l)
	default:
		return nil, fmt.Errorf("link type %q must be veth", disc.Type)
	}
}

// --- the network (a router or bridge netns) -------------------------------

// Uplink is a veth between this network's router netns and the host netns -- what makes
// egress/ingress possible, and what makes the run rootful. The host edge always masquerades
// (routed egress, the deferred nat=false option, isn't wired -- see docs/CONFIG-SKETCH.md).
type Uplink struct {
	HostIf   string     `json:"host_if"`
	RouterIf string     `json:"router_if"`
	Link     netip.Addr `json:"link"` // base of the point-to-point /31 (ends are Link and Link+1)
}

// Network is a router netns (default) or a bridge netns. Everything stateful is
// network-scoped: the nft table, the gateway, the uplink/DNAT, so flows and attach are
// per-network.
type Network struct {
	Type      NetworkType
	Gateway   netip.Addr
	GatewayIf string // router: the dummy gateway iface
	Subnet    netip.Prefix
	Uplink    *Uplink
	Attach    map[string]Attachment
	Flows     []Flow // the type-discriminated union; see unmarshalFlow
}

// UnmarshalJSON seeds the default type (router) so an omitted "type" stays router, then decodes
// the flows through the discriminated union (Flow is an interface, so it can't decode directly).
// Stays strict (extra keys rejected).
func (n *Network) UnmarshalJSON(b []byte) error {
	var raw struct {
		Type      NetworkType           `json:"type"`
		Gateway   netip.Addr            `json:"gateway"`
		GatewayIf string                `json:"gateway_if"`
		Subnet    netip.Prefix          `json:"subnet"`
		Uplink    *Uplink               `json:"uplink"`
		Attach    map[string]Attachment `json:"attach"`
		Flows     []json.RawMessage     `json:"flows"`
	}
	raw.Type = NetworkRouter
	if err := strictUnmarshal(b, &raw); err != nil {
		return err
	}
	n.Type, n.Gateway, n.GatewayIf = raw.Type, raw.Gateway, raw.GatewayIf
	n.Subnet, n.Uplink, n.Attach = raw.Subnet, raw.Uplink, raw.Attach
	for _, fr := range raw.Flows {
		fl, err := unmarshalFlow(fr)
		if err != nil {
			return err
		}
		n.Flows = append(n.Flows, fl)
	}
	return nil
}

// --- the whole config ------------------------------------------------------

// Turnip is the parsed turnip.json: containers, networks, attachments + runtime. It holds
// the cross-cutting, container-global checks (the price of the network-centric layout)
// that no single network can see on its own.
type Turnip struct {
	Runtime    Runtime              `json:"runtime"`
	Containers map[string]Container `json:"containers"`
	Networks   map[string]Network   `json:"networks"`
}

// RequiresRoot reports whether the host edge is in play: some network has an uplink or some
// container has links. A pure routed network with neither needs no host-netns privilege.
func (t *Turnip) RequiresRoot() bool {
	for _, n := range t.Networks {
		if n.Uplink != nil {
			return true
		}
	}
	for _, c := range t.Containers {
		if len(c.Links) > 0 {
			return true
		}
	}
	return false
}

// strictUnmarshal decodes b into v rejecting unknown fields -- the extra="forbid" rule,
// applied inside the custom unmarshalers the top-level strict decoder can't reach.
func strictUnmarshal(b []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}
