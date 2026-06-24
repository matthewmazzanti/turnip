// Package integration is the two-node hermetic harness for turnip's dataplane. It brings up a
// fixture with the real `turnip` binary, then asserts policy by probing INSIDE container netns
// (`turnip probe`, i.e. `podman unshare nsenter` -- no podman run) and against an external peer
// reached over SSH (the `world` node). Compiled with `go test -c` and run on the host node by the
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

// probe runs cmd inside a container's netns via `turnip probe`, returning the command's exit code.
func (h *H) probe(container string, cmd ...string) int {
	h.t.Helper()
	argv := append([]string{*turnipBin, "-c", h.fixture, "probe", container, "--"}, cmd...)
	out, code, err := h.host.Run(argv...)
	if err != nil {
		h.t.Fatalf("probe %s %v: %v\n%s", container, cmd, err, out)
	}
	return code
}

// connectPy is a stdlib TCP connect with a short timeout, run inside the source container's netns.
// Its exit code reports the POLICY verdict WITHOUT needing a listener on the destination:
//
//	0  reached the destination (connected, or refused == an RST came back) -> flow ALLOWED
//	3  timed out (the SYN was dropped, no reply)                           -> flow DENIED
//	4  other socket error (e.g. no route)
//
// The refused case is the crux: an allowed flow with no listener still gets an RST back through
// conntrack, so "reached" (0) cleanly separates allowed from the dropped-and-timed-out denied (3).
const connectPy = `
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

// reaches probes whether src can reach ip:port under policy (see connectPy for the verdict codes).
func (h *H) reaches(src, ip string, port int) int {
	return h.probe(src, "python3", "-c", connectPy, ip, strconv.Itoa(port))
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

// TestL1InternalFlow is fixture L1 (one network, zwave+hass, a single zwave->hass:8080 flow): the
// internal flow matrix -- the allowed flow connects, wrong port and reverse direction drop.
func TestL1InternalFlow(t *testing.T) {
	h := newH(t)
	h.up("l1.json")
	defer h.down()

	if code := h.reaches("zwave", "10.0.0.12", 8080); code != 0 {
		t.Errorf("FLOW-1 zwave->hass:8080: want reachable(0), got %d", code)
	}
	if code := h.reaches("zwave", "10.0.0.12", 9090); code != 3 {
		t.Errorf("FLOW-4 zwave->hass:9090 (wrong port): want denied/timeout(3), got %d", code)
	}
	if code := h.reaches("hass", "10.0.0.11", 8080); code != 3 {
		t.Errorf("FLOW-3 hass->zwave:8080 (reverse): want denied/timeout(3), got %d", code)
	}
}
