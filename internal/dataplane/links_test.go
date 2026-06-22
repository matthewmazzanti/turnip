package dataplane

import (
	"strings"
	"testing"

	"github.com/vishvananda/netlink"
)

// TestCheckParentFlavors: a parent device is a macvlan master XOR an ipvlan master. Same
// flavor on a shared parent is fine; mixing flavors on one parent is the conflict.
func TestCheckParentFlavors(t *testing.T) {
	cases := []struct {
		name    string
		specs   []LinkSpec
		wantErr string
	}{
		{
			name: "distinct parents, mixed flavors -- ok",
			specs: []LinkSpec{
				{Kind: "macvlan", Parent: "eth0"},
				{Kind: "ipvlan", Parent: "eth1"},
			},
		},
		{
			name: "same flavor shares a parent -- ok",
			specs: []LinkSpec{
				{Kind: "macvlan", Parent: "eth0"},
				{Kind: "macvlan", Parent: "eth0"},
			},
		},
		{
			name: "macvlan + ipvlan on one parent -- conflict",
			specs: []LinkSpec{
				{Kind: "macvlan", Parent: "eth0"},
				{Kind: "ipvlan", Parent: "eth0"},
			},
			wantErr: "cannot share a parent",
		},
		{
			name: "non-parent kinds are ignored",
			specs: []LinkSpec{
				{Kind: "veth-host"},
				{Kind: "phys", Dev: "eth9"},
				{Kind: "macvlan", Parent: "eth0"},
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := checkParentFlavors(c.specs)
			if c.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("error = %v, want containing %q", err, c.wantErr)
			}
		})
	}
}

func TestMacvlanMode(t *testing.T) {
	cases := map[string]netlink.MacvlanMode{
		"private":  netlink.MACVLAN_MODE_PRIVATE,
		"vepa":     netlink.MACVLAN_MODE_VEPA,
		"passthru": netlink.MACVLAN_MODE_PASSTHRU,
		"bridge":   netlink.MACVLAN_MODE_BRIDGE,
		"":         netlink.MACVLAN_MODE_BRIDGE, // default
		"garbage":  netlink.MACVLAN_MODE_BRIDGE, // unknown falls back to bridge
	}
	for in, want := range cases {
		if got := macvlanMode(in); got != want {
			t.Errorf("macvlanMode(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestIpvlanMode(t *testing.T) {
	cases := map[string]netlink.IPVlanMode{
		"l3":      netlink.IPVLAN_MODE_L3,
		"l3s":     netlink.IPVLAN_MODE_L3S,
		"l2":      netlink.IPVLAN_MODE_L2,
		"":        netlink.IPVLAN_MODE_L2, // default
		"garbage": netlink.IPVLAN_MODE_L2, // unknown falls back to l2
	}
	for in, want := range cases {
		if got := ipvlanMode(in); got != want {
			t.Errorf("ipvlanMode(%q) = %v, want %v", in, got, want)
		}
	}
}
