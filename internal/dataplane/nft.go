package dataplane

import (
	"encoding/binary"
	"fmt"
	"net/netip"

	"github.com/google/nftables"
	"github.com/google/nftables/expr"
	"golang.org/x/sys/unix"
)

// nftTable is the per-router-netns table (one per netns; constant name).
const nftTable = "turnip"

// ct state bits nft uses (1<<0 invalid, then established/related/new). Little-endian in
// the register, so the masks built from these are LE uint32s.
const (
	ctInvalid     uint32 = 1
	ctEstablished uint32 = 2
	ctRelated     uint32 = 4
	ctNew         uint32 = 8
)

// Flow is one directional allow: FromIP may initiate to ToIP on (Proto, Port), and only
// that -- the return path rides conntrack. The caller resolves container names to IPs and
// drops icmp / port="any" (not wired), so Proto is always tcp/udp here.
type Flow struct {
	FromIP netip.Addr
	ToIP   netip.Addr
	Proto  string // "tcp" | "udp"
	Port   uint16
}

// ConfigureNFT loads the `inet turnip` ruleset into the router netns by fd (google/nftables
// over the nf_tables netlink socket -- no `nft` subprocess): the forward flow matrix and the
// router's own-address lockdown. The netns is freshly bootstrapped (empty), so this just
// builds -- no flush/reload. Ports nftlib + main.py build_nft, minus the deferred uplink edge.
//
//   - forward (policy drop): accept ct established/related (the conntrack return path, so
//     flows are one-way in the map); drop ct invalid; for ct new, vmap the (saddr, daddr,
//     l4proto, dport) key into allowed_flows; else policy drop.
//   - input (policy drop): the router's OWN address (gateway, future uplink end) is
//     default-deny. Accept loopback, the conntrack return, and icmp (the gateway ping); tcp/
//     udp fall to the drop, so no router-local service is reachable without a deliberate allow.
func ConfigureNFT(routerFd int, flows []Flow) error {
	c, err := nftables.New(nftables.WithNetNSFd(routerFd))
	if err != nil {
		return fmt.Errorf("nft conn: %w", err)
	}

	table := c.AddTable(&nftables.Table{Family: nftables.TableFamilyINet, Name: nftTable})
	forward := addChain(c, table, "forward", nftables.ChainHookForward)
	input := addChain(c, table, "input", nftables.ChainHookInput)

	flowMap, err := addFlowMap(c, table, flows)
	if err != nil {
		return err
	}

	// forward: the intra-network flow matrix.
	c.AddRule(rule(forward, ctState(ctEstablished, ctRelated), accept()))
	c.AddRule(rule(forward, ctState(ctInvalid), drop()))
	c.AddRule(rule(forward, ctState(ctNew), flowVmap(flowMap)))

	// input: the router's own address is default-deny; allow lo, the ct return, icmp.
	c.AddRule(rule(input, iifname("lo"), accept()))
	c.AddRule(rule(input, ctState(ctEstablished, ctRelated), accept()))
	c.AddRule(rule(input, l4proto(unix.IPPROTO_ICMP), accept()))

	if err := c.Flush(); err != nil {
		return fmt.Errorf("nft flush: %w", err)
	}
	return nil
}

// addChain adds a base filter chain at hook with policy drop, priority filter (0).
func addChain(c *nftables.Conn, t *nftables.Table, name string, hook *nftables.ChainHook) *nftables.Chain {
	drop := nftables.ChainPolicyDrop
	return c.AddChain(&nftables.Chain{
		Name: name, Table: t,
		Type: nftables.ChainTypeFilter, Hooknum: hook,
		Priority: nftables.ChainPriorityFilter, Policy: &drop,
	})
}

// addFlowMap builds allowed_flows: ipv4 . ipv4 . inet_proto . inet_service -> verdict, one
// directional element per flow (the return path rides ct, so no reverse entry).
func addFlowMap(c *nftables.Conn, t *nftables.Table, flows []Flow) (*nftables.Set, error) {
	keyType, err := nftables.ConcatSetType(
		nftables.TypeIPAddr, nftables.TypeIPAddr, nftables.TypeInetProto, nftables.TypeInetService)
	if err != nil {
		return nil, fmt.Errorf("flow map key type: %w", err)
	}
	m := &nftables.Set{
		Name: "allowed_flows", Table: t,
		IsMap: true, KeyType: keyType, DataType: nftables.TypeVerdict,
	}
	elems := make([]nftables.SetElement, 0, len(flows))
	for _, f := range flows {
		key, err := flowKey(f)
		if err != nil {
			return nil, err
		}
		elems = append(elems, nftables.SetElement{
			Key:         key,
			VerdictData: &expr.Verdict{Kind: expr.VerdictAccept},
		})
	}
	if err := c.AddSet(m, elems); err != nil {
		return nil, fmt.Errorf("flow map: %w", err)
	}
	return m, nil
}

// --- a tiny expr DSL over google/nftables ----------------------------------
//
// google/nftables is the nf_tables bytecode level: a rule is a sequence of register-machine
// instructions (load a field into a register, compare it, ...) with no compiler to hide the
// registers the way `nft`/libnftables does. So we keep a thin layer here, with the register
// convention in one place. Convention: every SIMPLE match loads its field into register 1
// and compares it; a later match in the same rule just reuses register 1 (each is
// self-contained -- the load is consumed by its cmp). The one exception is the concatenated
// flow-map key (flowVmap), which must span contiguous registers; that's documented there.
//
// A frag is the expression fragment a helper contributes; rule() concatenates frags (the
// matches, then a verdict) into a Rule.

type frag = []expr.Any

func rule(chain *nftables.Chain, frags ...frag) *nftables.Rule {
	var exprs []expr.Any
	for _, f := range frags {
		exprs = append(exprs, f...)
	}
	return &nftables.Rule{Table: chain.Table, Chain: chain, Exprs: exprs}
}

// ctState matches `ct state in {bits}`: load ct state, mask it, test != 0.
func ctState(bits ...uint32) frag {
	var mask uint32
	for _, b := range bits {
		mask |= b
	}
	return frag{
		&expr.Ct{Register: 1, Key: expr.CtKeySTATE},
		&expr.Bitwise{SourceRegister: 1, DestRegister: 1, Len: 4, Mask: le32(mask), Xor: le32(0)},
		&expr.Cmp{Op: expr.CmpOpNeq, Register: 1, Data: le32(0)},
	}
}

// metaCmp is the shared "load a meta key into reg 1, compare it" primitive.
func metaCmp(key expr.MetaKey, data []byte) frag {
	return frag{
		&expr.Meta{Key: key, Register: 1},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: data},
	}
}

func iifname(name string) frag { return metaCmp(expr.MetaKeyIIFNAME, ifnameBytes(name)) }
func l4proto(proto byte) frag  { return metaCmp(expr.MetaKeyL4PROTO, []byte{proto}) }
func nfprotoIPv4() frag        { return metaCmp(expr.MetaKeyNFPROTO, []byte{unix.AF_INET}) }
func accept() frag             { return frag{&expr.Verdict{Kind: expr.VerdictAccept}} }
func drop() frag               { return frag{&expr.Verdict{Kind: expr.VerdictDrop}} }

// flowVmap matches (ip saddr . ip daddr . meta l4proto . th dport) against m and applies the
// mapped verdict. Two things nft's compiler hides but we must do by hand:
//   - the inet table needs `meta nfproto ipv4` before any ip-header payload load;
//   - the concat key must land in CONTIGUOUS registers 1,9,10,11 (NFT_REG_1 aliases
//     NFT_REG32_00, then 01/02/03) so the lookup reads a packed 16-byte key from reg 1 --
//     1,2,3,4 are SEPARATE 16-byte registers and leave 12 bytes uninitialized, which the
//     kernel rejects. This is the one place the "everything in reg 1" convention breaks.
func flowVmap(m *nftables.Set) frag {
	f := nfprotoIPv4()
	return append(f,
		&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseNetworkHeader, Offset: 12, Len: 4},   // ip saddr
		&expr.Payload{DestRegister: 9, Base: expr.PayloadBaseNetworkHeader, Offset: 16, Len: 4},   // ip daddr
		&expr.Meta{Key: expr.MetaKeyL4PROTO, Register: 10},                                        // l4proto
		&expr.Payload{DestRegister: 11, Base: expr.PayloadBaseTransportHeader, Offset: 2, Len: 2}, // th dport
		&expr.Lookup{SourceRegister: 1, SetName: m.Name, SetID: m.ID},
	)
}

// --- key encoding ----------------------------------------------------------

// flowKey is the concatenated map key: ipv4(4) . ipv4(4) . inet_proto(1, padded to 4) .
// inet_service(2 big-endian, padded to 4) = 16 bytes, matching the register layout flowVmap
// loads (verified against `nft`'s element encoding).
func flowKey(f Flow) ([]byte, error) {
	p, err := protoByte(f.Proto)
	if err != nil {
		return nil, err
	}
	key := make([]byte, 0, 16)
	from, to := f.FromIP.As4(), f.ToIP.As4()
	key = append(key, from[:]...)
	key = append(key, to[:]...)
	key = append(key, p, 0, 0, 0)
	var port [4]byte
	binary.BigEndian.PutUint16(port[:2], f.Port)
	key = append(key, port[:]...)
	return key, nil
}

func protoByte(p string) (byte, error) {
	switch p {
	case "tcp":
		return unix.IPPROTO_TCP, nil
	case "udp":
		return unix.IPPROTO_UDP, nil
	}
	return 0, fmt.Errorf("flow proto %q is not tcp/udp", p)
}

func ifnameBytes(name string) []byte {
	b := make([]byte, unix.IFNAMSIZ)
	copy(b, name)
	return b
}

func le32(v uint32) []byte {
	b := make([]byte, 4)
	binary.LittleEndian.PutUint32(b, v)
	return b
}
