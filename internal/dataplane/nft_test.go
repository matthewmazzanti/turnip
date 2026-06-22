package dataplane

import (
	"bytes"
	"encoding/json"
	"flag"
	"net/netip"
	"os"
	"path/filepath"
	"testing"
)

// -update rewrites the golden files from the current output. Run `go test ./internal/dataplane
// -run NFT -update` after a deliberate ruleset change, then eyeball the diff.
var update = flag.Bool("update", false, "rewrite golden files")

// goldenRuleset renders rs to indented JSON and compares it against testdata/<name>.json
// (or rewrites it under -update). The indented form keeps the golden diffs readable.
func goldenRuleset(t *testing.T, name string, rs interface{ Render() ([]byte, error) }) {
	t.Helper()
	raw, err := rs.Render()
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, raw, "", "  "); err != nil {
		t.Fatalf("indent: %v", err)
	}
	pretty.WriteByte('\n')
	path := filepath.Join("testdata", name+".json")
	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, pretty.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden (run with -update to create): %v", err)
	}
	if !bytes.Equal(want, pretty.Bytes()) {
		t.Errorf("%s mismatch:\n--- got ---\n%s\n--- want ---\n%s", name, pretty.Bytes(), want)
	}
}

func mustAddr(t *testing.T, s string) netip.Addr {
	t.Helper()
	a, err := netip.ParseAddr(s)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return a
}

// TestBuildNFTGolden locks the full inet-turnip ruleset for a router WITH an uplink: the
// directional flow matrix, egress allows (an All container + a scoped one), the ingress
// DNAT allow, and the input lockdown.
func TestBuildNFTGolden(t *testing.T) {
	flows := []Flow{
		{FromIP: mustAddr(t, "10.0.0.11"), ToIP: mustAddr(t, "10.0.0.12"), Proto: "tcp", Port: 443},
		{FromIP: mustAddr(t, "10.0.0.12"), ToIP: mustAddr(t, "10.0.0.13"), Proto: "udp", Port: 53},
	}
	edge := &Edge{
		UplinkIf: "vethR-up",
		Egress: []EgressAllow{
			{IP: mustAddr(t, "10.0.0.11"), All: true},
			{IP: mustAddr(t, "10.0.0.12"), Rules: []EgressScope{
				{Protos: []string{"udp", "tcp"}, Port: 53},
				{Protos: []string{"icmp"}}, // portless
			}},
		},
		Ingress: []IngressAllow{
			{IP: mustAddr(t, "10.0.0.13"), Proto: "tcp", Port: 443},
		},
	}
	goldenRuleset(t, "build_nft_uplink", BuildNFT(flows, edge))
}

// TestBuildNFTNoUplinkGolden is the plain routed case (edge == nil): flow matrix + input
// lockdown only, no egress/ingress rules.
func TestBuildNFTNoUplinkGolden(t *testing.T) {
	flows := []Flow{
		{FromIP: mustAddr(t, "10.0.0.11"), ToIP: mustAddr(t, "10.0.0.12"), Proto: "tcp", Port: 443},
	}
	goldenRuleset(t, "build_nft_routed", BuildNFT(flows, nil))
}

// TestBuildHostNFTGolden locks the init-netns host zone: masquerade for uplink-forwarded
// egress + a prerouting DNAT for each published port (one with an explicit Listen, one any).
func TestBuildHostNFTGolden(t *testing.T) {
	up := Uplink{
		HostIf: "veth-lan-host", RouterIf: "vethR-up",
		HostIP: mustAddr(t, "169.254.1.0"), RouterIP: mustAddr(t, "169.254.1.1"),
	}
	dnats := []DNAT{
		{Listen: netip.IPv4Unspecified(), Proto: "tcp", HostPort: 8080, ContIP: mustAddr(t, "10.0.0.13"), ContPort: 80},
		{Listen: mustAddr(t, "192.0.2.1"), Proto: "tcp", HostPort: 8443, ContIP: mustAddr(t, "10.0.0.14"), ContPort: 443},
	}
	goldenRuleset(t, "build_host_nft", BuildHostNFT("lan", up, dnats))
}

// TestBuildHostNFTNoDNAT is the masquerade-only host zone (no published ports): the
// prerouting chain must be absent.
func TestBuildHostNFTNoDNAT(t *testing.T) {
	up := Uplink{
		HostIf: "veth-lan-host", RouterIf: "vethR-up",
		HostIP: mustAddr(t, "169.254.1.0"), RouterIP: mustAddr(t, "169.254.1.1"),
	}
	raw, err := BuildHostNFT("lan", up, nil).Render()
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte("prerouting")) {
		t.Errorf("host nft with no DNAT must omit the prerouting chain:\n%s", raw)
	}
	if !bytes.Contains(raw, []byte("masquerade")) {
		t.Errorf("host nft must always masquerade:\n%s", raw)
	}
}
