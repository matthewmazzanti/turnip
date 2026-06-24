// Package integration is the two-node hermetic harness for turnip's dataplane. It brings up a
// fixture with the real `turnip` binary, then asserts policy by probing INSIDE netns (`turnip
// probe`, i.e. `podman unshare nsenter` -- no podman run) and against an external peer reached
// over SSH (the `world` node). Compiled with `go test -c` and run on the host node by the
// nixosTest driver. See flake.nix `checks.integration` and docs/TEST-PLAN.md for the matrix.
//
// The host vantage is local (the binary runs ON the host node, where turnip + root live); world
// is the only SSH target (the dumb peer). Tests that need world skip when -world is unset, so a
// single-node run still exercises the host-only majority.
package integration

import (
	"bytes"
	"errors"
	"flag"
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

// H is one test's harness: the host (local turnip) and the optional world peer.
type H struct {
	t       *testing.T
	host    Target
	world   Target // nil when -world is unset
	fixture string // path of the currently-up fixture (set by up)
}

func newH(t *testing.T) *H {
	h := &H{t: t, host: local{}}
	if *worldAddr != "" {
		h.world = ssh{addr: *worldAddr, key: *sshKey}
	}
	return h
}

// up brings up a fixture by name (a file under -fixtures) with the real turnip binary.
func (h *H) up(fixture string) {
	h.t.Helper()
	h.fixture = filepath.Join(*fixturesDir, fixture)
	out, code, err := h.host.Run(*turnipBin, "-c", h.fixture, "up")
	if err != nil || code != 0 {
		h.t.Fatalf("turnip up %s: code=%d err=%v\n%s", fixture, code, err, out)
	}
}

// down tears the fixture down -- the isolation reset between fixtures. Best-effort (logs, never
// fails the test): a failed teardown shouldn't mask the assertions that already ran.
func (h *H) down() {
	h.t.Helper()
	out, code, err := h.host.Run(*turnipBin, "-c", h.fixture, "down")
	if err != nil || code != 0 {
		h.t.Logf("turnip down: code=%d err=%v\n%s", code, err, out)
	}
}

// probe runs cmd inside a netns (target = a container name or "router:<net>") via `turnip probe`,
// returning combined output + the command's exit code. A launch failure is fatal.
func (h *H) probe(target string, cmd ...string) (string, int) {
	h.t.Helper()
	argv := append([]string{*turnipBin, "-c", h.fixture, "probe", target, "--"}, cmd...)
	out, code, err := h.host.Run(argv...)
	if err != nil {
		h.t.Fatalf("probe %s %v: %v\n%s", target, cmd, err, out)
	}
	return out, code
}

// --- traffic primitives ---------------------------------------------------

// connect verdict codes (see connectTCP / connectUDP): 0 reached (allowed), 3 timed out (denied),
// 4 other socket error (e.g. no route).
const (
	reached  = 0
	denied   = 3
	otherErr = 4
)

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

// reachesTCP / reachesUDP probe whether src can reach ip:port under policy (verdict codes above).
func (h *H) reachesTCP(src, ip string, port int) int {
	_, code := h.probe(src, "python3", "-c", connectTCP, ip, strconv.Itoa(port))
	return code
}

func (h *H) reachesUDP(src, ip string, port int) int {
	_, code := h.probe(src, "python3", "-c", connectUDP, ip, strconv.Itoa(port))
	return code
}

// pings reports whether src can ICMP-ping ip (exit 0 = reply; non-zero = no reply / dropped).
func (h *H) pings(src, ip string) int {
	_, code := h.probe(src, "ping", "-c1", "-W2", ip)
	return code
}

// has asserts out contains want (an inspection assertion for the structural checks).
func (h *H) has(out, want, label string) {
	h.t.Helper()
	if !strings.Contains(out, want) {
		h.t.Errorf("%s: missing %q in:\n%s", label, want, out)
	}
}

// --- the scenarios --------------------------------------------------------

// TestWorldReachable proves the host->world SSH channel that the egress/ingress scenarios ride.
func TestWorldReachable(t *testing.T) {
	h := newH(t)
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
	h := newH(t)
	h.up("l1.json")
	defer h.down()

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
	gw, _ := h.probe("router:lan", "ip", "addr", "show", "gw0")
	h.has(gw, "10.0.0.1", "NET-2 gateway gw0")

	// NET-3: the container side -- eth0 holds the /32 and the default route points at the gateway.
	addr, _ := h.probe("zwave", "ip", "addr", "show", "eth0")
	h.has(addr, "10.0.0.11/32", "NET-3 zwave eth0")
	rt, _ := h.probe("zwave", "ip", "route")
	h.has(rt, "default via 10.0.0.1", "NET-3 zwave default route")

	// NET-4: the nft flow matrix is loaded, forward + input both fail-closed (policy drop).
	nft, _ := h.probe("router:lan", "nft", "list", "ruleset")
	h.has(nft, "chain forward", "NET-4 forward chain")
	h.has(nft, "chain input", "NET-4 input chain")
	if n := strings.Count(nft, "policy drop"); n < 2 {
		t.Errorf("NET-4: want >=2 'policy drop' (forward+input), got %d in:\n%s", n, nft)
	}

	// NET-5: the router's anti-spoof sysctls -- forwarding on, strict per-veth rp_filter, ipv6 off.
	for _, c := range []struct{ path, want, label string }{
		{"/proc/sys/net/ipv4/ip_forward", "1", "NET-5 ip_forward"},
		{"/proc/sys/net/ipv4/conf/vethR-zwave/rp_filter", "1", "NET-5 rp_filter strict"},
		{"/proc/sys/net/ipv6/conf/all/disable_ipv6", "1", "NET-5 ipv6 disabled"},
	} {
		out, _ := h.probe("router:lan", "cat", c.path)
		if strings.TrimSpace(out) != c.want {
			t.Errorf("%s: %s = %q, want %q", c.label, c.path, strings.TrimSpace(out), c.want)
		}
	}
}

// TestL1InternalFlow is fixture L1's internal flow matrix (§3): one network, zwave+hass, a single
// zwave->hass:8080 flow. The allowed flow connects (and its return path rides conntrack);
// everything else -- wrong port, wrong proto, icmp, no-such-peer, reverse direction -- is dropped.
func TestL1InternalFlow(t *testing.T) {
	h := newH(t)
	h.up("l1.json")
	defer h.down()

	// FLOW-1 (+FLOW-2 return path): the one allowed flow reaches hass.
	if code := h.reachesTCP("zwave", "10.0.0.12", 8080); code != reached {
		t.Errorf("FLOW-1 zwave->hass:8080: want reached(%d), got %d", reached, code)
	}
	// FLOW-4: wrong port is not in the vmap -> dropped.
	if code := h.reachesTCP("zwave", "10.0.0.12", 9090); code != denied {
		t.Errorf("FLOW-4 zwave->hass:9090 (wrong port): want denied(%d), got %d", denied, code)
	}
	// FLOW-5: wrong proto (udp on the tcp flow's port) -> dropped.
	if code := h.reachesUDP("zwave", "10.0.0.12", 8080); code != denied {
		t.Errorf("FLOW-5 zwave->hass udp/8080 (wrong proto): want denied(%d), got %d", denied, code)
	}
	// FLOW-6: no icmp flow -> ping dropped.
	if code := h.pings("zwave", "10.0.0.12"); code == 0 {
		t.Errorf("FLOW-6 zwave->hass icmp: want dropped (non-zero ping), got 0")
	}
	// FLOW-7: an address with no attachment -> no route / dropped.
	if code := h.reachesTCP("zwave", "10.0.0.99", 8080); code == reached {
		t.Errorf("FLOW-7 zwave->10.0.0.99 (no peer): want unreachable, got reached")
	}
	// FLOW-3 (and FLOW-8 fail-closed): the reverse direction has no flow -> dropped; hass, with no
	// outgoing flow, reaches nothing.
	if code := h.reachesTCP("hass", "10.0.0.11", 8080); code != denied {
		t.Errorf("FLOW-3 hass->zwave:8080 (reverse): want denied(%d), got %d", denied, code)
	}
	if code := h.reachesTCP("hass", "10.0.0.11", 9090); code != denied {
		t.Errorf("FLOW-8 hass->zwave:9090 (zero-flow container): want denied(%d), got %d", denied, code)
	}
}
