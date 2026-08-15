package runtime

import (
	"encoding/json"
	"reflect"
	"testing"
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

// UseInit is derived from NoInit, so the default must be "init on".
func TestInitIsOnByDefault(t *testing.T) {
	cfg := ChildConfig{UseInit: !RunOptions{}.NoInit}
	if !cfg.UseInit {
		t.Error("containers should run with an init by default")
	}
	cfg = ChildConfig{UseInit: !RunOptions{NoInit: true}.NoInit}
	if cfg.UseInit {
		t.Error("NoInit should disable the init shim")
	}
}
