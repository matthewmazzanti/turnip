package config

import (
	"strings"
	"testing"
)

// The shipped example (old/tests/turnip.example.json), inline so the test is self-contained.
const exampleJSON = `{
  "runtime": { "user": "homelab" },
  "containers": { "zwave": {}, "hass": {}, "proxy": {} },
  "networks": {
    "lan": {
      "gateway": "10.0.0.1",
      "gateway_if": "gw0",
      "uplink": { "host_if": "veth-lan-host", "router_if": "vethR-lan-up", "link": "169.254.1.0" },
      "attach": {
        "zwave": { "ip": "10.0.0.11", "interface": "eth0", "egress": [ { "proto": ["udp", "tcp"], "port": 53 } ] },
        "hass":  { "ip": "10.0.0.12", "interface": "eth0", "egress": true },
        "proxy": { "ip": "10.0.0.13", "interface": "eth0", "ingress": [ { "proto": "tcp", "host_port": 8443, "port": 443 } ] }
      },
      "flows": [ { "from": "zwave", "to": "hass", "proto": "tcp", "port": 443 } ]
    }
  }
}`

func mustParse(t *testing.T, s string) *Turnip {
	t.Helper()
	cfg, err := Parse([]byte(s))
	if err != nil {
		t.Fatalf("Parse: unexpected error: %v", err)
	}
	return cfg
}

func parseErr(t *testing.T, s, want string) {
	t.Helper()
	_, err := Parse([]byte(s))
	if err == nil {
		t.Fatalf("expected error containing %q, got nil", want)
	}
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("error %q does not contain %q", err.Error(), want)
	}
}

// --- happy path ------------------------------------------------------------

func TestExampleLoads(t *testing.T) {
	fab := mustParse(t, exampleJSON)
	lan := fab.Networks["lan"]
	if lan.Type != NetworkRouter {
		t.Errorf("type = %q, want router", lan.Type)
	}
	if got := lan.Gateway.String(); got != "10.0.0.1" {
		t.Errorf("gateway = %q", got)
	}
	if got := lan.Uplink.Link.String(); got != "169.254.1.0" {
		t.Errorf("uplink.link = %q", got)
	}
	zw := lan.Attach["zwave"].Egress.Rules[0].Proto
	if len(zw) != 2 || zw[0] != ProtoUDP || zw[1] != ProtoTCP {
		t.Errorf("zwave egress proto = %v, want [udp tcp]", zw)
	}
	px := lan.Attach["proxy"].Ingress[0]
	if px.HostPort != 8443 || px.Port != 443 {
		t.Errorf("proxy ingress = host_port %d port %d, want 8443/443", px.HostPort, px.Port)
	}
	if lan.Flows[0].From != "zwave" || lan.Flows[0].To != "hass" {
		t.Errorf("flow = %s->%s", lan.Flows[0].From, lan.Flows[0].To)
	}
	if !fab.RequiresRoot() {
		t.Errorf("RequiresRoot = false, want true (the lan uplink)")
	}
}

func TestIngressPortDefaultsToHostPort(t *testing.T) {
	// no explicit container port -> defaults to host_port
	fab := mustParse(t, `{"containers":{"a":{}},"networks":{"n":{"gateway":"10.0.0.1","gateway_if":"gw0",
	  "uplink":{"host_if":"h","router_if":"r","link":"169.254.1.0"},
	  "attach":{"a":{"ip":"10.0.0.5","interface":"eth0","ingress":[{"proto":"tcp","host_port":8080}]}}}}}`)
	ing := fab.Networks["n"].Attach["a"].Ingress[0]
	if ing.Port != 8080 || ing.Listen.String() != "0.0.0.0" {
		t.Errorf("ingress defaults = port %d listen %s, want 8080 / 0.0.0.0", ing.Port, ing.Listen)
	}
}

func TestBridgeShapeAccepted(t *testing.T) {
	fab := mustParse(t, `{"containers":{"sensor":{}},"networks":{"iot":{"type":"bridge","subnet":"10.2.0.0/24",
	  "gateway":"10.2.0.1","uplink":{"host_if":"vh","router_if":"vr","link":"169.254.2.0"},
	  "attach":{"sensor":{"ip":"10.2.0.10","interface":"eth0","egress":true}}}}}`)
	if fab.Networks["iot"].Type != NetworkBridge {
		t.Errorf("type = %q, want bridge", fab.Networks["iot"].Type)
	}
}

func TestLinksUnionShape(t *testing.T) {
	fab := mustParse(t, `{"containers":{"box":{"links":[
	  {"type":"macvlan","parent":"eth0","name":"lan0","address":"192.168.1.12/24","gateway":"192.168.1.1"},
	  {"type":"veth","bridge":"br-lan","name":"eth2","address":"192.168.1.13/24"},
	  {"type":"phys","dev":"enp3s0","name":"eth3","address":"192.168.1.20/24","default":true}
	]}},"networks":{}}`)
	links := fab.Containers["box"].Links
	mv, ok := links[0].(*MacvlanLink)
	if !ok {
		t.Fatalf("link0 = %T, want *MacvlanLink", links[0])
	}
	if mv.Mode != MacvlanBridge {
		t.Errorf("macvlan mode = %q, want bridge (default)", mv.Mode)
	}
	if !fab.RequiresRoot() {
		t.Errorf("RequiresRoot = false, want true (a link)")
	}
}

func TestEgressAnyToken(t *testing.T) {
	fab := mustParse(t, netCfg(`"a":{"ip":"10.0.0.5","interface":"eth0","egress":[{"proto":"tcp","port":"any"}]}`))
	if !fab.Networks["n"].Attach["a"].Egress.Rules[0].Port.Any {
		t.Errorf("egress port .Any = false, want true")
	}
}

func TestICMPEgressPortless(t *testing.T) {
	fab := mustParse(t, netCfg(`"a":{"ip":"10.0.0.5","interface":"eth0","egress":[{"proto":"icmp"}]}`))
	if fab.Networks["n"].Attach["a"].Egress.Rules[0].Port != nil {
		t.Errorf("icmp egress port != nil")
	}
}

func TestRequiresRootFalseForPlainRouted(t *testing.T) {
	fab := mustParse(t, `{"containers":{"a":{}},"networks":{"n":{"gateway":"10.0.0.1","gateway_if":"gw0",
	  "attach":{"a":{"ip":"10.0.0.5","interface":"eth0"}}}}}`)
	if fab.RequiresRoot() {
		t.Errorf("RequiresRoot = true, want false (no uplink, no links)")
	}
}

// --- rejections ------------------------------------------------------------

func TestScopedEgressMissingPort(t *testing.T) {
	parseErr(t, netCfg(`"a":{"ip":"10.0.0.5","interface":"eth0","egress":[{"proto":"tcp"}]}`), "missing 'port'")
}

func TestEgressNeedsUplink(t *testing.T) {
	parseErr(t, `{"containers":{"a":{}},"networks":{"n":{"gateway":"10.0.0.1","gateway_if":"f0",
	  "attach":{"a":{"ip":"10.0.0.5","interface":"eth0","egress":true}}}}}`, "needs this network's uplink")
}

func TestPortBounds(t *testing.T) {
	parseErr(t, netCfg(`"a":{"ip":"10.0.0.5","interface":"eth0","ingress":[{"proto":"tcp","host_port":99999}]}`), "out of range")
}

func TestICMPIngressRejected(t *testing.T) {
	parseErr(t, netCfg(`"a":{"ip":"10.0.0.5","interface":"eth0","ingress":[{"proto":"icmp","host_port":1}]}`), "port-bearing proto")
}

func TestSubnetForbiddenOnRouter(t *testing.T) {
	parseErr(t, `{"containers":{},"networks":{"r":{"gateway":"10.0.0.1","gateway_if":"gw0","subnet":"10.0.0.0/24",
	  "uplink":{"host_if":"h","router_if":"r","link":"169.254.1.0"},"attach":{}}}}`, "subnet is forbidden on a router")
}

func TestFlowsForbiddenOnBridge(t *testing.T) {
	parseErr(t, `{"containers":{"a":{},"b":{}},"networks":{"b":{"type":"bridge","subnet":"10.2.0.0/24","gateway":"10.2.0.1",
	  "attach":{"a":{"ip":"10.2.0.5","interface":"eth0"},"b":{"ip":"10.2.0.6","interface":"eth0"}},
	  "flows":[{"from":"a","to":"b","proto":"tcp","port":1}]}}}`, "router-only")
}

func TestRouterRequiresGatewayIf(t *testing.T) {
	parseErr(t, `{"containers":{},"networks":{"r":{"gateway":"10.0.0.1"}}}`, "requires 'gateway_if'")
}

func TestFlowEndpointMustBeAttached(t *testing.T) {
	parseErr(t, `{"containers":{"a":{}},"networks":{"n":{"gateway":"10.0.0.1","gateway_if":"gw0",
	  "uplink":{"host_if":"h","router_if":"r","link":"169.254.1.0"},
	  "attach":{"a":{"ip":"10.0.0.5","interface":"eth0"}},
	  "flows":[{"from":"a","to":"ghost","proto":"tcp","port":1}]}}}`, "not attached to this network")
}

func TestUnknownContainerInAttach(t *testing.T) {
	parseErr(t, netCfg(`"ghost":{"ip":"10.0.0.5","interface":"eth0"}`), "unknown container")
}

func TestTwoDefaultsRejected(t *testing.T) {
	parseErr(t, `{"containers":{"a":{}},"networks":{
	  "n1":{"gateway":"10.0.0.1","gateway_if":"f0","attach":{"a":{"ip":"10.0.0.5","interface":"eth0","default":true}}},
	  "n2":{"gateway":"10.1.0.1","gateway_if":"f1","attach":{"a":{"ip":"10.1.0.5","interface":"eth1","default":true}}}}}`,
		"marked default; pick one")
}

func TestZeroDefaultMultiIface(t *testing.T) {
	parseErr(t, `{"containers":{"a":{}},"networks":{
	  "n1":{"gateway":"10.0.0.1","gateway_if":"f0","attach":{"a":{"ip":"10.0.0.5","interface":"eth0"}}},
	  "n2":{"gateway":"10.1.0.1","gateway_if":"f1","attach":{"a":{"ip":"10.1.0.5","interface":"eth1"}}}}}`,
		"none marked default")
}

func TestDuplicateInterface(t *testing.T) {
	parseErr(t, `{"containers":{"a":{"links":[{"type":"phys","dev":"x","name":"eth0","address":"1.2.3.4/24"}]}},
	  "networks":{"n":{"gateway":"10.0.0.1","gateway_if":"gw0","uplink":{"host_if":"h","router_if":"r","link":"169.254.1.0"},
	  "attach":{"a":{"ip":"10.0.0.5","interface":"eth0"}}}}}`, "duplicate interface")
}

func TestHostPortCollision(t *testing.T) {
	parseErr(t, `{"containers":{"a":{},"b":{}},"networks":{"n":{"gateway":"10.0.0.1","gateway_if":"gw0",
	  "uplink":{"host_if":"h","router_if":"r","link":"169.254.1.0"},"attach":{
	    "a":{"ip":"10.0.0.5","interface":"eth0","ingress":[{"proto":"tcp","host_port":8443,"port":443}]},
	    "b":{"ip":"10.0.0.6","interface":"eth0","ingress":[{"proto":"tcp","host_port":8443,"port":444}]}}}}}`,
		"host_port collision")
}

func TestUplinkMustBeEvenBase(t *testing.T) {
	parseErr(t, `{"containers":{},"networks":{"n":{"gateway":"10.0.0.1","gateway_if":"gw0",
	  "uplink":{"host_if":"h","router_if":"r","link":"169.254.1.1"},"attach":{}}}}`, "even base of a /31")
}

func TestUplinkRejectsPrefix(t *testing.T) {
	// "169.254.1.0/31" is not a bare IPv4 address; netip rejects it at parse time.
	parseErr(t, `{"containers":{},"networks":{"n":{"gateway":"10.0.0.1","gateway_if":"gw0",
	  "uplink":{"host_if":"h","router_if":"r","link":"169.254.1.0/31"},"attach":{}}}}`, "parse turnip.json")
}

func TestIfnameLengthCapped(t *testing.T) {
	parseErr(t, `{"containers":{},"networks":{"n":{"gateway":"10.0.0.1","gateway_if":"this-name-is-too-long",
	  "uplink":{"host_if":"h","router_if":"r","link":"169.254.1.0"},"attach":{}}}}`, "1-15 chars")
}

func TestExtraKeyForbidden(t *testing.T) {
	parseErr(t, `{"containers":{},"networks":{"n":{"gateway":"10.0.0.1","gateway_if":"gw0",
	  "uplink":{"host_if":"h","router_if":"r","link":"169.254.1.0"},"attach":{},"egres":true}}}`, "unknown field")
}

func TestBadEnumValue(t *testing.T) {
	parseErr(t, `{"containers":{},"networks":{"n":{"type":"switch","gateway":"10.0.0.1"}}}`, "'router' or 'bridge'")
}

// netCfg wraps a single attach entry in a minimal valid router network with an uplink
// (container "a"/"ghost" declared as needed by the caller's attach key).
func netCfg(attach string) string {
	containers := `{"a":{}}`
	if strings.Contains(attach, `"ghost"`) {
		containers = `{}`
	}
	return `{"containers":` + containers + `,"networks":{"n":{"gateway":"10.0.0.1","gateway_if":"gw0",` +
		`"uplink":{"host_if":"h","router_if":"r","link":"169.254.1.0"},"attach":{` + attach + `}}}}`
}
