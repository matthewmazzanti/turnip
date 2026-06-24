// Package integration is the two-node hermetic harness for turnip's dataplane. It brings up a
// fixture with the real `turnip` binary, then asserts policy by probing INSIDE netns (`turnip
// probe`, i.e. `podman unshare nsenter` -- no podman run) and against an external peer reached
// over SSH (the `world` node). Compiled with `go test -c` and run on the host node by the
// nixosTest driver. See flake.nix `checks.integration` and docs/TEST-PLAN.md for the matrix.
//
// The host vantage is local (the binary runs ON the host node, where turnip + root live); world
// is the only SSH target (the dumb peer). Tests that need world skip when -world is unset, so a
// single-node run still exercises the host-only majority.
//
// The harness (H) holds no *testing.T: every method takes the current t. That keeps it safe to
// share one H across PARALLEL subtests (the fixture is up'd once, read-only during the probes).
package integration

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

var (
	turnipBin   = flag.String("turnip", "turnip", "path to the turnip binary")
	fixturesDir = flag.String("fixtures", ".", "directory holding the *.json fixtures")
	worldAddr   = flag.String("world", "", "ssh target for the world peer (user@host); empty => world tests skip")
	sshKey      = flag.String("ssh-key", "", "ssh identity file for the world target")
)

// stateDir is where turnip pins netns for the homelab owner (uid 1001) -- the fixtures all use
// runtime.user=homelab, so this is fixed.
const stateDir = "/run/user/1001/turnip"

// Connect-probe verdict codes (see connectTCP / connectUDP), plus an unreached sentinel for the
// want* helpers meaning "blocked by any mechanism" (got != reached).
const (
	reached   = 0  // connected, or RST-refused: the packet got through
	denied    = 3  // timed out: silently dropped (fail-closed)
	otherErr  = 4  // other socket error (e.g. an ICMP unreachable from a no-route)
	unreached = -1 // want* sentinel: not reached, mechanism unspecified
)

func verdict(code int) string {
	switch code {
	case reached:
		return "reached"
	case denied:
		return "denied(timeout)"
	case otherErr:
		return "other-error"
	case unreached:
		return "unreached"
	default:
		return fmt.Sprintf("code(%d)", code)
	}
}

// Target is an execution endpoint: the host (local) or the world peer (ssh). Run executes argv
// (argv[0] is the program) and returns combined output + the process exit code. err is set only
// for a launch failure, NOT a non-zero exit -- a non-zero code is a RESULT (e.g. a dropped flow).
type Target interface {
	Run(argv ...string) (out string, code int, err error)
}

type local struct{}

func (local) Run(argv ...string) (string, int, error) {
	cmd := exec.Command(argv[0], argv[1:]...)
	var buf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &buf, &buf
	err := cmd.Run()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return buf.String(), ee.ExitCode(), nil
	}
	return buf.String(), 0, err
}

// ssh wraps a remote command in `ssh -i <key> ... <addr> <argv>`; the remote shell re-splits the
// argv, so it's for simple peer commands (no shell metacharacters).
type ssh struct{ addr, key string }

func (s ssh) Run(argv ...string) (string, int, error) {
	full := append([]string{
		"ssh", "-i", s.key,
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "ConnectTimeout=5",
		s.addr,
	}, argv...)
	return local{}.Run(full...)
}

// H is the harness: the host (local turnip), the optional world peer, and the up'd fixture path.
// It carries no *testing.T -- methods take t -- so one H is safe across parallel subtests.
type H struct {
	host    Target
	world   Target // nil when -world is unset
	fixture string // path of the currently-up fixture (set by up; read-only during probes)
}

func newH() *H {
	h := &H{host: local{}}
	if *worldAddr != "" {
		h.world = ssh{addr: *worldAddr, key: *sshKey}
	}
	return h
}

// up brings up a fixture by name (a file under -fixtures) with the real turnip binary.
func (h *H) up(t *testing.T, fixture string) {
	t.Helper()
	h.fixture = filepath.Join(*fixturesDir, fixture)
	out, code, err := h.host.Run(*turnipBin, "-c", h.fixture, "up")
	if err != nil || code != 0 {
		t.Fatalf("turnip up %s: code=%d err=%v\n%s", fixture, code, err, out)
	}
}

// down tears the fixture down -- the isolation reset between fixtures. Best-effort (logs, never
// fails the test): a failed teardown shouldn't mask the assertions that already ran.
func (h *H) down(t *testing.T) {
	t.Helper()
	out, code, err := h.host.Run(*turnipBin, "-c", h.fixture, "down")
	if err != nil || code != 0 {
		t.Logf("turnip down: code=%d err=%v\n%s", code, err, out)
	}
}

// probe runs cmd inside a netns (target = a container name or "router:<net>") via `turnip probe`,
// returning combined output + the command's exit code. A launch failure is fatal.
func (h *H) probe(t *testing.T, target string, cmd ...string) (string, int) {
	t.Helper()
	argv := append([]string{*turnipBin, "-c", h.fixture, "probe", target, "--"}, cmd...)
	out, code, err := h.host.Run(argv...)
	if err != nil {
		t.Fatalf("probe %s %v: %v\n%s", target, cmd, err, out)
	}
	return out, code
}

// --- traffic primitives ---------------------------------------------------

// connectTCP is a stdlib TCP connect with a short timeout, run inside the source netns. Its exit
// code reports the POLICY verdict WITHOUT a listener on the destination: an allowed flow with no
// server still gets an RST back through conntrack (== "refused" == reached), which cleanly
// separates allowed (0) from the dropped-and-timed-out denied (3). This also proves the return
// path (FLOW-2): the RST/SYN-ACK only arrives if ct established/related lets the reply back.
const connectTCP = `
import socket, sys
ip, port = sys.argv[1], int(sys.argv[2])
s = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
s.settimeout(2)
try:
    s.connect((ip, port)); sys.exit(0)
except ConnectionRefusedError:
    sys.exit(0)
except (TimeoutError, socket.timeout):
    sys.exit(3)
except OSError:
    sys.exit(4)
`

// connectUDP is the UDP analogue: send a datagram, then recv. A reached-but-closed port answers
// with an ICMP port-unreachable (-> ConnectionRefused == reached); a dropped packet yields no
// reply (-> timeout == denied). Same verdict codes as connectTCP.
const connectUDP = `
import socket, sys
ip, port = sys.argv[1], int(sys.argv[2])
s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
s.settimeout(2)
try:
    s.connect((ip, port)); s.send(b"x"); s.recv(64); sys.exit(0)
except ConnectionRefusedError:
    sys.exit(0)
except (TimeoutError, socket.timeout):
    sys.exit(3)
except OSError:
    sys.exit(4)
`

func (h *H) reachesTCP(t *testing.T, src, ip string, port int) int {
	_, code := h.probe(t, src, "python3", "-c", connectTCP, ip, strconv.Itoa(port))
	return code
}

func (h *H) reachesUDP(t *testing.T, src, ip string, port int) int {
	_, code := h.probe(t, src, "python3", "-c", connectUDP, ip, strconv.Itoa(port))
	return code
}

func (h *H) pings(t *testing.T, src, ip string) int {
	_, code := h.probe(t, src, "ping", "-c1", "-W2", ip)
	return code
}

// --- assertion helpers ----------------------------------------------------

// wantTCP / wantUDP run a connect probe from src to ip:port and assert the verdict. want is a
// verdict code (reached / denied / otherErr) for an exact match, or unreached for "blocked, by
// any mechanism" (used where a no-route may legitimately drop OR ICMP-unreachable).
func (h *H) wantTCP(t *testing.T, label, src, ip string, port, want int) {
	t.Helper()
	wantVerdict(t, label, fmt.Sprintf("%s->%s:%d tcp", src, ip, port), h.reachesTCP(t, src, ip, port), want)
}

func (h *H) wantUDP(t *testing.T, label, src, ip string, port, want int) {
	t.Helper()
	wantVerdict(t, label, fmt.Sprintf("%s->%s:%d udp", src, ip, port), h.reachesUDP(t, src, ip, port), want)
}

// wantPing asserts src can (reachable) or cannot (!reachable) ICMP-ping ip.
func (h *H) wantPing(t *testing.T, label, src, ip string, reachable bool) {
	t.Helper()
	code := h.pings(t, src, ip)
	if reachable && code != 0 {
		t.Errorf("%s: ping %s->%s got no reply (code %d), want reachable", label, src, ip, code)
	}
	if !reachable && code == 0 {
		t.Errorf("%s: ping %s->%s got a reply, want dropped", label, src, ip)
	}
}

// wantVerdict is the exact/unreached comparison shared by wantTCP/wantUDP.
func wantVerdict(t *testing.T, label, edge string, got, want int) {
	t.Helper()
	if want == unreached {
		if got == reached {
			t.Errorf("%s (%s): %s, want unreached", label, edge, verdict(got))
		}
		return
	}
	if got != want {
		t.Errorf("%s (%s): %s, want %s", label, edge, verdict(got), verdict(want))
	}
}

// has asserts out contains want (an inspection assertion for the structural checks).
func has(t *testing.T, out, want, label string) {
	t.Helper()
	if !strings.Contains(out, want) {
		t.Errorf("%s: missing %q in:\n%s", label, want, out)
	}
}

// --- the scenarios --------------------------------------------------------

// TestWorldReachable proves the host->world SSH channel that the egress/ingress scenarios ride.
func TestWorldReachable(t *testing.T) {
	h := newH()
	if h.world == nil {
		t.Skip("no -world configured")
	}
	out, code, err := h.world.Run("echo", "turnip-world-ok")
	if err != nil || code != 0 || !strings.Contains(out, "turnip-world-ok") {
		t.Fatalf("world echo: code=%d err=%v out=%q", code, err, out)
	}
}

// TestL1Structure asserts fixture L1 materialized into kernel state (§2 NET-1..5): the netns are
// pinned, the gateway + routed veths exist, the nft flow matrix is loaded fail-closed, and the
// router's anti-spoof sysctls are set. Observed via host stat + router/container netns probes.
func TestL1Structure(t *testing.T) {
	h := newH()
	h.up(t, "l1.json")
	defer h.down(t)

	// NET-1: the netns are pinned at their state-dir paths.
	for _, p := range []string{
		stateDir + "/routers/lan",
		stateDir + "/containers/zwave/netns",
		stateDir + "/containers/hass/netns",
	} {
		if _, code, _ := h.host.Run("test", "-e", p); code != 0 {
			t.Errorf("NET-1: netns pin %s not present", p)
		}
	}

	// NET-2: the gateway dummy carries the gateway address in the router netns.
	gw, _ := h.probe(t, "router:lan", "ip", "addr", "show", "gw0")
	has(t, gw, "10.0.0.1", "NET-2 gateway gw0")

	// NET-3: the container side -- eth0 holds the /32 and the default route points at the gateway.
	addr, _ := h.probe(t, "zwave", "ip", "addr", "show", "eth0")
	has(t, addr, "10.0.0.11/32", "NET-3 zwave eth0")
	rt, _ := h.probe(t, "zwave", "ip", "route")
	has(t, rt, "default via 10.0.0.1", "NET-3 zwave default route")

	// NET-4: the nft flow matrix is loaded, forward + input both fail-closed (policy drop).
	nft, _ := h.probe(t, "router:lan", "nft", "list", "ruleset")
	has(t, nft, "chain forward", "NET-4 forward chain")
	has(t, nft, "chain input", "NET-4 input chain")
	if n := strings.Count(nft, "policy drop"); n < 2 {
		t.Errorf("NET-4: want >=2 'policy drop' (forward+input), got %d in:\n%s", n, nft)
	}

	// NET-5: the router's anti-spoof sysctls -- forwarding on, strict per-veth rp_filter, ipv6 off.
	for _, c := range []struct{ path, want, label string }{
		{"/proc/sys/net/ipv4/ip_forward", "1", "NET-5 ip_forward"},
		{"/proc/sys/net/ipv4/conf/vethR-zwave/rp_filter", "1", "NET-5 rp_filter strict"},
		{"/proc/sys/net/ipv6/conf/all/disable_ipv6", "1", "NET-5 ipv6 disabled"},
	} {
		out, _ := h.probe(t, "router:lan", "cat", c.path)
		if strings.TrimSpace(out) != c.want {
			t.Errorf("%s: %s = %q, want %q", c.label, c.path, strings.TrimSpace(out), c.want)
		}
	}
}

// TestL1InternalFlow is fixture L1's internal flow matrix (§3): one network, zwave+hass, a single
// zwave->hass:8080 flow. The allowed flow connects (return path rides conntrack); everything else
// drops. The rows run as PARALLEL subtests (each is timeout-bound and read-only), wrapped in a
// "flows" group so the deferred down() waits for them.
func TestL1InternalFlow(t *testing.T) {
	h := newH()
	h.up(t, "l1.json")
	t.Cleanup(func() { h.down(t) }) // runs after the parallel subtests complete -- no group wrapper

	flows := []struct {
		name string
		run  func(t *testing.T)
	}{
		// FLOW-1 (+FLOW-2 return path): the one allowed flow reaches hass.
		{"FLOW-1_allowed", func(t *testing.T) { h.wantTCP(t, "FLOW-1", "zwave", "10.0.0.12", 8080, reached) }},
		// FLOW-3 (+FLOW-8 fail-closed): reverse direction has no flow; hass reaches nothing.
		{"FLOW-3_reverse", func(t *testing.T) { h.wantTCP(t, "FLOW-3", "hass", "10.0.0.11", 8080, denied) }},
		{"FLOW-8_zeroflow", func(t *testing.T) { h.wantTCP(t, "FLOW-8", "hass", "10.0.0.11", 9090, denied) }},
		// FLOW-4: wrong port is not in the vmap -> dropped.
		{"FLOW-4_wrongport", func(t *testing.T) { h.wantTCP(t, "FLOW-4", "zwave", "10.0.0.12", 9090, denied) }},
		// FLOW-5: wrong proto (udp on the tcp flow's port) -> dropped.
		{"FLOW-5_wrongproto", func(t *testing.T) { h.wantUDP(t, "FLOW-5", "zwave", "10.0.0.12", 8080, denied) }},
		// FLOW-6: no icmp flow -> ping dropped.
		{"FLOW-6_icmp", func(t *testing.T) { h.wantPing(t, "FLOW-6", "zwave", "10.0.0.12", false) }},
		// FLOW-7: an address with no attachment -> no route / dropped (drop OR icmp-unreachable).
		{"FLOW-7_nopeer", func(t *testing.T) { h.wantTCP(t, "FLOW-7", "zwave", "10.0.0.99", 8080, unreached) }},
	}

	for _, f := range flows {
		t.Run(f.name, func(t *testing.T) {
			t.Parallel()
			f.run(t)
		})
	}
}

// TestL2Isolation is fixture L2 (§2 NET-6): two networks, lan {alpha,bravo} and iot
// {charlie,delta}, each with its own internal flow. Each network's intra-flow works, but the two
// are isolated -- a container in one cannot reach an address in the other, since the router netns
// are siloed (no route, no forwarding path between them).
func TestL2Isolation(t *testing.T) {
	h := newH()
	h.up(t, "l2.json")
	t.Cleanup(func() { h.down(t) })

	// NET-6 structure: both router netns are pinned (one per network).
	for _, p := range []string{stateDir + "/routers/lan", stateDir + "/routers/iot"} {
		if _, code, _ := h.host.Run("test", "-e", p); code != 0 {
			t.Errorf("NET-6: router netns %s not present", p)
		}
	}

	checks := []struct {
		name string
		run  func(t *testing.T)
	}{
		// each network's own flow still works.
		{"intra_lan_allowed", func(t *testing.T) { h.wantTCP(t, "L2-intra", "alpha", "10.0.0.12", 8080, reached) }},
		{"intra_iot_allowed", func(t *testing.T) { h.wantTCP(t, "L2-intra", "charlie", "10.1.0.12", 8080, reached) }},
		// cross-network is isolated, both directions and to either peer.
		{"lan_to_iot_gw_isolated", func(t *testing.T) { h.wantTCP(t, "NET-6", "alpha", "10.1.0.11", 8080, unreached) }},
		{"lan_to_iot_peer_isolated", func(t *testing.T) { h.wantTCP(t, "NET-6", "alpha", "10.1.0.12", 8080, unreached) }},
		{"iot_to_lan_isolated", func(t *testing.T) { h.wantTCP(t, "NET-6", "charlie", "10.0.0.11", 8080, unreached) }},
	}
	for _, c := range checks {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			c.run(t)
		})
	}
}
