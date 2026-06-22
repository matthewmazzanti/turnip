package dataplane

import "testing"

// TestRouterSysctls covers the router-netns knob set: the global pins, per-veth proxy_arp +
// strict rp_filter, and the uplink veth's strict rp_filter (only when an uplink exists).
func TestRouterSysctls(t *testing.T) {
	got := RouterSysctls([]string{"vethR-a", "vethR-b"}, "vethR-up")

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
	got := RouterSysctls([]string{"vethR-a"}, "")
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
