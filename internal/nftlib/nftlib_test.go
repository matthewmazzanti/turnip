package nftlib

import (
	"encoding/json"
	"testing"
)

func renderJSON(t *testing.T, n Node) string {
	t.Helper()
	b, err := json.Marshal(n.render())
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}

// Note: encoding/json sorts map keys, so the expected strings use alphabetical key order.
func TestRenderNodes(t *testing.T) {
	cases := []struct {
		name string
		node Node
		want string
	}{
		{"payload", Payload("ip", "saddr"), `{"payload":{"field":"saddr","protocol":"ip"}}`},
		{"meta", Meta("l4proto"), `{"meta":{"key":"l4proto"}}`},
		{"ct", Ct("state"), `{"ct":{"key":"state"}}`},
		{"match", Match(Meta("iifname"), "lo"),
			`{"match":{"left":{"meta":{"key":"iifname"}},"op":"==","right":"lo"}}`},
		{"ctstate-single", CtState("new"),
			`{"match":{"left":{"ct":{"key":"state"}},"op":"in","right":"new"}}`},
		{"ctstate-multi", CtState("established", "related"),
			`{"match":{"left":{"ct":{"key":"state"}},"op":"in","right":["established","related"]}}`},
		{"vmap", Vmap(Concat(Payload("ip", "saddr"), Meta("l4proto")), "allowed_flows"),
			`{"vmap":{"data":"@allowed_flows","key":{"concat":[{"payload":{"field":"saddr","protocol":"ip"}},{"meta":{"key":"l4proto"}}]}}}`},
		{"accept", Accept(), `{"accept":null}`},
		{"drop", Drop(), `{"drop":null}`},
		{"jump", Jump("nat"), `{"jump":{"target":"nat"}}`},
		{"masquerade", Masquerade(), `{"masquerade":null}`},
		{"dnat", Dnat("10.0.0.5", 443), `{"dnat":{"addr":"10.0.0.5","port":443}}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := renderJSON(t, c.node); got != c.want {
				t.Errorf("render =\n  %s\nwant\n  %s", got, c.want)
			}
		})
	}
}

func TestRenderTableObjects(t *testing.T) {
	tab := Table{Family: "inet", Name: "turnip"}

	if got := renderJSON(t, tab.Chain("forward", "filter", "forward", 0, "drop").(Node)); got !=
		`{"add":{"chain":{"family":"inet","hook":"forward","name":"forward","policy":"drop","prio":0,"table":"turnip","type":"filter"}}}` {
		t.Errorf("chain render = %s", got)
	}

	rule := tab.Rule("input", Match(Meta("l4proto"), "icmp"), Accept())
	if got := renderJSON(t, rule.(Node)); got !=
		`{"add":{"rule":{"chain":"input","expr":[{"match":{"left":{"meta":{"key":"l4proto"}},"op":"==","right":"icmp"}},{"accept":null}],"family":"inet","table":"turnip"}}}` {
		t.Errorf("rule render = %s", got)
	}
}

func TestRulesetWraps(t *testing.T) {
	rs := Rules(Table{Family: "inet", Name: "t"}.Add())
	b, err := rs.Render()
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if got := string(b); got != `{"nftables":[{"add":{"table":{"family":"inet","name":"t"}}}]}` {
		t.Errorf("ruleset = %s", got)
	}
}
