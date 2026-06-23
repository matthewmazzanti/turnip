package config

import (
	"strings"
	"testing"
)

// The shipped example, inline so the test is self-contained. All policy is a flow now: the
// internal reachability, the two egress forms (scoped + the wide "any"), and the ingress DNAT.
const exampleJSON = `{
  "runtime": { "user": "homelab" },
  "containers": { "zwave": {}, "hass": {}, "proxy": {} },
  "networks": {
    "lan": {
      "gateway": "10.0.0.1",
      "gateway_if": "gw0",
      "uplink": { "host_if": "veth-lan-host", "router_if": "vethR-lan-up", "link": "169.254.1.0" },
      "attach": {
        "zwave": { "ip": "10.0.0.11", "interface": "eth0" },
        "hass":  { "ip": "10.0.0.12", "interface": "eth0" },
        "proxy": { "ip": "10.0.0.13", "interface": "eth0" }
      },
      "flows": [
        { "type": "internal", "from": "zwave", "to": "hass", "proto": "tcp", "port": 443 },
        { "type": "egress",   "from": "zwave", "proto": ["udp","tcp"], "port": 53 },
        { "type": "egress",   "from": "hass",  "proto": "any" },
        { "type": "ingress",  "to": "proxy",   "proto": "tcp", "host_port": 8443, "port": 443 }
      ]
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

	internal, ok := lan.Flows[0].(*InternalFlow)
	if !ok || internal.From != "zwave" || internal.To != "hass" {
		t.Errorf("flow[0] = %+v, want internal zwave->hass", lan.Flows[0])
	}
	zwEg, ok := lan.Flows[1].(*EgressFlow)
	if !ok || len(zwEg.Proto.List) != 2 || zwEg.Proto.List[0] != ProtoUDP || zwEg.Proto.List[1] != ProtoTCP {
		t.Errorf("flow[1] = %+v, want egress zwave [udp tcp]:53", lan.Flows[1])
	}
	hassEg, ok := lan.Flows[2].(*EgressFlow)
	if !ok || !hassEg.Proto.Any {
		t.Errorf("flow[2] = %+v, want egress hass proto=any", lan.Flows[2])
	}
	ing, ok := lan.Flows[3].(*IngressFlow)
	if !ok || ing.HostPort != 8443 || ing.Port != 443 || ing.To != "proxy" {
		t.Errorf("flow[3] = %+v, want ingress proxy 8443/443", lan.Flows[3])
	}
	if !fab.RequiresRoot() {
		t.Errorf("RequiresRoot = false, want true (the lan uplink)")
	}
}

func TestIngressPortDefaultsToHostPort(t *testing.T) {
	// no explicit container port -> defaults to host_port
	fab := mustParse(t, `{"containers":{"a":{}},"networks":{"n":{"gateway":"10.0.0.1","gateway_if":"gw0",
	  "uplink":{"host_if":"h","router_if":"r","link":"169.254.1.0"},
	  "attach":{"a":{"ip":"10.0.0.5","interface":"eth0"}},
	  "flows":[{"type":"ingress","to":"a","proto":"tcp","host_port":8080}]}}}`)
	ing := fab.Networks["n"].Flows[0].(*IngressFlow)
	if ing.Port != 8080 || ing.Listen.String() != "0.0.0.0" {
		t.Errorf("ingress defaults = port %d listen %s, want 8080 / 0.0.0.0", ing.Port, ing.Listen)
	}
}

func TestBridgeShapeAccepted(t *testing.T) {
	// edge flows (egress/ingress) are fine on a bridge; only internal flows are router-only.
	fab := mustParse(t, `{"containers":{"sensor":{}},"networks":{"iot":{"type":"bridge","subnet":"10.2.0.0/24",
	  "gateway":"10.2.0.1","uplink":{"host_if":"vh","router_if":"vr","link":"169.254.2.0"},
	  "attach":{"sensor":{"ip":"10.2.0.10","interface":"eth0"}},
	  "flows":[{"type":"egress","from":"sensor","proto":"any"}]}}}`)
	if fab.Networks["iot"].Type != NetworkBridge {
		t.Errorf("type = %q, want bridge", fab.Networks["iot"].Type)
	}
}

func TestLinksUnionShape(t *testing.T) {
	fab := mustParse(t, `{"containers":{"box":{"links":[
	  {"type":"veth","bridge":"br-lan","name":"eth2","address":"192.168.1.13/24","gateway":"192.168.1.1"},
	  {"type":"veth","peer":"host","name":"eth3","address":"192.168.50.2/30","default":true}
	]}},"networks":{}}`)
	links := fab.Containers["box"].Links
	vb, ok := links[0].(*VethLink)
	if !ok {
		t.Fatalf("link0 = %T, want *VethLink", links[0])
	}
	if vb.Bridge != "br-lan" {
		t.Errorf("veth bridge = %q, want br-lan", vb.Bridge)
	}
	vh := links[1].(*VethLink)
	if vh.Peer != "host" {
		t.Errorf("veth peer = %q, want host", vh.Peer)
	}
	if !fab.RequiresRoot() {
		t.Errorf("RequiresRoot = false, want true (a link)")
	}
}

func TestEgressAnyToken(t *testing.T) {
	fab := mustParse(t, flowCfg(`{"type":"egress","from":"a","proto":"tcp","port":"any"}`))
	if !fab.Networks["n"].Flows[0].(*EgressFlow).Port.Any {
		t.Errorf("egress port .Any = false, want true")
	}
}

func TestICMPEgressPortless(t *testing.T) {
	fab := mustParse(t, flowCfg(`{"type":"egress","from":"a","proto":"icmp"}`))
	if fab.Networks["n"].Flows[0].(*EgressFlow).Port != nil {
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
	parseErr(t, flowCfg(`{"type":"egress","from":"a","proto":"tcp"}`), "missing 'port'")
}

func TestEgressNeedsUplink(t *testing.T) {
	parseErr(t, `{"containers":{"a":{}},"networks":{"n":{"gateway":"10.0.0.1","gateway_if":"f0",
	  "attach":{"a":{"ip":"10.0.0.5","interface":"eth0"}},
	  "flows":[{"type":"egress","from":"a","proto":"any"}]}}}`, "needs this network's uplink")
}

func TestPortBounds(t *testing.T) {
	parseErr(t, flowCfg(`{"type":"ingress","to":"a","proto":"tcp","host_port":99999}`), "out of range")
}

func TestICMPIngressRejected(t *testing.T) {
	parseErr(t, flowCfg(`{"type":"ingress","to":"a","proto":"icmp","host_port":1}`), "port-bearing proto")
}

func TestUnknownFlowType(t *testing.T) {
	parseErr(t, flowCfg(`{"type":"sideways","from":"a","to":"b","proto":"tcp","port":1}`),
		"must be one of internal/egress/ingress")
}

func TestSubnetForbiddenOnRouter(t *testing.T) {
	parseErr(t, `{"containers":{},"networks":{"r":{"gateway":"10.0.0.1","gateway_if":"gw0","subnet":"10.0.0.0/24",
	  "uplink":{"host_if":"h","router_if":"r","link":"169.254.1.0"},"attach":{}}}}`, "subnet is forbidden on a router")
}

func TestInternalFlowForbiddenOnBridge(t *testing.T) {
	parseErr(t, `{"containers":{"a":{},"b":{}},"networks":{"b":{"type":"bridge","subnet":"10.2.0.0/24","gateway":"10.2.0.1",
	  "attach":{"a":{"ip":"10.2.0.5","interface":"eth0"},"b":{"ip":"10.2.0.6","interface":"eth0"}},
	  "flows":[{"type":"internal","from":"a","to":"b","proto":"tcp","port":1}]}}}`, "router-only")
}

func TestRouterRequiresGatewayIf(t *testing.T) {
	parseErr(t, `{"containers":{},"networks":{"r":{"gateway":"10.0.0.1"}}}`, "requires 'gateway_if'")
}

func TestFlowEndpointMustBeAttached(t *testing.T) {
	parseErr(t, `{"containers":{"a":{}},"networks":{"n":{"gateway":"10.0.0.1","gateway_if":"gw0",
	  "uplink":{"host_if":"h","router_if":"r","link":"169.254.1.0"},
	  "attach":{"a":{"ip":"10.0.0.5","interface":"eth0"}},
	  "flows":[{"type":"internal","from":"a","to":"ghost","proto":"tcp","port":1}]}}}`, "not attached to this network")
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
	parseErr(t, `{"containers":{"a":{"links":[{"type":"veth","peer":"host","name":"eth0","address":"1.2.3.4/24"}]}},
	  "networks":{"n":{"gateway":"10.0.0.1","gateway_if":"gw0","uplink":{"host_if":"h","router_if":"r","link":"169.254.1.0"},
	  "attach":{"a":{"ip":"10.0.0.5","interface":"eth0"}}}}}`, "duplicate interface")
}

func TestHostPortCollision(t *testing.T) {
	parseErr(t, `{"containers":{"a":{},"b":{}},"networks":{"n":{"gateway":"10.0.0.1","gateway_if":"gw0",
	  "uplink":{"host_if":"h","router_if":"r","link":"169.254.1.0"},"attach":{
	    "a":{"ip":"10.0.0.5","interface":"eth0"},
	    "b":{"ip":"10.0.0.6","interface":"eth0"}},
	  "flows":[
	    {"type":"ingress","to":"a","proto":"tcp","host_port":8443,"port":443},
	    {"type":"ingress","to":"b","proto":"tcp","host_port":8443,"port":444}]}}}`,
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

// flowCfg wraps a single flow in a minimal valid router network with an uplink and the
// containers a/b attached (the endpoints flows reference).
func flowCfg(flow string) string {
	return `{"containers":{"a":{},"b":{}},"networks":{"n":{"gateway":"10.0.0.1","gateway_if":"gw0",` +
		`"uplink":{"host_if":"h","router_if":"r","link":"169.254.1.0"},` +
		`"attach":{"a":{"ip":"10.0.0.5","interface":"eth0"},"b":{"ip":"10.0.0.6","interface":"eth0"}},` +
		`"flows":[` + flow + `]}}}`
}
