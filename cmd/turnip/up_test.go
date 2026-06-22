// Unit tests for the pure config->dataplane lowering in plan.go: the buildPlan helpers
// that turn the parsed config into netns specs, link specs, endpoint defaults, flows, host
// edge allows, and the generated /etc/hosts. No netns, no netlink, no root -- just the
// transforms. Inputs are built through config.Parse so the tests exercise real config shapes
// (black-box: expectations are hand-authored here, never read back out of the same lowering).

package main

import (
	"net/netip"
	"reflect"
	"strings"
	"testing"

	"git.lan/mmazzanti/turnip/internal/config"
	"git.lan/mmazzanti/turnip/internal/dataplane"
)

func mustParse(t *testing.T, s string) *config.Turnip {
	t.Helper()
	cfg, err := config.Parse([]byte(s))
	if err != nil {
		t.Fatalf("config.Parse: %v", err)
	}
	return cfg
}

// --- netnsSpecs -------------------------------------------------------------

func TestNetnsSpecs(t *testing.T) {
	cfg := mustParse(t, `{"containers":{"zwave":{},"hass":{}},"networks":{
	  "lan":{"gateway":"10.0.0.1","gateway_if":"gw0","attach":{
	    "zwave":{"ip":"10.0.0.11","interface":"eth0"},
	    "hass":{"ip":"10.0.0.12","interface":"eth0"}}}}}`)
	specs := netnsSpecs(cfg, "/run/user/1001/turnip")

	want := map[string]string{
		"router:lan":      "/run/user/1001/turnip/routers/lan",
		"container:hass":  "/run/user/1001/turnip/containers/hass/netns",
		"container:zwave": "/run/user/1001/turnip/containers/zwave/netns",
	}
	if len(specs) != len(want) {
		t.Fatalf("got %d specs, want %d: %+v", len(specs), len(want), specs)
	}
	for _, s := range specs {
		if want[s.Name] != s.Path {
			t.Errorf("%s -> %q, want %q", s.Name, s.Path, want[s.Name])
		}
	}
	// routers sort before containers? No -- sortedKeys is per-category; assert deterministic
	// order: networks first (sorted), then containers (sorted).
	var order []string
	for _, s := range specs {
		order = append(order, s.Name)
	}
	wantOrder := []string{"router:lan", "container:hass", "container:zwave"}
	if !reflect.DeepEqual(order, wantOrder) {
		t.Errorf("spec order = %v, want %v", order, wantOrder)
	}
}

// --- interfaceCounts + ownsDefault -----------------------------------------

func TestInterfaceCounts(t *testing.T) {
	// hass: one attachment (sole iface). box: a (default) link + an attachment (two ifaces;
	// the validator requires exactly one marked default once a container is multi-homed).
	cfg := mustParse(t, `{"containers":{"hass":{},
	  "box":{"links":[{"type":"phys","dev":"enp3s0","name":"eth9","address":"192.168.9.10/24","default":true}]}},
	  "networks":{"lan":{"gateway":"10.0.0.1","gateway_if":"gw0","attach":{
	    "hass":{"ip":"10.0.0.12","interface":"eth0"},
	    "box":{"ip":"10.0.0.20","interface":"eth0"}}}}}`)
	counts := interfaceCounts(cfg)
	if counts["hass"] != 1 {
		t.Errorf("hass count = %d, want 1", counts["hass"])
	}
	if counts["box"] != 2 {
		t.Errorf("box count = %d, want 2 (link + attach)", counts["box"])
	}
}

// ownsDefault is pure -- test the resolution directly (a valid config can't even express
// "multi-homed, none marked", since the validator rejects it, so cover it here).
func TestOwnsDefault(t *testing.T) {
	if !ownsDefault(false, 1) {
		t.Errorf("sole interface should implicitly own the default route")
	}
	if ownsDefault(false, 2) {
		t.Errorf("multi-homed + not marked should NOT own the default route")
	}
	if !ownsDefault(true, 2) {
		t.Errorf("explicit default should own the route even when multi-homed")
	}
}

// --- buildFlows -------------------------------------------------------------

func TestBuildFlows(t *testing.T) {
	cfg := mustParse(t, `{"containers":{"a":{},"b":{}},"networks":{"n":{"gateway":"10.0.0.1","gateway_if":"gw0",
	  "attach":{"a":{"ip":"10.0.0.5","interface":"eth0"},"b":{"ip":"10.0.0.6","interface":"eth0"}},
	  "flows":[{"from":"a","to":"b","proto":"tcp","port":443}]}}}`)
	flows, err := buildFlows(cfg.Networks["n"])
	if err != nil {
		t.Fatalf("buildFlows: %v", err)
	}
	if len(flows) != 1 {
		t.Fatalf("got %d flows, want 1", len(flows))
	}
	got := flows[0]
	want := dataplane.Flow{FromIP: mustAddrT(t, "10.0.0.5"), ToIP: mustAddrT(t, "10.0.0.6"), Proto: "tcp", Port: 443}
	if got != want {
		t.Errorf("flow = %+v, want %+v", got, want)
	}
}

func TestBuildFlowsRejectsUnwired(t *testing.T) {
	// icmp and port="any" both need a second nft map shape that isn't wired yet.
	for _, flow := range []string{
		`{"from":"a","to":"b","proto":"icmp","port":1}`,
		`{"from":"a","to":"b","proto":"tcp","port":"any"}`,
	} {
		cfg := mustParse(t, `{"containers":{"a":{},"b":{}},"networks":{"n":{"gateway":"10.0.0.1","gateway_if":"gw0",
		  "attach":{"a":{"ip":"10.0.0.5","interface":"eth0"},"b":{"ip":"10.0.0.6","interface":"eth0"}},
		  "flows":[`+flow+`]}}}`)
		if _, err := buildFlows(cfg.Networks["n"]); err == nil {
			t.Errorf("flow %s: expected an error, got nil", flow)
		}
	}
}

// --- buildLinkSpec(s) -------------------------------------------------------

func TestBuildLinkSpecs(t *testing.T) {
	cfg := mustParse(t, `{"containers":{"box":{"links":[
	  {"type":"veth","bridge":"br-lan","name":"eth0","address":"192.168.50.10/24"},
	  {"type":"veth","peer":"host","name":"eth1","address":"10.9.0.2/30"},
	  {"type":"macvlan","parent":"eth0","name":"lan0","address":"192.168.1.12/24","mode":"private"},
	  {"type":"ipvlan","parent":"eth1","name":"lan1","address":"192.168.2.12/24"},
	  {"type":"phys","dev":"enp3s0","name":"eth9","address":"192.168.9.10/24","default":true}
	]}},"networks":{}}`)
	specs, err := buildLinkSpecs(cfg)
	if err != nil {
		t.Fatalf("buildLinkSpecs: %v", err)
	}
	box := specs["box"]
	if len(box) != 5 {
		t.Fatalf("got %d specs, want 5", len(box))
	}

	// kind + the host-side anchor each kind selects.
	checks := []struct {
		i              int
		kind, hostIf   string
		bridge, parent string
		mode, dev      string
	}{
		{0, "veth-bridge", "vethL-box-eth0", "br-lan", "", "", ""},
		{1, "veth-host", "vethL-box-eth1", "", "", "", ""},
		{2, "macvlan", "", "", "eth0", "private", ""},
		{3, "ipvlan", "", "", "eth1", "l2", ""},
		{4, "phys", "", "", "", "", "enp3s0"},
	}
	for _, c := range checks {
		s := box[c.i]
		if s.Kind != c.kind || s.HostIf != c.hostIf || s.Bridge != c.bridge ||
			s.Parent != c.parent || s.Mode != c.mode || s.Dev != c.dev {
			t.Errorf("spec[%d] = %+v, want kind=%q hostIf=%q bridge=%q parent=%q mode=%q dev=%q",
				c.i, s, c.kind, c.hostIf, c.bridge, c.parent, c.mode, c.dev)
		}
	}
	// phys carries default + the parsed address through.
	if !box[4].Default || box[4].Address.String() != "192.168.9.10/24" {
		t.Errorf("phys spec = %+v, want default + 192.168.9.10/24", box[4])
	}
}

func TestBuildLinkSpecMTU(t *testing.T) {
	cfg := mustParse(t, `{"containers":{"box":{"links":[
	  {"type":"veth","peer":"host","name":"eth0","address":"10.9.0.2/30","mtu":9000}]}},"networks":{}}`)
	spec := mustParse2LinkSpec(t, cfg)
	if spec.MTU != 9000 {
		t.Errorf("MTU = %d, want 9000", spec.MTU)
	}
}

func TestBuildLinkSpecNoMTU(t *testing.T) {
	cfg := mustParse(t, `{"containers":{"box":{"links":[
	  {"type":"veth","peer":"host","name":"eth0","address":"10.9.0.2/30"}]}},"networks":{}}`)
	spec := mustParse2LinkSpec(t, cfg)
	if spec.MTU != 0 {
		t.Errorf("MTU = %d, want 0 (unset)", spec.MTU)
	}
}

// --- routerIf / linkHostIf IFNAMSIZ rejection ------------------------------

func TestRouterIf(t *testing.T) {
	if got, err := routerIf("zwave"); err != nil || got != "vethR-zwave" {
		t.Errorf("routerIf(zwave) = %q, %v; want vethR-zwave, nil", got, err)
	}
	// "vethR-" is 6 chars; 9 more = 15 (the cap). 10 more = 16 (over).
	if _, err := routerIf(strings.Repeat("x", 9)); err != nil {
		t.Errorf("9-char container should fit IFNAMSIZ: %v", err)
	}
	if _, err := routerIf(strings.Repeat("x", 10)); err == nil {
		t.Errorf("10-char container should exceed IFNAMSIZ (15)")
	}
}

func TestLinkHostIf(t *testing.T) {
	// "vethL-" (6) + container + "-" + link must be <= 15.
	if got, err := linkHostIf("a", "b"); err != nil || got != "vethL-a-b" {
		t.Errorf("linkHostIf(a,b) = %q, %v; want vethL-a-b, nil", got, err)
	}
	if _, err := linkHostIf("container", "link"); err == nil {
		t.Errorf("over-long link host name should be rejected")
	}
}

// --- buildEgressAllows ------------------------------------------------------

func TestBuildEgressAllows(t *testing.T) {
	cfg := mustParse(t, `{"containers":{"out":{},"scoped":{},"quiet":{}},"networks":{"lan":{
	  "gateway":"10.0.0.1","gateway_if":"gw0",
	  "uplink":{"host_if":"h","router_if":"r","link":"169.254.1.0"},
	  "attach":{
	    "out":{"ip":"10.0.0.11","interface":"eth0","egress":true},
	    "scoped":{"ip":"10.0.0.12","interface":"eth0","egress":[{"proto":["udp","tcp"],"port":53}]},
	    "quiet":{"ip":"10.0.0.13","interface":"eth0"}}}}}`)
	allows := buildEgressAllows(cfg.Networks["lan"])

	// quiet has no egress -> not present. out is All. scoped has one (udp,tcp):53 rule.
	byIP := map[string]dataplane.EgressAllow{}
	for _, a := range allows {
		byIP[a.IP.String()] = a
	}
	if len(allows) != 2 {
		t.Fatalf("got %d allows, want 2 (quiet excluded): %+v", len(allows), allows)
	}
	if !byIP["10.0.0.11"].All {
		t.Errorf("out should be All egress")
	}
	sc := byIP["10.0.0.12"]
	if sc.All || len(sc.Rules) != 1 || sc.Rules[0].Port != 53 ||
		!reflect.DeepEqual(sc.Rules[0].Protos, []string{"udp", "tcp"}) {
		t.Errorf("scoped allow = %+v, want one (udp,tcp):53 rule", sc)
	}
}

// --- buildIngress -----------------------------------------------------------

func TestBuildIngress(t *testing.T) {
	cfg := mustParse(t, `{"containers":{"svc":{}},"networks":{"lan":{
	  "gateway":"10.0.0.1","gateway_if":"gw0",
	  "uplink":{"host_if":"h","router_if":"r","link":"169.254.1.0"},
	  "attach":{"svc":{"ip":"10.0.0.13","interface":"eth0",
	    "ingress":[{"proto":"tcp","host_port":8080,"port":80}]}}}}}`)
	dnats, allows := buildIngress(cfg.Networks["lan"])
	if len(dnats) != 1 || len(allows) != 1 {
		t.Fatalf("got %d dnats, %d allows; want 1 each", len(dnats), len(allows))
	}
	d := dnats[0]
	if d.HostPort != 8080 || d.ContPort != 80 || d.ContIP.String() != "10.0.0.13" || d.Proto != "tcp" {
		t.Errorf("dnat = %+v, want 8080->10.0.0.13:80 tcp", d)
	}
	// the matching forward allow is keyed on the CONTAINER port (post-DNAT).
	a := allows[0]
	if a.Port != 80 || a.IP.String() != "10.0.0.13" || a.Proto != "tcp" {
		t.Errorf("ingress allow = %+v, want 10.0.0.13:80 tcp", a)
	}
}

// --- hostsFile --------------------------------------------------------------

func TestHostsFile(t *testing.T) {
	// zwave attaches lan + iot, and has a flow to hass (lan). Its hosts file lists localhost,
	// its own addr on each network, and the flow peer -- but NOT proxy (no flow to it).
	cfg := mustParse(t, `{"containers":{"zwave":{},"hass":{},"proxy":{}},"networks":{
	  "lan":{"gateway":"10.0.0.1","gateway_if":"gw0","attach":{
	    "zwave":{"ip":"10.0.0.11","interface":"eth0"},
	    "hass":{"ip":"10.0.0.12","interface":"eth0"},
	    "proxy":{"ip":"10.0.0.13","interface":"eth0"}},
	    "flows":[{"from":"zwave","to":"hass","proto":"tcp","port":443}]},
	  "iot":{"gateway":"10.1.0.1","gateway_if":"gw1","attach":{
	    "zwave":{"ip":"10.1.0.11","interface":"eth1","default":true}}}}}`)
	got := hostsFile(cfg, "zwave")

	mustContain := []string{
		"127.0.0.1 localhost",
		"10.0.0.11 zwave", // own addr on lan
		"10.1.0.11 zwave", // own addr on iot
		"10.0.0.12 hass",  // the flow peer
	}
	for _, line := range mustContain {
		if !strings.Contains(got, line) {
			t.Errorf("hosts file missing %q:\n%s", line, got)
		}
	}
	if strings.Contains(got, "proxy") {
		t.Errorf("hosts file should not list proxy (no flow to it):\n%s", got)
	}

	// hass is only a flow TARGET, never a source -> it resolves itself but no peers.
	hassHosts := hostsFile(cfg, "hass")
	if strings.Contains(hassHosts, "zwave") {
		t.Errorf("hass hosts should not list zwave (flows are directional):\n%s", hassHosts)
	}
}

// --- helpers ----------------------------------------------------------------

func mustParse2LinkSpec(t *testing.T, cfg *config.Turnip) dataplane.LinkSpec {
	t.Helper()
	specs, err := buildLinkSpecs(cfg)
	if err != nil {
		t.Fatalf("buildLinkSpecs: %v", err)
	}
	if len(specs["box"]) != 1 {
		t.Fatalf("want exactly one link spec, got %d", len(specs["box"]))
	}
	return specs["box"][0]
}

func mustAddrT(t *testing.T, s string) netip.Addr {
	t.Helper()
	addr, err := netip.ParseAddr(s)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return addr
}

// TestRouterSysctls covers the router-netns knob set: the global pins, per-veth proxy_arp +
// strict rp_filter, and the uplink veth's strict rp_filter (only when an uplink exists).
func TestRouterSysctls(t *testing.T) {
	got := routerSysctls([]string{"vethR-a", "vethR-b"}, "vethR-up")

	want := map[string]string{
		"net.ipv4.ip_forward":                "1",
		"net.ipv4.conf.all.rp_filter":        "0", // per-veth values are authoritative
		"net.ipv6.conf.all.disable_ipv6":     "1",
		"net.ipv6.conf.default.disable_ipv6": "1",
		"net.ipv4.conf.vethR-a.proxy_arp":    "1",
		"net.ipv4.conf.vethR-a.rp_filter":    "1",
		"net.ipv4.conf.vethR-b.proxy_arp":    "1",
		"net.ipv4.conf.vethR-b.rp_filter":    "1",
		"net.ipv4.conf.vethR-up.rp_filter":   "1",
	}
	if len(got) != len(want) {
		t.Errorf("got %d keys, want %d:\n%v", len(got), len(want), got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
}

// TestRouterSysctlsNoUplink: with no uplink the uplink rp_filter key must be absent, and the
// global pins + per-veth keys are still present.
func TestRouterSysctlsNoUplink(t *testing.T) {
	got := routerSysctls([]string{"vethR-a"}, "")
	for k := range got {
		if k == "net.ipv4.conf..rp_filter" {
			t.Errorf("empty uplink produced a bare rp_filter key: %v", got)
		}
	}
	if got["net.ipv4.conf.vethR-a.proxy_arp"] != "1" {
		t.Errorf("per-veth proxy_arp missing: %v", got)
	}
	if _, ok := got["net.ipv4.ip_forward"]; !ok {
		t.Errorf("ip_forward missing: %v", got)
	}
}
