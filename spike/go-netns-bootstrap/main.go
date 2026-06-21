// go-netns-bootstrap -- spike for the Go rewrite's bootstrap + fd-collection step.
//
// It proves the rootful-only path turnip's Go port will use: instead of entering
// podman's user+mount namespaces IN-PROCESS (the Python tool does this single-
// threaded, which Go's multithreaded runtime can't do for CLONE_NEWUSER), we use
// `podman unshare` as an exec boundary -- it puts a FRESH process inside podman's
// userns for us, sidestepping the single-threaded-setns problem entirely.
//
// Flow:
//   parent (real root, run via sudo)
//     -> drop to the rootless user + `podman unshare <self> --phase1 <names...>`
//        (the exec-boundary version of turnip's enter_podman; sudo would close the
//         passed fd, so we drop in-process via SysProcAttr.Credential)
//   phase1 child (inside podman's user+mount ns, mapped to uid 0, holds CAP_*)
//     -> for each name: unshare a fresh netns, send its fd back over SCM_RIGHTS
//   parent
//     -> collect every (name, fd) into one registry, then prove each fd is:
//          1. present (all requested names came back)
//          2. a distinct namespace (distinct nsfs inode)
//          3. operable AS ROOT (enter via netlink, do a CAP_NET_ADMIN op) -- the
//             rootful thesis: init-root has caps over the podman-userns-owned netns.
//
// The netns here are anonymous (kept alive purely by the held fds) -- persistence
// for `podman run --network ns:<path>` needs a bind-mount under a user-writable
// state dir, which is a separate concern (see README). This spike is ONLY about
// the bootstrap + getting the fds together in one place.
package main

import (
	"fmt"
	"os"
	"os/exec"
	osuser "os/user"
	"runtime"
	"strconv"
	"syscall"

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

// fd 3: the first entry in cmd.ExtraFiles lands here in the execed phase1 child.
const bridgeFd = 3

func main() {
	if len(os.Args) > 1 && os.Args[1] == "--phase1" {
		phase1(os.Args[2:])
		return
	}
	parent()
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
	pair, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_SEQPACKET|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		fatal("socketpair: %v", err)
	}
	parentSock, childSock := pair[0], pair[1]

	// os.Executable, NOT the literal "/proc/self/exe": execve resolves that magic path
	// against the CALLING process at exec time -- which is podman, mid-setup -- so it
	// would re-exec podman, not us.
	self, err := os.Executable()
	if err != nil {
		fatal("os.Executable: %v", err)
	}

	childFile := os.NewFile(uintptr(childSock), "bridge") // -> fd 3 in the child
	args := append([]string{"unshare", self, "--phase1"}, nsNames...)
	cmd := exec.Command("podman", args...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	cmd.ExtraFiles = []*os.File{childFile}
	// Drop to the rootless user so `podman unshare` enters THAT user's podman userns
	// (a child of init's userns, which root still has full caps over). In-process drop
	// via Credential, not `sudo -u` -- sudo closes inherited fds and would drop fd 3.
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Credential: &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid)},
	}
	cmd.Env = append(os.Environ(),
		"HOME="+u.HomeDir,
		fmt.Sprintf("XDG_RUNTIME_DIR=/run/user/%d", uid),
	)

	fmt.Printf("[parent] root pid %d -> `podman unshare` as %s (uid %d), requesting %d netns\n",
		os.Getpid(), username, uid, len(nsNames))
	if err := cmd.Start(); err != nil {
		fatal("start `podman unshare`: %v", err)
	}
	childFile.Close() // the child holds its own fd 3 now; drop the parent's copy

	fds := recvFdsByName(parentSock)
	unix.Close(parentSock)
	if err := cmd.Wait(); err != nil {
		fatal("phase1 child (`podman unshare`) failed: %v", err)
	}

	checkFds(fds)
}

// --- phase 1 child (inside podman's user+mount ns) -------------------------

func phase1(names []string) {
	// Sanity: did fd 3 survive `podman unshare`'s re-exec? If podman closed inherited
	// fds, the whole bridge is moot -- say so clearly (the fallback is an abstract-
	// namespace socket dialed by name, see README).
	if _, err := unix.Getsockname(bridgeFd); err != nil {
		fatalChild("fd %d is not a socket (%v): `podman unshare` did not forward the "+
			"inherited fd -- switch the bridge to an abstract-namespace socket", bridgeFd, err)
	}

	runtime.LockOSThread() // we mutate THIS thread's netns repeatedly; pin the goroutine
	orig, err := netns.Get()
	if err != nil {
		fatalChild("get host netns: %v", err)
	}

	for _, name := range names {
		// A fresh netns on the current thread; no bind-mount -- the fd we ship keeps it
		// alive in the parent. unshare needs CAP_SYS_ADMIN in our userns, which we hold
		// as uid-0-in-podman's-userns.
		if err := unix.Unshare(unix.CLONE_NEWNET); err != nil {
			fatalChild("unshare netns for %q: %v", name, err)
		}
		ns, err := netns.Get()
		if err != nil {
			fatalChild("get fresh netns for %q: %v", name, err)
		}
		if err := sendFdByName(bridgeFd, name, int(ns)); err != nil {
			fatalChild("send fd for %q: %v", name, err)
		}
		ns.Close() // the parent received its own dup via SCM_RIGHTS
		if err := netns.Set(orig); err != nil {
			fatalChild("restore host netns before next unshare: %v", err)
		}
		fmt.Fprintf(os.Stderr, "[phase1] created + sent %q\n", name)
	}
	orig.Close()
	unix.Close(bridgeFd) // close -> EOF, unblocking the parent's recv loop
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

// --- checks (black-box, hand-authored expectations) ------------------------

func checkFds(fds map[string]int) {
	fmt.Printf("\n[parent] collected %d netns fd(s) in one place\n", len(fds))
	ok := true

	// 1. every requested netns came back, keyed by its type:name
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

	// 2. distinct namespaces -- distinct nsfs inode, not one ns aliased N times
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

	// 3. each fd is operable FROM THE ROOT PARENT: enter via netlink (NewHandleAt does
	//    setns(CLONE_NEWNET) under the hood) and run a CAP_NET_ADMIN op. Green here is
	//    the rootful thesis -- init-root wields caps over the podman-userns-owned netns.
	for name, fd := range fds {
		if !probeCaps(name, fd) {
			ok = false
		}
	}

	if ok {
		fmt.Println("\nPASS")
		return
	}
	fmt.Println("\nFAIL")
	os.Exit(1)
}

func probeCaps(name string, fd int) bool {
	h, err := netlink.NewHandleAt(netns.NsHandle(fd))
	if err != nil {
		fmt.Printf("  [FAIL] %q: NewHandleAt: %v\n", name, err)
		return false
	}
	defer h.Close()
	dummy := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "probe0"}}
	if err := h.LinkAdd(dummy); err != nil {
		fmt.Printf("  [FAIL] %q: LinkAdd (caps?): %v\n", name, err)
		return false
	}
	links, err := h.LinkList()
	if err != nil {
		fmt.Printf("  [FAIL] %q: LinkList: %v\n", name, err)
		return false
	}
	found := false
	for _, l := range links {
		if l.Attrs().Name == "probe0" {
			found = true
		}
	}
	_ = h.LinkDel(dummy)
	if !found {
		fmt.Printf("  [FAIL] %q: probe0 missing after add\n", name)
		return false
	}
	fmt.Printf("  [ok] %q: entered as root + created link (CAP_NET_ADMIN over podman netns)\n", name)
	return true
}

// --- diagnostics -----------------------------------------------------------

func fatal(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "error: "+format+"\n", a...)
	os.Exit(1)
}

func fatalChild(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "[phase1] error: "+format+"\n", a...)
	os.Exit(1)
}
