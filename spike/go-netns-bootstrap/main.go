// go-netns-bootstrap -- spike for the Go rewrite's bootstrap, fd-collection, and
// netns-persistence steps (rootful only).
//
// Bootstrap: instead of entering podman's user+mount namespaces IN-PROCESS (the
// Python tool does this single-threaded, which Go's multithreaded runtime can't do
// for CLONE_NEWUSER), we use `podman unshare` as an exec boundary -- it drops a FRESH
// process inside podman's userns for us.
//
// Flow:
//   parent (real root, via sudo)
//     -> drop to the rootless user + `podman unshare <self> --phase1 <names...>`
//   phase1 child (inside podman's user+mount ns, mapped to uid 0)
//     -> per name: create a netns AT A BIND-MOUNT PATH under the user runtime dir
//        (the from-scratch `ip netns add` idiom -- unshare + bind-mount /proc/.../ns/net),
//        drop a persistent marker iface, and ship the fd back over SCM_RIGHTS
//   parent
//     -> collect every fd into one registry; prove each is present, distinct, and
//        operable AS ROOT (CAP_NET_ADMIN over the podman-userns-owned netns);
//     -> CLOSE all fds (so ONLY the bind-mount keeps each netns alive), then launch a
//        SECOND `podman unshare <self> --verify <paths...>` that must still find each
//        netns at its path with the marker -- the persistence proof. It validates the
//        foundational assumption that `podman unshare` joins the PERSISTENT pause-process
//        mount ns (so a later `podman run --network ns:<path>` can attach), not a
//        transient one that dies with phase 1.
package main

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	osuser "os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"

	"github.com/google/nftables"
	"github.com/google/nftables/expr"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
)

// The netns the parent asks phase 1 to create, keyed BY TYPE (router:<net> /
// container:<name>) like turnip -- so a router and a container that share a name
// can't collide in the registry.
var nsNames = []string{
	"router:fabric",
	"container:zwave",
	"container:hass",
	"container:proxy",
}

const (
	bridgeFd  = 3            // first cmd.ExtraFiles entry lands here in the phase1 child
	markerIf  = "marker0"    // persistent iface phase1 leaves in each netns (identity proof)
	nsfsMagic = 0x6e736673   // NSFS_MAGIC -- a live netns mount vs an empty leftover file
)

func main() {
	switch {
	case len(os.Args) > 1 && os.Args[1] == "--phase1":
		phase1(os.Args[2:])
	case len(os.Args) > 1 && os.Args[1] == "--verify":
		verify(os.Args[2:])
	default:
		parent()
	}
}

// --- parent (real root) ----------------------------------------------------

func parent() {
	if os.Geteuid() != 0 {
		fatal("run via sudo: the host process owns the dataplane over the collected fds")
	}
	username := os.Getenv("SUDO_USER")
	if username == "" {
		fatal("set $SUDO_USER (invoke via sudo): need the rootless-podman owner to enter its userns")
	}
	u, err := osuser.Lookup(username)
	if err != nil {
		fatal("lookup user %q: %v", username, err)
	}
	uid, _ := strconv.Atoi(u.Uid)
	gid, _ := strconv.Atoi(u.Gid)

	// SEQPACKET socketpair: one (name, fd) per message, explicit framing -- the name
	// rides in the SAME SCM_RIGHTS message as its fd so the two can't misalign.
	sp, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_SEQPACKET|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		fatal("socketpair: %v", err)
	}
	parentSock, childSock := sp[0], sp[1]

	childFile := os.NewFile(uintptr(childSock), "bridge") // -> fd 3 in the child
	cmd := podmanUnshareCmd(uid, gid, u.HomeDir, append([]string{selfExe(), "--phase1"}, nsNames...)...)
	cmd.ExtraFiles = []*os.File{childFile}

	fmt.Printf("[parent] root pid %d -> `podman unshare` as %s (uid %d), requesting %d netns\n",
		os.Getpid(), username, uid, len(nsNames))
	if err := cmd.Start(); err != nil {
		fatal("start `podman unshare`: %v", err)
	}
	childFile.Close()

	fds := recvFdsByName(parentSock)
	unix.Close(parentSock)
	if err := cmd.Wait(); err != nil {
		fatal("phase1 child (`podman unshare`) failed: %v", err)
	}

	fdOK := checkFds(fds)

	// Drop every fd so ONLY the bind-mount keeps each netns alive -- otherwise the held
	// fd would mask a failed/transient bind-mount and the persistence check would lie.
	for _, fd := range fds {
		unix.Close(fd)
	}
	persistOK := verifyPersistence(uid, gid, u.HomeDir)

	if fdOK && persistOK {
		fmt.Println("\nPASS")
		return
	}
	fmt.Println("\nFAIL")
	os.Exit(1)
}

// podmanUnshareCmd builds `podman unshare <args...>` that runs as the rootless user
// (so it enters THAT user's persistent podman userns). In-process drop via Credential,
// not `sudo -u` -- sudo closes inherited fds and would drop the phase1 bridge fd.
func podmanUnshareCmd(uid, gid int, home string, args ...string) *exec.Cmd {
	cmd := exec.Command("podman", append([]string{"unshare"}, args...)...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Credential: &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid)},
	}
	cmd.Env = append(os.Environ(),
		"HOME="+home,
		fmt.Sprintf("XDG_RUNTIME_DIR=/run/user/%d", uid),
	)
	return cmd
}

// verifyPersistence launches a SEPARATE `podman unshare ... --verify` (phase1 is gone,
// its fds closed): if the bind-mounts landed in podman's persistent pause-process mount
// ns, this fresh unshare joins the same ns and still sees them.
func verifyPersistence(uid, gid int, home string) bool {
	paths := make([]string, len(nsNames))
	for i, n := range nsNames {
		paths[i] = statePath(fmt.Sprintf("/run/user/%d", uid), n)
	}
	fmt.Printf("\n[parent] fds closed; re-entering a FRESH `podman unshare` to check persistence\n")
	cmd := podmanUnshareCmd(uid, gid, home, append([]string{selfExe(), "--verify"}, paths...)...)
	return cmd.Run() == nil
}

// --- phase 1 child (inside podman's user+mount ns) -------------------------

func phase1(names []string) {
	if _, err := unix.Getsockname(bridgeFd); err != nil {
		fatalChild("fd %d is not a socket (%v): `podman unshare` did not forward the "+
			"inherited fd -- switch the bridge to an abstract-namespace socket", bridgeFd, err)
	}
	runtimeDir := os.Getenv("XDG_RUNTIME_DIR") // the parent set this to /run/user/<owner-uid>

	// We unshare THIS thread's netns repeatedly, so pin the goroutine to it. We do NOT
	// restore the host netns and never return to it: phase 1 runs inside podman's userns,
	// but the host netns is owned by the INIT userns (an ancestor), where our mapped-root
	// holds no caps -- so setns BACK into it is EPERM. unshare always mints a fresh netns
	// regardless of the current one, so we chain forward and bind-mount each WHILE IN IT,
	// before moving on.
	runtime.LockOSThread()

	for _, name := range names {
		path := statePath(runtimeDir, name)
		if err := createPersistentNetns(path); err != nil {
			fatalChild("create+pin netns %q: %v", name, err)
		}
		// A persistent marker in THIS (current) netns -- left in place so --verify can
		// confirm it found the SAME live netns, not an empty leftover file.
		if err := netlink.LinkAdd(&netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: markerIf}}); err != nil {
			fatalChild("add marker in %q: %v", name, err)
		}
		ns, err := netns.Get()
		if err != nil {
			fatalChild("get netns fd for %q: %v", name, err)
		}
		if err := sendFdByName(bridgeFd, name, int(ns)); err != nil {
			fatalChild("send fd for %q: %v", name, err)
		}
		ns.Close()
		fmt.Fprintf(os.Stderr, "[phase1] created+pinned %s (marker %s), sent %q\n", path, markerIf, name)
	}
	unix.Close(bridgeFd) // close -> EOF, unblocking the parent's recv loop
}

// createPersistentNetns mints a netns and PINS it at `path` -- the from-scratch
// `ip netns add` idiom at an arbitrary path (vishvananda/netns.NewNamed only targets
// /run/netns, which the mapped-root user can't write). Idempotent: a stale pin from a
// prior run is lazily unmounted + removed first.
func createPersistentNetns(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("mkdir state dir: %w", err)
	}
	_ = unix.Unmount(path, unix.MNT_DETACH) // drop a prior run's pin (ignore "not mounted")
	_ = os.Remove(path)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL, 0o444) // the bind-mount target file
	if err != nil {
		return fmt.Errorf("create mount target: %w", err)
	}
	f.Close()
	if err := unix.Unshare(unix.CLONE_NEWNET); err != nil { // fresh netns on this thread
		return fmt.Errorf("unshare: %w", err)
	}
	// Pin it: bind-mount this thread's own ns/net onto the target file (an explicit
	// task/<tid> path -- unambiguous about WHICH thread's netns under Go's M:N runtime).
	src := fmt.Sprintf("/proc/self/task/%d/ns/net", unix.Gettid())
	if err := unix.Mount(src, path, "none", unix.MS_BIND, ""); err != nil {
		return fmt.Errorf("bind-mount %s -> %s: %w", src, path, err)
	}
	return nil
}

func statePath(runtimeDir, name string) string {
	return filepath.Join(runtimeDir, "turnip-spike", strings.ReplaceAll(name, ":", "_"))
}

// --- verify (inside a SECOND, independent `podman unshare`) -----------------

func verify(paths []string) {
	ok := true
	for _, p := range paths {
		fd, err := unix.Open(p, unix.O_RDONLY|unix.O_CLOEXEC, 0)
		if err != nil {
			fmt.Printf("  [FAIL] %s: open (mount vanished -- transient ns?): %v\n", p, err)
			ok = false
			continue
		}
		var sf unix.Statfs_t
		if err := unix.Fstatfs(fd, &sf); err != nil || sf.Type != nsfsMagic {
			fmt.Printf("  [FAIL] %s: not a netns mount (bind-mount didn't persist; empty file left)\n", p)
			ok = false
			unix.Close(fd)
			continue
		}
		h, err := netlink.NewHandleAt(netns.NsHandle(fd))
		if err != nil {
			fmt.Printf("  [FAIL] %s: NewHandleAt: %v\n", p, err)
			ok = false
			unix.Close(fd)
			continue
		}
		links, err := h.LinkList()
		h.Close()
		unix.Close(fd)
		if err != nil {
			fmt.Printf("  [FAIL] %s: LinkList: %v\n", p, err)
			ok = false
			continue
		}
		found := false
		for _, l := range links {
			if l.Attrs().Name == markerIf {
				found = true
			}
		}
		if !found {
			fmt.Printf("  [FAIL] %s: a netns, but marker %q missing (not the one we created)\n", p, markerIf)
			ok = false
			continue
		}
		fmt.Printf("  [ok] %s persisted (live netns mount, marker %q present)\n", p, markerIf)
	}
	if !ok {
		os.Exit(1)
	}
}

// --- the SCM_RIGHTS fd bridge ----------------------------------------------

func sendFdByName(sock int, name string, fd int) error {
	return unix.Sendmsg(sock, []byte(name), unix.UnixRights(fd), nil, 0)
}

func recvFdsByName(sock int) map[string]int {
	out := map[string]int{}
	name := make([]byte, 256)
	oob := make([]byte, unix.CmsgSpace(4)) // room for exactly one fd
	for {
		// MSG_CMSG_CLOEXEC: received fds arrive WITHOUT cloexec otherwise, and would
		// leak into any later exec.
		n, oobn, _, _, err := unix.Recvmsg(sock, name, oob, unix.MSG_CMSG_CLOEXEC)
		if err != nil {
			fatal("recvmsg: %v", err)
		}
		if n == 0 { // peer closed after the last fd -> EOF
			return out
		}
		scms, err := unix.ParseSocketControlMessage(oob[:oobn])
		if err != nil {
			fatal("parse control message: %v", err)
		}
		var fds []int
		if len(scms) > 0 {
			if fds, err = unix.ParseUnixRights(&scms[0]); err != nil {
				fatal("parse rights: %v", err)
			}
		}
		if len(fds) == 0 {
			fatal("message %q carried no fd", string(name[:n]))
		}
		out[string(name[:n])] = fds[0]
	}
}

// --- fd checks (bootstrap thesis) ------------------------------------------

func checkFds(fds map[string]int) bool {
	fmt.Printf("\n[parent] collected %d netns fd(s) in one place\n", len(fds))
	ok := true

	missing := false
	for _, name := range nsNames {
		if _, present := fds[name]; !present {
			fmt.Printf("  [FAIL] no fd for %q\n", name)
			missing, ok = true, false
		}
	}
	if !missing && len(fds) == len(nsNames) {
		fmt.Printf("  [ok] all %d requested netns present, keyed by name\n", len(nsNames))
	}

	inodes := map[uint64]string{}
	for name, fd := range fds {
		var st unix.Stat_t
		if err := unix.Fstat(fd, &st); err != nil {
			fmt.Printf("  [FAIL] fstat %q: %v\n", name, err)
			ok = false
			continue
		}
		if prev, dup := inodes[st.Ino]; dup {
			fmt.Printf("  [FAIL] %q and %q are the SAME netns (ino %d)\n", name, prev, st.Ino)
			ok = false
		}
		inodes[st.Ino] = name
	}
	if len(inodes) == len(fds) {
		fmt.Printf("  [ok] %d distinct netns inode(s)\n", len(inodes))
	}

	for name, fd := range fds {
		if !applyDataplane(name, fd) {
			ok = false
		}
	}
	return ok
}

// applyDataplane proves the THREE kernel-config interfaces over a collected fd, all
// from the ROOT parent (which can round-trip setns -- unlike phase 1): netlink
// (link+addr+route), sysctls (a setns episode), and nft (the netns-bound nf_tables
// socket). Together they're everything the real dataplane needs.
func applyDataplane(name string, fd int) bool {
	if err := netlinkDataplane(fd); err != nil {
		fmt.Printf("  [FAIL] %q: netlink: %v\n", name, err)
		return false
	}
	if err := sysctlDataplane(fd); err != nil {
		fmt.Printf("  [FAIL] %q: sysctl: %v\n", name, err)
		return false
	}
	if err := nftDataplane(fd); err != nil {
		fmt.Printf("  [FAIL] %q: nft: %v\n", name, err)
		return false
	}
	fmt.Printf("  [ok] %q: netlink (gw0 +addr +route) + sysctl (ip_forward) + nft (inet table) by fd\n", name)
	return true
}

// netlinkDataplane: a gateway-style dummy with a /32 address and a /32 link route --
// the create_gateway/connect ops, via a netns-bound vishvananda handle (NewHandleAt
// does the setns at socket-dial under the hood; CAP_NET_ADMIN is the root parent's).
func netlinkDataplane(fd int) error {
	h, err := netlink.NewHandleAt(netns.NsHandle(fd))
	if err != nil {
		return err
	}
	defer h.Close()
	if err := h.LinkAdd(&netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "gw0"}}); err != nil {
		return fmt.Errorf("LinkAdd: %w", err)
	}
	link, err := h.LinkByName("gw0") // re-fetch for the kernel-assigned index
	if err != nil {
		return fmt.Errorf("LinkByName: %w", err)
	}
	addr, err := netlink.ParseAddr("10.0.0.1/32")
	if err != nil {
		return err
	}
	if err := h.AddrAdd(link, addr); err != nil {
		return fmt.Errorf("AddrAdd: %w", err)
	}
	if err := h.LinkSetUp(link); err != nil {
		return fmt.Errorf("LinkSetUp: %w", err)
	}
	_, dst, err := net.ParseCIDR("10.0.0.2/32")
	if err != nil {
		return err
	}
	if err := h.RouteAdd(&netlink.Route{
		LinkIndex: link.Attrs().Index, Dst: dst, Scope: unix.RT_SCOPE_LINK,
	}); err != nil {
		return fmt.Errorf("RouteAdd: %w", err)
	}
	addrs, err := h.AddrList(link, netlink.FAMILY_V4)
	if err != nil || len(addrs) == 0 {
		return fmt.Errorf("addr readback empty (err=%v)", err)
	}
	return nil
}

// sysctlDataplane: set + read back net.ipv4.ip_forward inside the netns. There's no
// netlink verb for this -- /proc/sys/net is keyed to the process's netns -- so it needs
// a real setns episode. The root parent CAN return to the host netns afterwards (it has
// CAP_SYS_ADMIN in its own init userns); phase 1 could not.
func sysctlDataplane(fd int) error {
	return inNetns(fd, func() error {
		path := "/proc/sys/net/ipv4/ip_forward"
		if err := os.WriteFile(path, []byte("1"), 0); err != nil {
			return fmt.Errorf("write: %w", err)
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read: %w", err)
		}
		if got := strings.TrimSpace(string(b)); got != "1" {
			return fmt.Errorf("readback=%q want 1", got)
		}
		return nil
	})
}

// nftDataplane: load an inet table+chain+rule via google/nftables bound to the netns by
// fd (WithNetNSFd) -- the nf_tables netlink socket, NO `nft` subprocess. Confirm by
// listing the netns's tables.
func nftDataplane(fd int) error {
	c, err := nftables.New(nftables.WithNetNSFd(fd))
	if err != nil {
		return fmt.Errorf("new conn: %w", err)
	}
	t := c.AddTable(&nftables.Table{Family: nftables.TableFamilyINet, Name: "turnipspike"})
	pol := nftables.ChainPolicyAccept
	ch := c.AddChain(&nftables.Chain{
		Name: "forward", Table: t,
		Type: nftables.ChainTypeFilter, Hooknum: nftables.ChainHookForward,
		Priority: nftables.ChainPriorityFilter, Policy: &pol,
	})
	c.AddRule(&nftables.Rule{
		Table: t, Chain: ch,
		Exprs: []expr.Any{&expr.Verdict{Kind: expr.VerdictAccept}},
	})
	if err := c.Flush(); err != nil {
		return fmt.Errorf("flush: %w", err)
	}
	tables, err := c.ListTables()
	if err != nil {
		return fmt.Errorf("list tables: %w", err)
	}
	for _, tb := range tables {
		if tb.Name == "turnipspike" {
			return nil
		}
	}
	return fmt.Errorf("table turnipspike absent after flush")
}

// inNetns runs fn while the calling goroutine is setns'd into the netns `fd` refers to,
// restoring the host netns afterwards. LockOSThread pins the goroutine; on a failed
// restore the thread is poisoned, so we deliberately do NOT unlock it (Go retires it).
func inNetns(fd int, fn func() error) error {
	runtime.LockOSThread()
	orig, err := netns.Get()
	if err != nil {
		runtime.UnlockOSThread()
		return fmt.Errorf("get host netns: %w", err)
	}
	if err := netns.Set(netns.NsHandle(fd)); err != nil {
		orig.Close()
		runtime.UnlockOSThread()
		return fmt.Errorf("setns target: %w", err)
	}
	fnErr := fn()
	if err := netns.Set(orig); err != nil {
		orig.Close() // poisoned thread: no UnlockOSThread
		return fmt.Errorf("restore host netns (fn err=%v): %w", fnErr, err)
	}
	orig.Close()
	runtime.UnlockOSThread()
	return fnErr
}

// --- helpers ---------------------------------------------------------------

// selfExe is the absolute path to this binary -- NOT the literal "/proc/self/exe",
// which execve resolves against the calling process (podman, mid-setup) and would
// re-exec podman instead of us.
func selfExe() string {
	self, err := os.Executable()
	if err != nil {
		fatal("os.Executable: %v", err)
	}
	return self
}

func fatal(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "error: "+format+"\n", a...)
	os.Exit(1)
}

func fatalChild(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "[phase1] error: "+format+"\n", a...)
	os.Exit(1)
}
