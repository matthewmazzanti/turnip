package netns

import (
	"fmt"
	"os"
	osuser "os/user"
	"strconv"
	"testing"
)

// TestMain dispatches the re-exec'd provisioner: when `go test` runs THIS binary under
// `podman unshare <testbin> __provision ...` (via Bootstrap below), it provisions and exits
// -- so the child never runs the test suite. (Mirrors cmd/turnip's run() dispatch.)
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == ProvisionArg {
		if err := RunProvisioner(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func TestEncodeDecodeSpecsRoundTrip(t *testing.T) {
	specs := []Spec{{"router:fab", "/run/user/1001/turnip/routers/fab"}, {"container:a", "/p/a"}}
	got := decodeSpecs(encodeSpecs(specs))
	if len(got) != len(specs) {
		t.Fatalf("len = %d, want %d", len(got), len(specs))
	}
	for i := range specs {
		if got[i] != specs[i] {
			t.Errorf("spec[%d] = %+v, want %+v", i, got[i], specs[i])
		}
	}
}

// TestBootstrapRoundTrip drives the full bootstrap: re-exec into the provisioner under
// `podman unshare`, collect the netns fds, enter one, and close. Needs the rootful VM
// (real root + the rootless-podman owner via $SUDO_USER + podman); skips otherwise.
func TestBootstrapRoundTrip(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root (rootful bootstrap)")
	}
	username := os.Getenv("SUDO_USER")
	if username == "" {
		t.Skip("needs $SUDO_USER (the rootless-podman owner)")
	}
	u, err := osuser.Lookup(username)
	if err != nil {
		t.Fatalf("lookup %q: %v", username, err)
	}
	uid, _ := strconv.Atoi(u.Uid)
	gid, _ := strconv.Atoi(u.Gid)
	owner := Owner{User: username, UID: uid, GID: gid, Home: u.HomeDir}

	base := fmt.Sprintf("/run/user/%d/turnip-netnstest", uid)
	specs := []Spec{
		{Name: "router:fab", Path: base + "/routers/fab"},
		{Name: "container:a", Path: base + "/containers/a/netns"},
	}

	set, err := Bootstrap(owner, specs)
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	defer set.Close()

	for _, sp := range specs {
		if _, ok := set.FD(sp.Name); !ok {
			t.Errorf("missing fd for %q", sp.Name)
		}
	}

	// Enter one netns (a setns episode) and confirm it round-trips back to the host netns.
	ran := false
	if err := set.Enter("router:fab", func() error { ran = true; return nil }); err != nil {
		t.Fatalf("Enter: %v", err)
	}
	if !ran {
		t.Errorf("Enter did not run fn")
	}
	// Back on the host netns, a fresh open must still succeed (not poisoned).
	if fd, err := os.Open("/proc/thread-self/ns/net"); err != nil {
		t.Errorf("host netns unreadable after Enter: %v", err)
	} else {
		fd.Close()
	}
}
