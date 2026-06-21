// Package config is the declarative turnip model: the typed loader + validator for
// turnip.json (see docs/CONFIG-SKETCH.md). This is the model ONLY -- WHO exists, WHO
// may talk, WHAT crosses the edge -- never the mechanism that secures it. It is a port
// of old/src/turnip/config.py (pydantic) onto encoding/json + an explicit Validate pass.
//
// Three entities, three scopes:
//   - container   (containers.<name>)              identity + router-independent links
//   - network     (networks.<name>)                a router/bridge netns: gateway, uplink, flows
//   - attachment  (networks.<name>.attach.<name>)  container x network: ip, interface, egress/ingress
//
// Governing rule (default-deny): omission must never widen. Breadth is opt-in and visible
// -- an explicit proto list, port = "any", a deliberate egress = true -- never the result
// of a dropped field. A missing proto/port on a scoped rule is a load error, not a wildcard.
//
// Loading is split like the Python: Parse([]byte) is the pure model_validate equivalent
// (unmarshal with extra="forbid" + defaults + Validate); reading $TURNIP_CONFIG / the file
// lives in the caller (cmd/turnip), so this package has no IO.
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

// HOSTPrefix / LINKPrefix are locked by topology, not configurable (mirrors config.py).
const (
	HOSTPrefix = 32 // the routed /32 model
	LINKPrefix = 31 // uplink veth is a point-to-point /31 (RFC 3021)
)

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

// LinkType discriminates the container-link union.
type LinkType string

const (
	LinkVeth    LinkType = "veth"
	LinkMacvlan LinkType = "macvlan"
	LinkIpvlan  LinkType = "ipvlan"
	LinkPhys    LinkType = "phys"
)

type MacvlanMode string

const (
	MacvlanBridge   MacvlanMode = "bridge"
	MacvlanPrivate  MacvlanMode = "private"
	MacvlanVepa     MacvlanMode = "vepa"
	MacvlanPassthru MacvlanMode = "passthru"
)

func (m MacvlanMode) valid() bool {
	switch m {
	case MacvlanBridge, MacvlanPrivate, MacvlanVepa, MacvlanPassthru:
		return true
	}
	return false
}

type IpvlanMode string

const (
	IpvlanL2  IpvlanMode = "l2"
	IpvlanL3  IpvlanMode = "l3"
	IpvlanL3S IpvlanMode = "l3s"
)

func (m IpvlanMode) valid() bool {
	switch m {
	case IpvlanL2, IpvlanL3, IpvlanL3S:
		return true
	}
	return false
}

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

// protoList accepts the scalar "tcp" sugar as well as ["udp", "tcp"] (config.py's
// EgressRule._listify).
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

// ResolvedRuntime is Runtime with the environment-dependent defaults filled in (by the
// caller's resolve step -- this package stays free of env/IO). Not parsed from JSON.
type ResolvedRuntime struct {
	User     string
	UID      int
	GID      int
	Home     string
	StateDir string
	Nft      string
	Podman   string
}

// --- edges: egress / ingress ----------------------------------------------

// EgressRule is one scoped outbound allowance. proto and port are both required (a dropped
// field is a load error, never a wildcard) -- except ICMP, which is portless. proto may be
// a list and fans out to one nft element per proto.
type EgressRule struct {
	Proto protoList    `json:"proto"`
	Port  *PortPattern `json:"port"`
}

// Egress on an attachment: a deliberate true (any external dest/proto/port), a list of
// scoped rules, or false/absent (default-deny).
type Egress struct {
	All   bool         // egress: true
	Rules []EgressRule // egress: [ {...}, ... ]
}

func (e *Egress) UnmarshalJSON(b []byte) error {
	switch string(b) {
	case "true":
		e.All = true
		return nil
	case "false", "null":
		return nil
	}
	if len(b) > 0 && b[0] == '[' {
		var raws []json.RawMessage
		if err := json.Unmarshal(b, &raws); err != nil {
			return err
		}
		for _, r := range raws {
			var er EgressRule
			if err := strictUnmarshal(r, &er); err != nil {
				return err
			}
			e.Rules = append(e.Rules, er)
		}
		return nil
	}
	return fmt.Errorf("egress must be a bool or a list of {proto, port} rules")
}

// active mirrors Python's bool(egress): true, or a non-empty rule list.
func (e Egress) active() bool { return e.All || len(e.Rules) > 0 }

// IngressRule is one host->container DNAT mapping. It carries TWO ports because it does
// DNAT: HostPort is published on the host edge, Port is the container port (defaults to
// HostPort -- the one widening-safe default).
type IngressRule struct {
	Proto    Proto      `json:"proto"`
	HostPort int        `json:"host_port"`
	Port     int        `json:"port"`   // 0 = unset -> defaults to HostPort (setDefaults)
	Listen   netip.Addr `json:"listen"` // host address the DNAT listens on; default 0.0.0.0
}

// --- intra-network policy: flows (router-only) ----------------------------

// Flow is a who-may-initiate-to-whom edge in a router network's forward chain. Endpoints
// are container names attached to THIS network. Directional: from may initiate to `to` on
// (proto, port), and only that -- the return path rides conntrack, so no reverse entry.
type Flow struct {
	From  string      `json:"from"`
	To    string      `json:"to"`
	Proto Proto       `json:"proto"`
	Port  PortPattern `json:"port"`
}

// --- the attachment (container x network) ---------------------------------

// Attachment is a container's membership in one network, keyed by container under the
// network (so the (network, container) pair is unique by construction).
type Attachment struct {
	IP        netip.Addr    `json:"ip"`
	Interface string        `json:"interface"`
	Default   bool          `json:"default"` // owns the container's 0.0.0.0/0 route
	Egress    Egress        `json:"egress"`
	Ingress   []IngressRule `json:"ingress"`
}

// wantsEdge reports whether this attachment needs its network's uplink (any egress/ingress).
func (a Attachment) wantsEdge() bool { return a.Egress.active() || len(a.Ingress) > 0 }

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

// Link is the discriminated union (on "type"). Ownership is implied by type, never a flag:
// veth/macvlan/ipvlan are virtual => owned; phys is physical => borrowed.
type Link interface {
	base() *LinkBase
	Kind() LinkType
	validate(cname string) error
}

// VethLink: a veth into a host bridge (Bridge) or the root netns (Peer == "host").
type VethLink struct {
	LinkBase
	Type   LinkType `json:"type"`
	Bridge string   `json:"bridge"`
	Peer   string   `json:"peer"` // "host" or ""
}

// MacvlanLink: own MAC/IP on the parent's LAN.
type MacvlanLink struct {
	LinkBase
	Type   LinkType    `json:"type"`
	Parent string      `json:"parent"`
	Mode   MacvlanMode `json:"mode"` // default bridge
}

// IpvlanLink: single MAC (works on WiFi).
type IpvlanLink struct {
	LinkBase
	Type   LinkType   `json:"type"`
	Parent string     `json:"parent"`
	Mode   IpvlanMode `json:"mode"` // default l2
}

// PhysLink: a BORROWED NIC/VF -- moved in, returned to root on down, never deleted.
type PhysLink struct {
	LinkBase
	Type LinkType `json:"type"`
	Dev  string   `json:"dev"`
}

func (l *VethLink) base() *LinkBase    { return &l.LinkBase }
func (l *MacvlanLink) base() *LinkBase { return &l.LinkBase }
func (l *IpvlanLink) base() *LinkBase  { return &l.LinkBase }
func (l *PhysLink) base() *LinkBase    { return &l.LinkBase }

func (l *VethLink) Kind() LinkType    { return LinkVeth }
func (l *MacvlanLink) Kind() LinkType { return LinkMacvlan }
func (l *IpvlanLink) Kind() LinkType  { return LinkIpvlan }
func (l *PhysLink) Kind() LinkType    { return LinkPhys }

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
	case LinkMacvlan:
		var l MacvlanLink
		return &l, strictUnmarshal(b, &l)
	case LinkIpvlan:
		var l IpvlanLink
		return &l, strictUnmarshal(b, &l)
	case LinkPhys:
		var l PhysLink
		return &l, strictUnmarshal(b, &l)
	default:
		return nil, fmt.Errorf("link type %q must be one of veth/macvlan/ipvlan/phys", disc.Type)
	}
}

// --- the network (a router or bridge netns) -------------------------------

// Uplink is a veth between this network's router netns and the host netns -- what makes
// egress/ingress possible, and what makes the run rootful.
type Uplink struct {
	HostIf   string     `json:"host_if"`
	RouterIf string     `json:"router_if"`
	Link     netip.Addr `json:"link"` // base of the point-to-point /31 (ends are Link and Link+1)
	Nat      *bool      `json:"nat"`  // masquerade; default true (false = routed)
}

// NAT reports the effective masquerade setting (default true when unset).
func (u *Uplink) NAT() bool { return u.Nat == nil || *u.Nat }

// Network is a router netns (default) or a bridge netns. Everything stateful is
// network-scoped: the nft table, the gateway, the uplink/DNAT, so flows and attach are
// per-network.
type Network struct {
	Type      NetworkType           `json:"type"` // default router
	Gateway   netip.Addr            `json:"gateway"`
	GatewayIf string                `json:"gateway_if"` // router: the dummy gateway iface
	Subnet    netip.Prefix          `json:"subnet"`     // bridge-only; zero = unset
	Uplink    *Uplink               `json:"uplink"`
	Attach    map[string]Attachment `json:"attach"`
	Flows     []Flow                `json:"flows"`
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
