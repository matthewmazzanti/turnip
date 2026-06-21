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
// the register, so the masks below are LE uint32s.
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
// builds -- no flush/reload. Ports main.py build_nft (sans the still-deferred uplink edge).
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

	drop := nftables.ChainPolicyDrop
	forward := c.AddChain(&nftables.Chain{
		Name: "forward", Table: table,
		Type: nftables.ChainTypeFilter, Hooknum: nftables.ChainHookForward,
		Priority: nftables.ChainPriorityFilter, Policy: &drop,
	})
	input := c.AddChain(&nftables.Chain{
		Name: "input", Table: table,
		Type: nftables.ChainTypeFilter, Hooknum: nftables.ChainHookInput,
		Priority: nftables.ChainPriorityFilter, Policy: &drop,
	})

	// allowed_flows: ipv4 . ipv4 . inet_proto . inet_service -> verdict. One element per
	// flow (DIRECTIONAL; the return path rides ct, so no reverse entry).
	keyType, err := nftables.ConcatSetType(
		nftables.TypeIPAddr, nftables.TypeIPAddr, nftables.TypeInetProto, nftables.TypeInetService)
	if err != nil {
		return fmt.Errorf("flow map key type: %w", err)
	}
	flowMap := &nftables.Set{
		Name: "allowed_flows", Table: table,
		IsMap: true, KeyType: keyType, DataType: nftables.TypeVerdict,
	}
	elems := make([]nftables.SetElement, 0, len(flows))
	for _, f := range flows {
		key, err := flowKey(f)
		if err != nil {
			return err
		}
		elems = append(elems, nftables.SetElement{
			Key:         key,
			VerdictData: &expr.Verdict{Kind: expr.VerdictAccept},
		})
	}
	if err := c.AddSet(flowMap, elems); err != nil {
		return fmt.Errorf("flow map: %w", err)
	}

	// forward chain
	c.AddRule(&nftables.Rule{Table: table, Chain: forward,
		Exprs: append(ctStateMatch(ctEstablished|ctRelated), accept())})
	c.AddRule(&nftables.Rule{Table: table, Chain: forward,
		Exprs: append(ctStateMatch(ctInvalid), dropV())})
	c.AddRule(&nftables.Rule{Table: table, Chain: forward, Exprs: flowVmap(flowMap)})

	// input chain
	c.AddRule(&nftables.Rule{Table: table, Chain: input,
		Exprs: append(iifname("lo"), accept())})
	c.AddRule(&nftables.Rule{Table: table, Chain: input,
		Exprs: append(ctStateMatch(ctEstablished|ctRelated), accept())})
	c.AddRule(&nftables.Rule{Table: table, Chain: input,
		Exprs: append(l4proto(unix.IPPROTO_ICMP), accept())})

	if err := c.Flush(); err != nil {
		return fmt.Errorf("nft flush: %w", err)
	}
	return nil
}

// --- expression helpers ----------------------------------------------------

// ctStateMatch loads ct state, masks it, and tests != 0 -- i.e. `ct state in {mask bits}`.
func ctStateMatch(mask uint32) []expr.Any {
	return []expr.Any{
		&expr.Ct{Register: 1, Key: expr.CtKeySTATE},
		&expr.Bitwise{SourceRegister: 1, DestRegister: 1, Len: 4, Mask: le32(mask), Xor: le32(0)},
		&expr.Cmp{Op: expr.CmpOpNeq, Register: 1, Data: le32(0)},
	}
}

// flowVmap is the `ct state new` guard + the vmap of (ip saddr . ip daddr . meta l4proto .
// th dport) into allowed_flows. Two subtleties nft hides but google/nftables doesn't:
//   - the inet table needs `meta nfproto ipv4` before any ip-header payload load;
//   - the concat key must land in CONTIGUOUS registers 1,9,10,11 (NFT_REG_1 aliases
//     NFT_REG32_00, then 32_01/02/03) so the lookup reads a packed 16-byte key from reg 1.
//     (Using 1,2,3,4 leaves 12 bytes of the key uninitialized -> the kernel rejects it.)
func flowVmap(m *nftables.Set) []expr.Any {
	return []expr.Any{
		&expr.Ct{Register: 1, Key: expr.CtKeySTATE},
		&expr.Bitwise{SourceRegister: 1, DestRegister: 1, Len: 4, Mask: le32(ctNew), Xor: le32(0)},
		&expr.Cmp{Op: expr.CmpOpNeq, Register: 1, Data: le32(0)},
		&expr.Meta{Key: expr.MetaKeyNFPROTO, Register: 1},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: []byte{unix.AF_INET}},
		&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseNetworkHeader, Offset: 12, Len: 4},   // ip saddr
		&expr.Payload{DestRegister: 9, Base: expr.PayloadBaseNetworkHeader, Offset: 16, Len: 4},   // ip daddr
		&expr.Meta{Key: expr.MetaKeyL4PROTO, Register: 10},                                        // l4proto
		&expr.Payload{DestRegister: 11, Base: expr.PayloadBaseTransportHeader, Offset: 2, Len: 2}, // th dport
		&expr.Lookup{SourceRegister: 1, SetName: m.Name, SetID: m.ID},
	}
}

func iifname(name string) []expr.Any {
	return []expr.Any{
		&expr.Meta{Key: expr.MetaKeyIIFNAME, Register: 1},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: ifnameBytes(name)},
	}
}

func l4proto(proto byte) []expr.Any {
	return []expr.Any{
		&expr.Meta{Key: expr.MetaKeyL4PROTO, Register: 1},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: []byte{proto}},
	}
}

func accept() expr.Any { return &expr.Verdict{Kind: expr.VerdictAccept} }
func dropV() expr.Any  { return &expr.Verdict{Kind: expr.VerdictDrop} }

// --- key encoding ----------------------------------------------------------

// flowKey is the concatenated map key: ipv4(4) . ipv4(4) . inet_proto(1, padded to 4) .
// inet_service(2 big-endian, padded to 4) = 16 bytes, matching the register layout the
// vmap loads.
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
