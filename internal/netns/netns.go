// Package netns owns the live network-namespace runtime state -- the open netns fds and
// their lifecycle -- and the rootful bootstrap that creates them. It is deliberately
// decoupled from config and policy: it knows only "named netns: here are their fds, enter
// them, close them." The config-derived runtime *model* (which netns to make, by what
// name + path) is a separate record the caller walks to build the []Spec it passes here.
//
// Why a re-exec under `podman unshare`
// ------------------------------------
// Everything must happen inside podman's user+mount namespaces (the mount ns so the
// persistent bind-mounts are visible; the user ns so we hold CAP_* over the namespaces
// podman owns). The Python tool entered them in-process, which Go's multithreaded runtime
// can't do for setns(CLONE_NEWUSER). So we use `podman unshare` as an EXEC boundary: it
// drops a fresh copy of this binary inside podman's userns, where the provisioner (the
// re-exec'd child, see Provision) creates each netns and ships its fd back over SCM_RIGHTS.
// The rootful parent then drives the dataplane against those fds (internal/dataplane).
//
// Two constraints the design respects (both validated in spike/go-netns-bootstrap):
//   - The provisioner cannot setns BACK to the host netns (it's owned by the ancestor init
//     userns -> EPERM); unshare always mints a fresh netns regardless, so it chains forward
//     and bind-mounts each WHILE IN IT, then exits.
//   - A netns persists by its bind-mount in podman's PERSISTENT pause-process mount ns, so
//     `podman run --network ns:<path>` can attach later. Set.Close drops only this
//     process's fd handles; it does not unmount.
package netns

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"syscall"

	"golang.org/x/sys/unix"
)

const (
	// ProvisionArg / TeardownArg are the hidden subcommands the parent re-execs into;
	// cmd/turnip routes them to RunProvisioner / RunTeardown from its normal arg dispatch.
	// Never user-typed.
	ProvisionArg = "__provision"
	TeardownArg  = "__teardown"
	bridgeFD     = 3 // the ExtraFiles SCM_RIGHTS socket in the re-exec'd child
)

// Owner is the rootless-podman owner the bootstrap drops to before `podman unshare`.
type Owner struct {
	User string
	UID  int
	GID  int
	Home string
}

// Spec is one netns to provision: a logical Name (the Set key, e.g. "router:<net>") and
// the bind-mount Path that pins it (e.g. <state_dir>/routers/<net>).
type Spec struct {
	Name string
	Path string
}

// Set is the live netns runtime state: open fds keyed by name, and their lifecycle. It
// holds no config and no policy -- just the fds the provisioner shipped back.
type Set struct {
	fds map[string]int
}

// FD returns the open netns fd for name.
func (s *Set) FD(name string) (int, bool) {
	fd, ok := s.fds[name]
	return fd, ok
}

// Close releases every fd. The netns themselves persist via their bind-mounts; Close drops
// only this process's handles. Returns the first close error, if any.
func (s *Set) Close() error {
	var first error
	for name, fd := range s.fds {
		if err := unix.Close(fd); err != nil && first == nil {
			first = fmt.Errorf("close netns %q: %w", name, err)
		}
	}
	s.fds = map[string]int{}
	return first
}

// Enter runs fn while the calling goroutine is setns'd into the named netns, restoring the
// host netns afterwards -- the episode used for the per-netns work that has no fd-targeted
// API (sysctls; an nft/netlink socket dialed in-netns). The rootful parent CAN return to
// the host netns (it holds CAP_SYS_ADMIN in its own init userns), unlike the provisioner.
func (s *Set) Enter(name string, fn func() error) error {
	fd, ok := s.fds[name]
	if !ok {
		return fmt.Errorf("netns %q not in set", name)
	}
	return inNetns(fd, fn)
}

// --- parent side: the bootstrap -------------------------------------------

// podmanUnshareCmd builds `podman unshare <self> <args...>` running as the rootless owner.
// Dropping to the owner via SysProcAttr.Credential (not `sudo -u`, which would close passed
// fds) makes `podman unshare` enter THAT user's persistent podman userns. USER/LOGNAME are
// set too, not just HOME/XDG_RUNTIME_DIR: under sudo they're still "root", and podman
// consults $USER for the subuid/subgid range -- left as root it finds none and falls back
// to a single uid mapping, whose userns disagrees with the autoSubUidGidRange `podman run`
// uses. (self is os.Executable, NOT "/proc/self/exe", which execve resolves against podman.)
func podmanUnshareCmd(owner Owner, args ...string) (*exec.Cmd, error) {
	self, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("resolve self: %w", err)
	}
	cmd := exec.Command("podman", append([]string{"unshare", self}, args...)...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	cmd.Dir = "/" // the child runs as the (dropped) owner; don't inherit a CWD it can't chdir into (e.g. root's home under sudo)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Credential: &syscall.Credential{Uid: uint32(owner.UID), Gid: uint32(owner.GID)},
	}
	cmd.Env = append(os.Environ(),
		"HOME="+owner.Home,
		"USER="+owner.User,
		"LOGNAME="+owner.User,
		fmt.Sprintf("XDG_RUNTIME_DIR=/run/user/%d", owner.UID),
	)
	return cmd, nil
}

// Bootstrap re-execs this binary under `podman unshare` as owner; the provisioner child
// pins each netns at its Path and ships the fds back over SCM_RIGHTS. Returns the live Set.
func Bootstrap(owner Owner, specs []Spec) (*Set, error) {
	if len(specs) == 0 {
		return &Set{fds: map[string]int{}}, nil
	}
	// SEQPACKET socketpair: one (name, fd) per message, so name<->fd can't misalign.
	pair, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_SEQPACKET|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("socketpair: %w", err)
	}
	parentSock := pair[0]
	childFile := os.NewFile(uintptr(pair[1]), "netns-bridge") // owns pair[1]; -> fd 3 in child

	cmd, err := podmanUnshareCmd(owner, append([]string{ProvisionArg}, encodeSpecs(specs)...)...)
	if err != nil {
		childFile.Close()
		unix.Close(parentSock)
		return nil, err
	}
	cmd.ExtraFiles = []*os.File{childFile} // -> fd 3 in the child

	if err := cmd.Start(); err != nil {
		childFile.Close()
		unix.Close(parentSock)
		return nil, fmt.Errorf("start `podman unshare`: %w", err)
	}
	childFile.Close() // the child holds its own fd 3 now

	fds, recvErr := recvFDs(parentSock)
	unix.Close(parentSock)
	waitErr := cmd.Wait()
	if recvErr != nil {
		closeAll(fds)
		return nil, fmt.Errorf("collect netns fds: %w", recvErr)
	}
	if waitErr != nil {
		closeAll(fds)
		return nil, fmt.Errorf("provisioner (`podman unshare`) failed: %w", waitErr)
	}
	for _, sp := range specs {
		if _, ok := fds[sp.Name]; !ok {
			closeAll(fds)
			return nil, fmt.Errorf("provisioner did not return netns %q", sp.Name)
		}
	}
	return &Set{fds: fds}, nil
}

// Teardown re-execs under `podman unshare` as owner to remove each pinned netns inside
// podman's mount ns -- the counterpart to Bootstrap/Provision. Unmounting a netns destroys
// it and everything inside (links, routes, sysctls, the nft table), so this is the whole
// netns teardown.
func Teardown(owner Owner, paths []string) error {
	if len(paths) == 0 {
		return nil
	}
	cmd, err := podmanUnshareCmd(owner, append([]string{TeardownArg}, paths...)...)
	if err != nil {
		return err
	}
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("teardown (`podman unshare`) failed: %w", err)
	}
	return nil
}

// --- child side: the provisioner ------------------------------------------

// RunProvisioner is the provisioner subcommand entrypoint: cmd/turnip's arg dispatch routes
// ProvisionArg here, passing the positional name/path args after it. The SCM_RIGHTS socket
// is the inherited ExtraFiles fd. It runs the provisioner and returns its error -- the
// caller's normal exit handling does the rest.
func RunProvisioner(args []string) error {
	return Provision(decodeSpecs(args), bridgeFD)
}

// RunTeardown is the teardown subcommand entrypoint (the TeardownArg re-exec): inside
// podman's mount ns it unmounts + removes each pinned netns. Best-effort -- a lazy unmount
// always detaches the netns (destroying it once unreferenced), and a leftover empty file is
// harmless (the next up's pin reclaims it).
func RunTeardown(paths []string) error {
	for _, p := range paths {
		_ = unix.Unmount(p, unix.MNT_DETACH)
		_ = os.Remove(p)
	}
	return nil
}

// Provision is the in-podman provisioner: inside podman's user+mount ns it creates and
// bind-mount-pins each netns, ships its fd back over sock, and exits. Closing sock signals
// EOF to the parent's collector.
func Provision(specs []Spec, sock int) error {
	defer unix.Close(sock)
	// We unshare THIS thread's netns once per spec and never return to the host netns, so
	// pin the goroutine. unshare mints a fresh netns regardless of the current one, so we
	// chain forward and bind-mount each while in it.
	runtime.LockOSThread()
	for _, sp := range specs {
		if err := pinNetns(sp.Path); err != nil {
			return fmt.Errorf("provision %q: %w", sp.Name, err)
		}
		fd, err := unix.Open("/proc/thread-self/ns/net", unix.O_RDONLY|unix.O_CLOEXEC, 0)
		if err != nil {
			return fmt.Errorf("provision %q: open netns fd: %w", sp.Name, err)
		}
		err = sendFD(sock, sp.Name, fd)
		unix.Close(fd) // the parent received its own dup; the in-flight SCM ref holds the ns
		if err != nil {
			return fmt.Errorf("provision %q: send fd: %w", sp.Name, err)
		}
	}
	return nil
}

// pinNetns mints a netns and pins it at path -- the `ip netns add` idiom at an arbitrary
// path. Idempotent: a stale pin from a prior run is lazily unmounted + removed first (up is
// clean-slate).
func pinNetns(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("mkdir state dir: %w", err)
	}
	_ = unix.Unmount(path, unix.MNT_DETACH) // drop any prior pin (ignore "not mounted")
	_ = os.Remove(path)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL, 0o444) // the bind-mount target
	if err != nil {
		return fmt.Errorf("create mount target: %w", err)
	}
	f.Close()
	if err := unix.Unshare(unix.CLONE_NEWNET); err != nil {
		return fmt.Errorf("unshare: %w", err)
	}
	// Pin this thread's own ns/net (an explicit task/<tid> path -- unambiguous about WHICH
	// thread's netns under Go's M:N runtime).
	src := fmt.Sprintf("/proc/self/task/%d/ns/net", unix.Gettid())
	if err := unix.Mount(src, path, "none", unix.MS_BIND, ""); err != nil {
		return fmt.Errorf("bind-mount %s -> %s: %w", src, path, err)
	}
	return nil
}

// --- the setns episode -----------------------------------------------------

// inNetns runs fn while the calling goroutine is setns'd into the netns fd refers to,
// restoring the host netns afterwards. LockOSThread pins the goroutine; a single deferred
// lambda owns cleanup -- always close the saved handle, but unlock ONLY when back on the
// host netns. A failed restore leaves the thread poisoned (still in the target netns), so
// we leave it LOCKED and let Go retire it rather than reuse it in the wrong ns.
//
// TODO: refine before this is load-bearing -- the "Go retires the locked thread" guarantee
// only fires when the GOROUTINE EXITS, which inNetns doesn't do (it returns to a live
// caller), so a poisoned thread stays pinned to that goroutine. The orchestration likely
// wants each setns episode on a dedicated short-lived goroutine. (Carried from the spike.)
func inNetns(fd int, fn func() error) error {
	runtime.LockOSThread()
	orig, err := unix.Open("/proc/thread-self/ns/net", unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		runtime.UnlockOSThread()
		return fmt.Errorf("open host netns: %w", err)
	}
	backOnHost := false
	defer func() {
		unix.Close(orig)
		if backOnHost {
			runtime.UnlockOSThread()
		}
	}()
	if err := unix.Setns(fd, unix.CLONE_NEWNET); err != nil {
		backOnHost = true // never left the host netns -> safe to unlock
		return fmt.Errorf("setns target: %w", err)
	}
	fnErr := fn()
	if err := unix.Setns(orig, unix.CLONE_NEWNET); err != nil {
		return fmt.Errorf("restore host netns (fn err=%v): %w", fnErr, err)
	}
	backOnHost = true
	return fnErr
}

// --- the SCM_RIGHTS fd bridge ----------------------------------------------

func sendFD(sock int, name string, fd int) error {
	return unix.Sendmsg(sock, []byte(name), unix.UnixRights(fd), nil, 0)
}

func recvFDs(sock int) (map[string]int, error) {
	out := map[string]int{}
	name := make([]byte, 256)
	oob := make([]byte, unix.CmsgSpace(4)) // room for exactly one fd
	for {
		// MSG_CMSG_CLOEXEC: received fds arrive WITHOUT cloexec otherwise, leaking into a
		// later exec.
		n, oobn, _, _, err := unix.Recvmsg(sock, name, oob, unix.MSG_CMSG_CLOEXEC)
		if err != nil {
			return out, fmt.Errorf("recvmsg: %w", err)
		}
		if n == 0 { // peer closed after the last fd -> EOF
			return out, nil
		}
		scms, err := unix.ParseSocketControlMessage(oob[:oobn])
		if err != nil {
			return out, fmt.Errorf("parse control message: %w", err)
		}
		var fds []int
		if len(scms) > 0 {
			if fds, err = unix.ParseUnixRights(&scms[0]); err != nil {
				return out, fmt.Errorf("parse rights: %w", err)
			}
		}
		if len(fds) == 0 {
			return out, fmt.Errorf("message %q carried no fd", string(name[:n]))
		}
		out[string(name[:n])] = fds[0]
	}
}

// --- spec wire encoding (parent argv -> child) -----------------------------

// Specs ride to the re-exec'd child as a flat name/path arg list (argv is already framed,
// so no escaping is needed and names/paths can't misalign with one pair per two args).
func encodeSpecs(specs []Spec) []string {
	args := make([]string, 0, len(specs)*2)
	for _, s := range specs {
		args = append(args, s.Name, s.Path)
	}
	return args
}

func decodeSpecs(args []string) []Spec {
	specs := make([]Spec, 0, len(args)/2)
	for i := 0; i+1 < len(args); i += 2 {
		specs = append(specs, Spec{Name: args[i], Path: args[i+1]})
	}
	return specs
}

func closeAll(fds map[string]int) {
	for _, fd := range fds {
		unix.Close(fd)
	}
}
