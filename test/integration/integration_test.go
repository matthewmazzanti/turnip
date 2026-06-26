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
	"embed"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

var (
	turnipBin = flag.String("turnip", "turnip", "path to the turnip binary")
	worldAddr = flag.String("world", "", "ssh target for the world peer (user@host); empty => world tests skip")
	sshKey    = flag.String("ssh-key", "", "ssh identity file for the world target")
	imageRef  = flag.String("image", "", "ref/tag of the pre-loaded probe image for the real `podman run` test; empty => TestPodmanRun skips")
)

// The fixture configs are embedded into the test binary (`go test -c`), so the harness is
// self-contained -- no -fixtures dir to route from a nix store path (the check) or a 9p mount (the
// dev VM). up() materializes the named fixture to a temp file for `turnip -c` (they're co-located:
// the binary runs ON the host node where turnip runs). Editing a fixture + recompiling picks it up.
//
//go:embed fixtures/*.json
var fixturesFS embed.FS

// stateDir is where turnip pins netns for the homelab owner (uid 1001) -- the fixtures all use
// runtime.user=homelab, so this is fixed. The owner* constants are the same rootless owner from
// the host node's perspective, used to drive its podman for the real `podman run` test.
const (
	stateDir        = "/run/user/1001/turnip"
	ownerUser       = "homelab"
	ownerHome       = "/home/homelab"
	ownerRuntimeDir = "/run/user/1001"
)

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

// up brings up a fixture by name (an embedded *.json) with the real turnip binary. The fixture is
// written to a temp file turnip reads via -c; h.fixture holds that path for down()/probe() to reuse.
func (h *H) up(t *testing.T, fixture string) {
	t.Helper()
	data, err := fixturesFS.ReadFile("fixtures/" + fixture)
	if err != nil {
		t.Fatalf("read embedded fixture %s: %v", fixture, err)
	}
	h.fixture = filepath.Join(t.TempDir(), fixture)
	if err := os.WriteFile(h.fixture, data, 0o644); err != nil {
		t.Fatalf("write fixture %s: %v", fixture, err)
	}
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

// --- the operator path: real podman run -----------------------------------

// ownerPodman runs a podman command as the rootless owner (homelab) from the host node, the way
// an operator would: `runuser -u homelab -- env HOME=.. XDG_RUNTIME_DIR=.. podman <args>`. runuser
// execs the argv DIRECTLY (no shell re-split), so a python `-c` script rides through unquoted;
// HOME is set explicitly because podman's graphroot lives under it and runuser keeps root's env.
func (h *H) ownerPodman(args ...string) (string, int, error) {
	argv := append([]string{
		"runuser", "-u", ownerUser, "--",
		"env", "HOME=" + ownerHome, "XDG_RUNTIME_DIR=" + ownerRuntimeDir,
		"podman",
	}, args...)
	return h.host.Run(argv...)
}

// runAttached does the full documented operator attach as the owner: `podman run --rm --network
// ns:<nsPath> -v <hostsPath>:/etc/hosts:ro <ref> <cmd...>`. Beyond joining the turnip-pinned netns
// it bind-mounts the container's GENERATED /etc/hosts (the "host bind" -- turnip writes it chowned
// to the owner so podman mounts it cleanly), so the container resolves itself and its flow peers by
// name. Returns combined output + the container's exit code (== the entrypoint's, so a connect
// probe's verdict propagates through).
func (h *H) runAttached(t *testing.T, ref, nsPath, hostsPath string, cmd ...string) (string, int) {
	t.Helper()
	args := append([]string{
		"run", "--rm",
		"--network", "ns:" + nsPath,
		"-v", hostsPath + ":/etc/hosts:ro", // the generated hosts file (CONFIG-SKETCH / README)
		ref,
	}, cmd...)
	out, code, err := h.ownerPodman(args...)
	if err != nil {
		t.Fatalf("podman run %v: %v\n%s", cmd, err, out)
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

// counterIn runs a bare-int-printing counter script (rpfCounter / ctInvalidCounter) in a router
// netns and parses the result. The delta across a crafted burst is the count the kernel dropped.
func (h *H) counterIn(t *testing.T, router, script, what string) int {
	t.Helper()
	out, code := h.probe(t, router, "python3", "-c", script)
	if code != 0 {
		t.Fatalf("read %s counter in %s: code=%d\n%s", what, router, code, out)
	}
	n, err := strconv.Atoi(strings.TrimSpace(out))
	if err != nil {
		t.Fatalf("parse %s counter %q: %v", what, out, err)
	}
	return n
}

// rpfDrops reads the strict-rp_filter drop counter (IPReversePathFilter); ctInvalid reads the
// conntrack out-of-state (invalid) counter. Both are netns-scoped -- read them in a router netns.
func (h *H) rpfDrops(t *testing.T, router string) int {
	return h.counterIn(t, router, rpfCounter, "rp_filter")
}

func (h *H) ctInvalid(t *testing.T, router string) int {
	return h.counterIn(t, router, ctInvalidCounter, "ct-invalid")
}

// recvPeer connects to ip:port and prints the source address the SERVER reports seeing. Run
// against world's peer-echo, it reads back the (masqueraded) source of an egress connection.
const recvPeer = `
import socket, sys
ip, port = sys.argv[1], int(sys.argv[2])
s = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
s.settimeout(4)
s.connect((ip, port))
sys.stdout.write(s.recv(64).decode())
`

// acceptPeer binds 0.0.0.0:<port> in the target netns, accepts ONE connection, prints the peer's
// source address, and exits. Run in svc's netns it reveals the source an INGRESS connection
// presents -- which must be world's real IP, since ingress is DNAT-only (never masqueraded).
const acceptPeer = `
import socket, sys
port = int(sys.argv[1])
s = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
s.bind(("0.0.0.0", port)); s.listen(1); s.settimeout(10)
try:
    c, addr = s.accept()
except (TimeoutError, socket.timeout):
    sys.exit(2)
sys.stdout.write(addr[0]); c.close()
`

// spoofSend crafts n IP/ICMP echo packets with a chosen (often unowned) source address and emits
// them on a raw IPPROTO_RAW socket (IP_HDRINCL) inside the source netns. argv: src dst n. The
// netns holds CAP_NET_RAW via the probe (podman unshare), so the build+send succeeds locally; the
// router's strict rp_filter is what drops a forged saddr (BAD-1). stdlib-only -- no scapy.
const spoofSend = `
import socket, struct, sys
def cks(b):
    if len(b) % 2: b += b"\x00"
    s = sum(struct.unpack("!%dH" % (len(b)//2), b)); s = (s >> 16) + (s & 0xffff); s += s >> 16
    return (~s) & 0xffff
src, dst, n = sys.argv[1], sys.argv[2], int(sys.argv[3])
s = socket.socket(socket.AF_INET, socket.SOCK_RAW, socket.IPPROTO_RAW)
s.setsockopt(socket.IPPROTO_IP, socket.IP_HDRINCL, 1)
pay = b"turnip-bad"
icmp = struct.pack("!BBHHH", 8, 0, 0, 0x1234, 1) + pay
icmp = struct.pack("!BBHHH", 8, 0, cks(icmp), 0x1234, 1) + pay
tot = 20 + len(icmp)
for _ in range(n):
    h = struct.pack("!BBHHHBBH4s4s", 0x45, 0, tot, 0, 0, 64, 1, 0,
                    socket.inet_aton(src), socket.inet_aton(dst))
    h = struct.pack("!BBHHHBBH4s4s", 0x45, 0, tot, 0, 0, 64, 1, cks(h),
                    socket.inet_aton(src), socket.inet_aton(dst))
    s.sendto(h + icmp, (dst, 0))
`

// ackSend crafts n bare TCP ACK segments (no SYN, no connection) from src to dst:dport on a raw
// IPPROTO_RAW socket inside the source netns. With conntrack tcp_loose off on the router, such an
// out-of-state packet is `ct invalid` -> dropped by the forward chain, even toward an ALLOWED
// (proto,dport). argv: src dst dport n. stdlib-only.
const ackSend = `
import socket, struct, sys
def cks(b):
    if len(b) % 2: b += b"\x00"
    s = sum(struct.unpack("!%dH" % (len(b)//2), b)); s = (s >> 16) + (s & 0xffff); s += s >> 16
    return (~s) & 0xffff
src, dst, dport, n = sys.argv[1], sys.argv[2], int(sys.argv[3]), int(sys.argv[4])
s = socket.socket(socket.AF_INET, socket.SOCK_RAW, socket.IPPROTO_RAW)
s.setsockopt(socket.IPPROTO_IP, socket.IP_HDRINCL, 1)
sport, seq, ackn = 40000, 12345, 67890
def seg():
    off, flags, win = 5 << 4, 0x10, 8192  # ACK only, no SYN
    h = struct.pack("!HHIIBBHHH", sport, dport, seq, ackn, off, flags, win, 0, 0)
    pseudo = socket.inet_aton(src) + socket.inet_aton(dst) + struct.pack("!BBH", 0, 6, len(h))
    return struct.pack("!HHIIBBHHH", sport, dport, seq, ackn, off, flags, win, cks(pseudo + h), 0)
for _ in range(n):
    seg_b = seg(); tot = 20 + len(seg_b)
    h = struct.pack("!BBHHHBBH4s4s", 0x45, 0, tot, 0, 0, 64, 6, 0,
                    socket.inet_aton(src), socket.inet_aton(dst))
    h = struct.pack("!BBHHHBBH4s4s", 0x45, 0, tot, 0, 0, 64, 6, cks(h),
                    socket.inet_aton(src), socket.inet_aton(dst))
    s.sendto(h + seg_b, (dst, 0))
`

// arpPoison blasts n gratuitous ARP replies out iface claiming claim_ip is at THIS netns's MAC --
// the lateral ARP-spoof a container would attempt to hijack a peer's address. AF_PACKET raw frame
// (CAP_NET_RAW via the probe). argv: iface claim_ip n. Prints the source MAC it used (hex).
const arpPoison = `
import socket, struct, sys
iface, claim_ip, n = sys.argv[1], sys.argv[2], int(sys.argv[3])
with open("/sys/class/net/%s/address" % iface) as f:
    mac = bytes.fromhex(f.read().strip().replace(":", ""))
s = socket.socket(socket.AF_PACKET, socket.SOCK_RAW, socket.htons(0x0806))
s.bind((iface, 0))
bcast = b"\xff" * 6
ip = socket.inet_aton(claim_ip)
eth = bcast + mac + struct.pack("!H", 0x0806)
arp = struct.pack("!HHBBH", 1, 0x0800, 6, 4, 2) + mac + ip + bcast + ip  # oper=2 reply, gratuitous
for _ in range(n):
    s.send(eth + arp)
sys.stdout.write(mac.hex())
`

// rpfCounter prints the netns-scoped IPReversePathFilter counter (strict rp_filter drops) from
// /proc/net/netstat as a bare integer. Read in the router netns it counts forged-saddr drops.
const rpfCounter = `
import sys
want = "IPReversePathFilter"
val = 0
with open("/proc/net/netstat") as f:
    lines = f.read().splitlines()
for i in range(0, len(lines) - 1, 2):
    keys, vals = lines[i].split(), lines[i + 1].split()
    if want in keys:
        val = int(vals[keys.index(want)]); break
sys.stdout.write(str(val))
`

// ctInvalidCounter prints the netns-scoped conntrack "invalid" total (summed across the per-CPU
// rows of /proc/net/stat/nf_conntrack, whose first line labels the columns) as a bare integer.
const ctInvalidCounter = `
import sys
L = open("/proc/net/stat/nf_conntrack").read().splitlines()
i = L[0].split().index("invalid")
sys.stdout.write(str(sum(int(r.split()[i], 16) for r in L[1:])))
`

// worldIPv4 resolves the world node's test-LAN IPv4 (via the host's /etc/hosts), so a container
// can dial it numerically -- container netns have no name resolution for the peer.
func (h *H) worldIPv4(t *testing.T) string {
	t.Helper()
	out, code, err := h.host.Run("getent", "ahostsv4", "world")
	if err != nil || code != 0 {
		t.Fatalf("resolve world ipv4: code=%d err=%v\n%s", code, err, out)
	}
	f := strings.Fields(out)
	if len(f) == 0 {
		t.Fatalf("no ipv4 for world in: %q", out)
	}
	return f[0]
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
		// conf.default is the TEMPLATE set before veth creation; the per-veth values below are
		// INHERITED from it (no per-veth pinning), which is what makes new veths born-hardened.
		{"/proc/sys/net/ipv4/conf/default/rp_filter", "1", "NET-5 conf.default rp_filter (template)"},
		{"/proc/sys/net/ipv4/conf/default/proxy_arp", "1", "NET-5 conf.default proxy_arp (template)"},
		{"/proc/sys/net/ipv4/conf/vethR-zwave/rp_filter", "1", "NET-5 veth rp_filter strict (inherited)"},
		{"/proc/sys/net/ipv4/conf/vethR-zwave/proxy_arp", "1", "NET-5 veth proxy_arp (inherited)"},
		{"/proc/sys/net/ipv4/conf/vethR-zwave/send_redirects", "0", "NET-5 veth send_redirects off (inherited)"},
		{"/proc/sys/net/ipv4/conf/all/rp_filter", "0", "NET-5 conf.all rp_filter (per-if authoritative)"},
		{"/proc/sys/net/ipv4/conf/all/accept_source_route", "0", "NET-5 source-route off"},
		{"/proc/sys/net/ipv4/conf/all/send_redirects", "0", "NET-5 all send_redirects off"},
		{"/proc/sys/net/ipv4/conf/all/accept_redirects", "0", "NET-5 accept_redirects off"},
		{"/proc/sys/net/ipv4/conf/vethR-zwave/accept_redirects", "0", "NET-5 veth accept_redirects off (inherited)"},
		{"/proc/sys/net/ipv4/conf/vethR-zwave/secure_redirects", "0", "NET-5 veth secure_redirects off (inherited)"},
		{"/proc/sys/net/netfilter/nf_conntrack_tcp_loose", "0", "NET-5 ct tcp_loose off"},
		{"/proc/sys/net/netfilter/nf_conntrack_tcp_be_liberal", "0", "NET-5 ct be_liberal off"},
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

// TestL1Gateway is fixture L1's router-local input chain (§4 IN-5/IN-6, host-only): the router's
// own address (the gateway) is default-deny but for ICMP. A container pings the gateway and gets a
// reply (input accepts icmp), but a TCP connect to the gateway on any port is dropped (input
// policy drop -- no router-local service is exposed). Same H/fixture, parallel rows.
func TestL1Gateway(t *testing.T) {
	h := newH()
	h.up(t, "l1.json")
	t.Cleanup(func() { h.down(t) })

	const gw = "10.0.0.1"
	checks := []struct {
		name string
		run  func(t *testing.T)
	}{
		// IN-5: icmp to the gateway is accepted into the router netns -> a ping reply.
		{"IN-5_ping_gateway", func(t *testing.T) { h.wantPing(t, "IN-5", "zwave", gw, true) }},
		// IN-6: tcp to the gateway has no input accept -> dropped (policy drop, no service exposed).
		{"IN-6_tcp_gateway", func(t *testing.T) { h.wantTCP(t, "IN-6", "zwave", gw, 22, denied) }},
	}
	for _, c := range checks {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			c.run(t)
		})
	}
}

// TestPodmanRun is the operator path (TEST-PLAN §1, black-box): a REAL `podman run --network
// ns:<pin> -v <hosts>:/etc/hosts` against a turnip netns, vs the `turnip probe` (podman unshare
// nsenter) shortcut the rest of the harness uses. It proves the pinned netns is attachable by a
// fresh container, the generated hosts file mounts so the container resolves its flow peer by
// NAME, and the flow matrix governs that container exactly as it governs the probe. Reuses L1's
// single allowed flow (zwave->hass:8080). Skips when -image is unset; -image is the tag of the
// probe image the host pre-loads at boot (the turnip-test-image service).
func TestPodmanRun(t *testing.T) {
	if *imageRef == "" {
		t.Skip("no -image configured")
	}
	h := newH()
	h.up(t, "l1.json")
	t.Cleanup(func() { h.down(t) })

	// -image is the tag of an already-loaded image (the host loads it at boot; you can't `podman
	// run` a tar). We assume it's present and run it directly.
	ref := *imageRef
	zwaveNS := stateDir + "/containers/zwave/netns"
	zwaveHosts := stateDir + "/containers/zwave/hosts"

	// POD-1: a real container in zwave's netns reaches hass (by IP) over the one allowed flow.
	if out, code := h.runAttached(t, ref, zwaveNS, zwaveHosts, "python3", "-c", connectTCP, "10.0.0.12", "8080"); code != reached {
		t.Errorf("POD-1: podman run zwave->10.0.0.12:8080 = %s, want reached\n%s", verdict(code), out)
	}
	// POD-2: the host bind works -- the container resolves the peer by NAME (hass -> 10.0.0.12 via
	// the bind-mounted /etc/hosts) and the same allowed flow reaches.
	if out, code := h.runAttached(t, ref, zwaveNS, zwaveHosts, "python3", "-c", connectTCP, "hass", "8080"); code != reached {
		t.Errorf("POD-2: podman run zwave->hass(by name):8080 = %s, want reached\n%s", verdict(code), out)
	}
	// POD-3: the flow matrix still governs the real container -- the wrong port drops.
	if out, code := h.runAttached(t, ref, zwaveNS, zwaveHosts, "python3", "-c", connectTCP, "hass", "9090"); code != denied {
		t.Errorf("POD-3: podman run zwave->hass:9090 = %s, want denied\n%s", verdict(code), out)
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

// TestL3Egress is fixture L3 (§4 egress): one network with an uplink and a container egr with a
// scoped egress flow (tcp:8443). egr reaches world on the allowed port and world sees a
// MASQUERADED source (EG-1+EG-2); the unscoped port is dropped at the router (EG-3). Needs world.
func TestL3Egress(t *testing.T) {
	h := newH()
	if h.world == nil {
		t.Skip("needs -world (the egress target)")
	}
	h.up(t, "l3.json")
	t.Cleanup(func() { h.down(t) })
	world := h.worldIPv4(t)

	// EG-1 + EG-2: egr egresses to world:8443 (allowed); world's peer-echo reports the source it
	// saw, which must be masqueraded to the host edge -- not egr's 10.0.0.11.
	out, code := h.probe(t, "egr", "python3", "-c", recvPeer, world, "8443")
	if code != 0 {
		t.Fatalf("EG-1: egr->world:8443 egress failed (code %d):\n%s", code, out)
	}
	seen := strings.TrimSpace(out)
	if seen == "10.0.0.11" {
		t.Errorf("EG-2: world saw the container IP %q -- masquerade not applied", seen)
	} else if !strings.HasPrefix(seen, "192.168.") {
		t.Errorf("EG-2: world saw %q, want the host's masqueraded test-LAN (192.168.x) addr", seen)
	}

	// EG-3: the egress is scoped to 8443, so 8080 is not allowed -> dropped at the router.
	h.wantTCP(t, "EG-3", "egr", world, 8080, unreached)
}

// TestL4Ingress is fixture L4 (§4 ingress): one network with an uplink and a container svc that
// publishes host:8080 -> svc:8080 (the IngressFlow omits `port`, exercising the default-to-
// host_port path). world (the external client) connects to the host's published port and lands on
// svc via prerouting DNAT (IN-1); svc sees world's REAL source IP, since ingress is DNAT-only and
// never masqueraded (IN-2); an unpublished port has no DNAT and is refused (IN-3). Needs world.
func TestL4Ingress(t *testing.T) {
	h := newH()
	if h.world == nil {
		t.Skip("needs -world (the ingress client)")
	}
	h.up(t, "l4.json")
	t.Cleanup(func() { h.down(t) })
	world := h.worldIPv4(t)

	// The in-svc listener runs via a LOCAL probe (h.host.Run, not h.probe -- t.Fatalf is illegal
	// off the test goroutine), so no ssh argv mangling. It prints the source it sees, then exits.
	type res struct {
		out  string
		code int
		err  error
	}
	srv := make(chan res, 1)
	go func() {
		argv := []string{*turnipBin, "-c", h.fixture, "probe", "svc", "--", "python3", "-c", acceptPeer, "8080"}
		out, code, err := h.host.Run(argv...)
		srv <- res{out, code, err}
	}()
	time.Sleep(1 * time.Second) // let the listener bind before the client dials

	// IN-1: world connects to the published host port (8080 -> svc:8080 via DNAT). socat's args
	// have no shell metacharacters, so they survive the ssh argv re-split. host resolves via the
	// driver's /etc/hosts; the DNAT listens on 0.0.0.0 so any host edge address matches.
	out, code, err := h.world.Run("socat", "-u", "/dev/null", "TCP:host:8080,connect-timeout=5")
	if err != nil {
		t.Fatalf("IN-1: world socat launch: %v\n%s", err, out)
	}
	if code != 0 {
		t.Errorf("IN-1: world->host:8080 ingress did not connect (code %d):\n%s", code, out)
	}

	// IN-2: the source svc saw must be world's REAL ip -- ingress is DNAT-only, not masqueraded.
	r := <-srv
	if r.err != nil {
		t.Fatalf("IN-2: svc listener: %v\n%s", r.err, r.out)
	}
	if r.code != 0 {
		t.Fatalf("IN-2: svc listener exited %d (no connection seen):\n%s", r.code, r.out)
	}
	if seen := strings.TrimSpace(r.out); seen != world {
		t.Errorf("IN-2: svc saw source %q, want world's real ip %q (ingress must not masquerade)", seen, world)
	}

	// IN-3: an unpublished port has no DNAT -> the connection is refused (or the host drops it).
	if _, code, err = h.world.Run("socat", "-u", "/dev/null", "TCP:host:9999,connect-timeout=5"); err != nil {
		t.Fatalf("IN-3: world socat launch: %v", err)
	} else if code == 0 {
		t.Errorf("IN-3: world->host:9999 connected, want refused (no DNAT for the unpublished port)")
	}
}

// TestBADSpoofedSource is fixture bad's anti-spoof invariant (§5 BAD-1, host-only): the adversary
// container forges an UNOWNED source address (10.0.0.99, attached to no veth) and raw-sends a burst
// at the gateway. The container's own stack emits them (CAP_NET_RAW via the probe), but the router
// drops every one on reverse-path failure -- the forged saddr routes back to no interface, let
// alone this veth. Asserted on the exact IPReversePathFilter counter delta, not a non-reply.
//
// The counter is netns-global, so this owns its router (a dedicated bad fixture) and reads
// before/after around its own burst -- no parallel spoof-sender may share the router meanwhile.
func TestBADSpoofedSource(t *testing.T) {
	h := newH()
	h.up(t, "bad.json")
	t.Cleanup(func() { h.down(t) })

	const burst = 5
	before := h.rpfDrops(t, "router:lan")

	// adv forges 10.0.0.99 (unowned) -> the gateway. The send itself succeeds in adv's netns.
	out, code := h.probe(t, "adv", "python3", "-c", spoofSend, "10.0.0.99", "10.0.0.1", strconv.Itoa(burst))
	if code != 0 {
		t.Fatalf("BAD-1: spoof send failed (code %d):\n%s", code, out)
	}

	if got := h.rpfDrops(t, "router:lan") - before; got != burst {
		t.Errorf("BAD-1: rp_filter dropped %d of %d forged-saddr packets, want all (before=%d)", got, burst, before)
	}
}

// TestBADLateralSpoof is fixture bad's lateral-spoof invariant (§5 BAD-2, host-only): adv forges
// victim's OWNED address (10.0.0.12) -- the case that distinguishes STRICT rp_filter from loose.
// The forged saddr does route (to victim's /32), so loose mode would accept it; strict requires
// the reverse path to leave the SAME veth the packet arrived on, and 10.0.0.12 routes via
// vethR-victim, not adv's ingress veth -> drop. Same exact-counter assertion as BAD-1.
//
// Separate top-level test (so it never runs concurrently with BAD-1) on a fresh bad fixture, so
// the netns-global IPReversePathFilter counter is its own to bracket.
func TestBADLateralSpoof(t *testing.T) {
	h := newH()
	h.up(t, "bad.json")
	t.Cleanup(func() { h.down(t) })

	const burst = 5
	before := h.rpfDrops(t, "router:lan")

	// adv impersonates victim (10.0.0.12) -> the gateway. Routable saddr, wrong ingress veth.
	out, code := h.probe(t, "adv", "python3", "-c", spoofSend, "10.0.0.12", "10.0.0.1", strconv.Itoa(burst))
	if code != 0 {
		t.Fatalf("BAD-2: spoof send failed (code %d):\n%s", code, out)
	}

	if got := h.rpfDrops(t, "router:lan") - before; got != burst {
		t.Errorf("BAD-2: rp_filter dropped %d of %d victim-spoofed packets, want all (before=%d)", got, burst, before)
	}
}

// TestBADOutOfState is fixture bad's stateful-firewall invariant (§5 BAD-4, host-only): an
// out-of-state TCP ACK (no SYN, no connection) is dropped EVEN toward an allowed (proto,dport).
// adv has a real flow to victim:8080, but a bare ACK at that very port is `ct invalid` -- conntrack
// tcp_loose is off (routerSysctls), so it is NOT picked up as a mid-stream connection -- and the
// forward chain's invalid rule drops it before the flow vmap. Without tcp_loose=0 the kernel
// default would silently adopt the ACK as `ct new` and forward it; this test pins that hardening.
//
// Targets the ALLOWED port deliberately: a denied port would also drop (policy), so it could not
// isolate the ct-invalid path. Asserted on the exact conntrack-invalid counter delta. Separate
// top-level test on a fresh fixture (netns-global counter, never concurrent with other senders).
func TestBADOutOfState(t *testing.T) {
	h := newH()
	h.up(t, "bad.json")
	t.Cleanup(func() { h.down(t) })

	const burst = 5
	before := h.ctInvalid(t, "router:lan")

	// adv sends bare ACKs at victim's ALLOWED port 8080 -- out-of-state, so still dropped.
	out, code := h.probe(t, "adv", "python3", "-c", ackSend, "10.0.0.11", "10.0.0.12", "8080", strconv.Itoa(burst))
	if code != 0 {
		t.Fatalf("BAD-4: ack send failed (code %d):\n%s", code, out)
	}

	if got := h.ctInvalid(t, "router:lan") - before; got != burst {
		t.Errorf("BAD-4: conntrack marked %d of %d out-of-state ACKs invalid, want all (before=%d)", got, burst, before)
	}
}

// TestBADArpPoison is fixture bad's ARP-spoof invariant (§5, host-only): adv blasts gratuitous ARP
// claiming victim's IP (10.0.0.12) is at adv's MAC -- the lateral hijack attempt. The poison gains
// nothing because the routed-/32 model decides delivery by ROUTING, not ARP: the router's /32
// device route (which the container can't touch) keeps 10.0.0.12 egressing vethR-victim, and an
// unsolicited ARP for an IP unknown on that veth plants no usable neighbor entry (arp_accept=0
// default). Two structural assertions on the router netns the attacker has no access to.
func TestBADArpPoison(t *testing.T) {
	h := newH()
	h.up(t, "bad.json")
	t.Cleanup(func() { h.down(t) })

	out, code := h.probe(t, "adv", "python3", "-c", arpPoison, "eth0", "10.0.0.12", "10")
	if code != 0 {
		t.Fatalf("BAD-ARP: gratuitous arp send failed (code %d):\n%s", code, out)
	}

	// 1) The poison plants no 10.0.0.12 neighbor on the attacker's router veth.
	neigh, _ := h.probe(t, "router:lan", "ip", "neigh", "show", "dev", "vethR-adv")
	if strings.Contains(neigh, "10.0.0.12") {
		t.Errorf("BAD-ARP: router planted a 10.0.0.12 neighbor on vethR-adv (poison took):\n%s", neigh)
	}

	// 2) Routing is the authority: victim's IP still egresses victim's veth, never the attacker's.
	rt, _ := h.probe(t, "router:lan", "ip", "route", "get", "10.0.0.12")
	has(t, rt, "dev vethR-victim", "BAD-ARP route authority")
	if strings.Contains(rt, "vethR-adv") {
		t.Errorf("BAD-ARP: 10.0.0.12 routes via the attacker's veth vethR-adv:\n%s", rt)
	}
}
