// Package nftlib is a small, use-case-agnostic renderer for libnftables JSON. Each typed
// node (payload/meta/ct/concat/match/vmap/verdict/...) renders to its libnftables JSON
// shape; Load feeds the whole ruleset to `nft -j -f -`. This delegates rule COMPILATION
// (register allocation, set/concat layout, the nfproto guards) to libnftables, so callers
// express rules at the nft-semantic level rather than the kernel bytecode level.
//
// Ported from old/src/turnip/nftlib.py. The constructors are the ergonomic API (Payload,
// Meta, Match, CtState, Vmap, Accept, ...); the concrete node types stay unexported.
//
// Load runs `nft` in the CURRENT netns, so call it from inside a netns episode
// (netns.Set.Enter) to land in the target router netns.
package nftlib

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
)

type obj = map[string]any

// Node is anything that renders to a libnftables JSON value.
type Node interface{ render() any }

// renderValue renders a Value -- a Node, a literal (string/int/bool), or a list of them.
func renderValue(v any) any {
	switch x := v.(type) {
	case Node:
		return x.render()
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = renderValue(e)
		}
		return out
	default:
		return x // string / int / bool literal
	}
}

// --- expressions -----------------------------------------------------------

type payload struct{ protocol, field string }

func (p payload) render() any { return obj{"payload": obj{"protocol": p.protocol, "field": p.field}} }

// Payload references a header field -- Payload("ip","saddr"), Payload("th","dport").
func Payload(protocol, field string) Node { return payload{protocol, field} }

type meta struct{ key string }

func (m meta) render() any { return obj{"meta": obj{"key": m.key}} }

// Meta references a meta selector -- Meta("l4proto"), Meta("iifname").
func Meta(key string) Node { return meta{key} }

type ct struct{ key string }

func (c ct) render() any { return obj{"ct": obj{"key": c.key}} }

// Ct references a conntrack selector -- Ct("state").
func Ct(key string) Node { return ct{key} }

type concat struct{ parts []any }

func (c concat) render() any {
	parts := make([]any, len(c.parts))
	for i, p := range c.parts {
		parts[i] = renderValue(p)
	}
	return obj{"concat": parts}
}

// Concat joins operands/literals into a compound key -- a . b . c.
func Concat(parts ...any) Node { return concat{parts} }

type matchExpr struct {
	left  Node
	right any
	op    string
}

func (m matchExpr) render() any {
	return obj{"match": obj{"op": m.op, "left": m.left.render(), "right": renderValue(m.right)}}
}

// Match is `left == right` (right is an operand, literal, or set).
func Match(left Node, right any) Node { return matchExpr{left, right, "=="} }

// MatchOp is Match with an explicit operator (e.g. "!=", "<", ">=").
func MatchOp(left Node, right any, op string) Node { return matchExpr{left, right, op} }

// CtState matches `ct state` against one or more states (op "in", as nft requires): a
// single state renders as a scalar, multiple as a list -- matching nft's own JSON.
func CtState(states ...string) Node {
	var right any
	if len(states) == 1 {
		right = states[0]
	} else {
		r := make([]any, len(states))
		for i, s := range states {
			r[i] = s
		}
		right = r
	}
	return matchExpr{ct{"state"}, right, "in"}
}

type vmap struct {
	key     Node
	setName string
}

func (v vmap) render() any { return obj{"vmap": obj{"key": v.key.render(), "data": "@" + v.setName}} }

// Vmap looks key up in a named verdict map and applies the mapped verdict.
func Vmap(key Node, setName string) Node { return vmap{key, setName} }

type verdict struct{ kind, target string }

func (v verdict) render() any {
	if v.target != "" {
		return obj{v.kind: obj{"target": v.target}}
	}
	return obj{v.kind: nil}
}

func Accept() Node            { return verdict{"accept", ""} }
func Drop() Node              { return verdict{"drop", ""} }
func Jump(target string) Node { return verdict{"jump", target} }
func Goto(target string) Node { return verdict{"goto", target} }

type masquerade struct{}

func (masquerade) render() any { return obj{"masquerade": nil} }

// Masquerade is the `masquerade` nat statement (SNAT to the outgoing interface address).
func Masquerade() Node { return masquerade{} }

type dnat struct {
	addr string
	port int
}

func (d dnat) render() any { return obj{"dnat": obj{"addr": d.addr, "port": d.port}} }

// Dnat is the `dnat` nat statement (rewrite the destination to addr:port).
func Dnat(addr string, port int) Node { return dnat{addr, port} }

// --- objects + commands ----------------------------------------------------

type chain struct {
	family, table, name string
	typ, hook           string
	prio                int
	hasPrio             bool
	policy              string
}

func (c chain) render() any {
	spec := obj{"family": c.family, "table": c.table, "name": c.name}
	if c.typ != "" {
		spec["type"] = c.typ
	}
	if c.hook != "" {
		spec["hook"] = c.hook
	}
	if c.hasPrio {
		spec["prio"] = c.prio
	}
	if c.policy != "" {
		spec["policy"] = c.policy
	}
	return obj{"chain": spec}
}

type verdictMap struct {
	family, table, name string
	keyType             []string
	elems               [][2]Node
}

func (m verdictMap) render() any {
	spec := obj{"family": m.family, "table": m.table, "name": m.name, "map": "verdict", "type": m.keyType}
	// Omit `elem` when empty: nft rejects "elem": [], but a map with no elements is valid.
	if len(m.elems) > 0 {
		elems := make([]any, len(m.elems))
		for i, kv := range m.elems {
			elems[i] = []any{kv[0].render(), kv[1].render()}
		}
		spec["elem"] = elems
	}
	return obj{"map": spec}
}

type ruleObj struct {
	family, table, chain string
	exprs                []Node
}

func (r ruleObj) render() any {
	exprs := make([]any, len(r.exprs))
	for i, e := range r.exprs {
		exprs[i] = e.render()
	}
	return obj{"rule": obj{"family": r.family, "table": r.table, "chain": r.chain, "expr": exprs}}
}

type addCmd struct{ o Node }

func (a addCmd) render() any { return obj{"add": a.o.render()} }

type deleteCmd struct{ o Node }

func (d deleteCmd) render() any { return obj{"delete": d.o.render()} }

// --- the Table context -----------------------------------------------------

// Table is a bound (family, name) table -- both a renderable object and the context that
// stamps family/table (and chain) onto the chains, maps, and rules its methods build.
type Table struct{ Family, Name string }

func (t Table) render() any { return obj{"table": obj{"family": t.Family, "name": t.Name}} }

// Add adds the table object itself.
func (t Table) Add() Node { return addCmd{t} }

// Delete deletes the table (and everything in it). Pair with Add for an idempotent flush:
// Rules(t.Add(), t.Delete()) removes the table whether or not it existed.
func (t Table) Delete() Node { return deleteCmd{t} }

// Reload is the flush-and-reload triple: add (ensure exists) / delete / add -- so applying
// it replaces the whole table atomically and idempotently, even on a fresh netns.
func (t Table) Reload() []Node { return []Node{addCmd{t}, deleteCmd{t}, addCmd{t}} }

// Chain adds a base chain (type+hook+prio+policy). For a regular jump-target chain, pass an
// empty hook (prio/policy are then ignored).
func (t Table) Chain(name, typ, hook string, prio int, policy string) Node {
	return addCmd{
		chain{
			family:  t.Family,
			table:   t.Name,
			name:    name,
			typ:     typ,
			hook:    hook,
			prio:    prio,
			hasPrio: hook != "",
			policy:  policy,
		},
	}
}

// Map adds a named verdict map with the given key type and (key, verdict) elements.
func (t Table) Map(name string, keyType []string, elems [][2]Node) Node {
	return addCmd{
		verdictMap{
			family:  t.Family,
			table:   t.Name,
			name:    name,
			keyType: keyType,
			elems:   elems,
		},
	}
}

// Rule adds a rule (a list of expression nodes) to chainName.
func (t Table) Rule(chainName string, exprs ...Node) Node {
	return addCmd{
		ruleObj{
			family: t.Family,
			table:  t.Name,
			chain:  chainName,
			exprs:  exprs,
		},
	}
}

// --- ruleset + load --------------------------------------------------------

// Ruleset is a top-level command batch; it renders to {"nftables": [...]}.
type Ruleset struct{ commands []Node }

// Rules assembles a ruleset from commands (in order -- maps must precede the rules using them).
func Rules(commands ...Node) Ruleset { return Ruleset{commands} }

func (r Ruleset) render() any {
	cmds := make([]any, len(r.commands))
	for i, c := range r.commands {
		cmds[i] = c.render()
	}
	return obj{"nftables": cmds}
}

// Render returns the libnftables JSON for the ruleset.
func (r Ruleset) Render() ([]byte, error) { return json.Marshal(r.render()) }

// Load renders the ruleset and applies it via `nft -j -f -`. nft acts on the CURRENT
// process's netns, so call this from inside a netns episode (netns.Set.Enter) to land in
// the target router netns.
func Load(r Ruleset) error {
	data, err := r.Render()
	if err != nil {
		return fmt.Errorf("render nft: %w", err)
	}
	bin, err := findNft()
	if err != nil {
		return err
	}
	cmd := exec.Command(bin, "-j", "-f", "-")
	cmd.Stdin = bytes.NewReader(data)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("nft load: %w: %s", err, stderr.String())
	}
	return nil
}

// findNft locates the nft binary -- PATH first, then NixOS / common fallbacks (a sudo
// secure_path may drop the rootless user's PATH entries).
func findNft() (string, error) {
	if p, err := exec.LookPath("nft"); err == nil {
		return p, nil
	}
	for _, c := range []string{
		"/run/current-system/sw/bin/nft", "/usr/sbin/nft", "/usr/bin/nft", "/sbin/nft",
	} {
		if _, err := os.Stat(c); err == nil {
			return c, nil
		}
	}
	return "", fmt.Errorf("nft binary not found (PATH or common locations)")
}
