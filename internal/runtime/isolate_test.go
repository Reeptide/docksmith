package runtime

import (
	"encoding/json"
	"os/exec"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The child config crosses a process boundary as a single argv element, so it
// has to survive a JSON round-trip exactly — including fields the current step
// does not populate yet.
func TestChildConfigRoundTrip(t *testing.T) {
	want := ChildConfig{
		RootFS:   "/tmp/rootfs",
		WorkDir:  "/app",
		Command:  []string{"/bin/sh", "-c", "echo hi there"},
		Hostname: "abc123",
		UseInit:  true,
		Mounts: []Mount{
			{Source: "/host/data", Target: "/data"},
			{Source: "/host/ro", Target: "/ro", ReadOnly: true},
		},
		Network: &NetworkConfig{
			Interface: "eth0", IP: "172.30.0.5/16", Gateway: "172.30.0.1", MTU: 1500,
		},
	}

	data, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	var got ChildConfig
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(want, got) {
		t.Errorf("round-trip changed the config:\n want %+v\n  got %+v", want, got)
	}
}

// Arguments containing spaces and quotes used to be ambiguous under the old
// positional contract; as one JSON element they are not.
func TestChildConfigPreservesAwkwardArguments(t *testing.T) {
	want := []string{"/bin/sh", "-c", `echo "a b" && echo 'c  d'`, "arg with spaces", ""}
	data, err := json.Marshal(ChildConfig{Command: want})
	if err != nil {
		t.Fatal(err)
	}
	var got ChildConfig
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(want, got.Command) {
		t.Errorf("command mangled:\n want %q\n  got %q", want, got.Command)
	}
}

func TestEnvSliceIsSortedAndOverridesWin(t *testing.T) {
	got := envSlice(
		map[string]string{"B": "image", "A": "1", "C": "3"},
		map[string]string{"B": "override", "D": "4"},
	)
	want := []string{"A=1", "B=override", "C=3", "D=4"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("envSlice = %q, want %q", got, want)
	}
}

func TestEnvSliceDeterministic(t *testing.T) {
	env := map[string]string{"Z": "1", "A": "2", "M": "3", "B": "4", "Y": "5"}
	first := envSlice(env, nil)
	for i := 0; i < 50; i++ {
		if !reflect.DeepEqual(envSlice(env, nil), first) {
			t.Fatal("envSlice output depends on map iteration order")
		}
	}
}

func TestLookPathLeavesAbsolutePathsAlone(t *testing.T) {
	if got := lookPath("/bin/sh"); got != "/bin/sh" {
		t.Errorf("lookPath(/bin/sh) = %q", got)
	}
}

func TestLookPathFallsBackToTheBareName(t *testing.T) {
	t.Setenv("PATH", "/nonexistent-a:/nonexistent-b")
	// Unresolvable names are passed through so exec reports a proper ENOENT
	// naming the command, rather than this function inventing a path.
	if got := lookPath("definitely-not-a-real-binary"); got != "definitely-not-a-real-binary" {
		t.Errorf("lookPath = %q, want the name unchanged", got)
	}
}

// Every container gets PID 1 with signal handlers. Without it `stop` is broken
// by design: per pid_namespaces(7) a namespace's init only receives signals it
// has a handler for, even from an ancestor namespace, so an unhandled SIGTERM
// is discarded and stop always falls through to SIGKILL after the full timeout.
func TestInitIsAlwaysOn(t *testing.T) {
	for _, opts := range []RunOptions{
		{},
		{Command: []string{"/bin/sh"}},
		{Network: &NetworkConfig{}}, // a build RUN step
	} {
		if !childConfigFor(opts, "/").UseInit {
			t.Errorf("UseInit is false for %+v; stop would degrade to SIGKILL", opts)
		}
	}
}

// The config crosses a process boundary as a single JSON argv element, so a
// field the child needs but the parent never populates fails only at runtime,
// inside a namespace, with the setup error arriving down FD 3.
func TestChildConfigCarriesEveryOption(t *testing.T) {
	opts := RunOptions{
		RootFS:   "/var/lib/rootfs",
		Command:  []string{"/bin/sh", "-c", "echo hi"},
		Hostname: "abc123",
		Mounts:   []Mount{{Source: "/srv", Target: "/data", ReadOnly: true}},
		Network:  &NetworkConfig{Interface: "eth0", IP: "172.30.0.5/16", Gateway: "172.30.0.1"},
	}
	cfg := childConfigFor(opts, "/app")

	if cfg.RootFS != opts.RootFS {
		t.Errorf("RootFS = %q", cfg.RootFS)
	}
	if cfg.WorkDir != "/app" {
		t.Errorf("WorkDir = %q, want the resolved working directory", cfg.WorkDir)
	}
	if len(cfg.Command) != len(opts.Command) {
		t.Errorf("Command = %v", cfg.Command)
	}
	if cfg.Hostname != opts.Hostname {
		t.Errorf("Hostname = %q", cfg.Hostname)
	}
	if len(cfg.Mounts) != 1 || cfg.Mounts[0].Target != "/data" || !cfg.Mounts[0].ReadOnly {
		t.Errorf("Mounts = %+v", cfg.Mounts)
	}
	if cfg.Network == nil || cfg.Network.IP != opts.Network.IP {
		t.Errorf("Network = %+v", cfg.Network)
	}

	// And it must survive the round trip that actually happens.
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var back ChildConfig
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatal(err)
	}
	if back.Network == nil || back.Network.IP != opts.Network.IP || !back.UseInit {
		t.Errorf("config did not survive JSON: %+v", back)
	}
}

// A process killed by a signal has no exit code of its own: ExitCode() returns
// -1, which reaches os.Exit as 255 and is recorded as "Exited (-1)". `stop`
// escalating to SIGKILL takes this path on every forced stop.
func TestExitStatusMapsSignalDeaths(t *testing.T) {
	cmd := exec.Command("/bin/sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot start helper: %v", err)
	}
	go func() {
		time.Sleep(150 * time.Millisecond)
		cmd.Process.Signal(syscall.SIGKILL)
	}()
	err := cmd.Wait()
	exitErr, ok := err.(*exec.ExitError)
	if !ok {
		t.Fatalf("expected an ExitError, got %v", err)
	}
	if exitErr.ExitCode() != -1 {
		t.Skipf("platform reported %d rather than -1; nothing to map", exitErr.ExitCode())
	}
	if got := exitStatus(exitErr); got != 128+int(syscall.SIGKILL) {
		t.Errorf("exitStatus = %d, want %d (128+SIGKILL)", got, 128+int(syscall.SIGKILL))
	}
}

func TestExitStatusPassesOrdinaryExitCodesThrough(t *testing.T) {
	err := exec.Command("/bin/sh", "-c", "exit 42").Run()
	exitErr, ok := err.(*exec.ExitError)
	if !ok {
		t.Fatalf("expected an ExitError, got %v", err)
	}
	if got := exitStatus(exitErr); got != 42 {
		t.Errorf("exitStatus = %d, want 42", got)
	}
}

// __DOCKSMITH_CHILD__ is the sentinel the parent sets so the re-exec knows which
// mode it is in. It is docksmith's own plumbing: leaking it means every process
// in every container sees a variable naming the runtime it happens to run under,
// inherited by anything the container spawns, and able to confuse a nested
// docksmith into thinking it is a child.
func TestContainerEnvStripsTheChildSentinel(t *testing.T) {
	t.Setenv(childSentinel, "1")
	t.Setenv("PATH", "/usr/bin:/bin")
	t.Setenv("GREETING", "hello")

	env := containerEnv()
	for _, kv := range env {
		if strings.HasPrefix(kv, childSentinel+"=") {
			t.Errorf("%s leaked into the container environment", childSentinel)
		}
	}
	// And it strips only that: everything else must survive, or the container
	// loses its PATH and the image's ENV.
	for _, want := range []string{"PATH=/usr/bin:/bin", "GREETING=hello"} {
		if !containsEnv(env, want) {
			t.Errorf("%q missing from the container environment", want)
		}
	}
}

// A variable that merely starts with the sentinel's name is a different
// variable and must survive.
func TestContainerEnvKeepsSimilarlyNamedVariables(t *testing.T) {
	t.Setenv(childSentinel+"_EXTRA", "keep")
	if !containsEnv(containerEnv(), childSentinel+"_EXTRA=keep") {
		t.Errorf("%s_EXTRA was stripped; only the exact sentinel should be", childSentinel)
	}
}

func containsEnv(env []string, want string) bool {
	for _, kv := range env {
		if kv == want {
			return true
		}
	}
	return false
}
