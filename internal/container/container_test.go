package container

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func newRecord(t *testing.T, root, name string) *Record {
	t.Helper()
	id, err := NewID()
	if err != nil {
		t.Fatal(err)
	}
	r := &Record{
		ID:      id,
		Name:    name,
		Image:   "busybox:latest",
		Command: []string{"/bin/sh", "-c", "echo hi"},
	}
	if err := Create(root, r); err != nil {
		t.Fatal(err)
	}
	return r
}

func TestNewIDIsUniqueAndHex(t *testing.T) {
	seen := make(map[string]bool)
	for i := 0; i < 200; i++ {
		id, err := NewID()
		if err != nil {
			t.Fatal(err)
		}
		if len(id) != 64 {
			t.Fatalf("id length %d, want 64", len(id))
		}
		if strings.Trim(id, "0123456789abcdef") != "" {
			t.Fatalf("id is not hex: %q", id)
		}
		if seen[id] {
			t.Fatalf("duplicate id generated: %s", id)
		}
		seen[id] = true
	}
}

func TestShortID(t *testing.T) {
	if got := ShortID("0123456789abcdef0123"); got != "0123456789ab" {
		t.Errorf("ShortID = %q", got)
	}
	if got := ShortID("short"); got != "short" {
		t.Errorf("ShortID should leave short ids alone, got %q", got)
	}
}

func TestCreateSaveLoadRoundTrip(t *testing.T) {
	root := t.TempDir()
	r := newRecord(t, root, "test_container")
	r.WorkingDir = "/app"
	r.Env = map[string]string{"A": "1"}
	r.ExitCode = 7
	r.State = StateExited
	if err := Save(r); err != nil {
		t.Fatal(err)
	}

	got, err := Load(root, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "test_container" || got.WorkingDir != "/app" || got.ExitCode != 7 {
		t.Errorf("round-trip lost data: %+v", got)
	}
	if got.Env["A"] != "1" {
		t.Errorf("env lost: %v", got.Env)
	}
	if got.Dir == "" {
		t.Error("Dir should be populated on load")
	}
}

func TestCreateMakesRootFSDirectory(t *testing.T) {
	root := t.TempDir()
	r := newRecord(t, root, "x")
	info, err := os.Stat(r.RootFSPath())
	if err != nil || !info.IsDir() {
		t.Fatalf("rootfs directory not created: %v", err)
	}
	// Readable by an unprivileged `ps`/`logs` even when root created it.
	if info.Mode().Perm()&0055 == 0 {
		t.Errorf("container dir mode %v is not world-readable", info.Mode().Perm())
	}
}

// The same invariant under a hostile umask, which is where it actually broke:
// MkdirAll's mode argument is masked, and root's umask is 077 on a number of
// hardened distributions, so `sudo docksmith run` produced state that an
// unprivileged `docksmith ps` could not read.
func TestCreateIgnoresHostUmask(t *testing.T) {
	old := syscall.Umask(0077)
	defer syscall.Umask(old)

	root := t.TempDir()
	r := newRecord(t, root, "x")
	for _, dir := range []string{ContainersDir(root), r.Dir, r.RootFSPath()} {
		info, err := os.Stat(dir)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm()&0055 == 0 {
			t.Errorf("%s is %v under umask 077, want world-readable", dir, info.Mode().Perm())
		}
	}
}

func TestLoadMissingContainer(t *testing.T) {
	if _, err := Load(t.TempDir(), "deadbeef"); err == nil {
		t.Error("expected an error for a missing container")
	}
}

func TestListIsEmptyBeforeAnyContainers(t *testing.T) {
	got, err := List(t.TempDir())
	if err != nil {
		t.Fatalf("List on a fresh state root should not error: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %d records, want 0", len(got))
	}
}

func TestListSkipsCorruptRecords(t *testing.T) {
	root := t.TempDir()
	newRecord(t, root, "good")

	bad := filepath.Join(ContainersDir(root), "corrupt")
	if err := os.MkdirAll(bad, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bad, "config.json"), []byte("{nope"), 0644); err != nil {
		t.Fatal(err)
	}

	got, err := List(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Errorf("got %d records, want 1 (corrupt one skipped)", len(got))
	}
}

func TestResolveByPrefixNameAndFullID(t *testing.T) {
	root := t.TempDir()
	r := newRecord(t, root, "brave_anvil")

	for _, ref := range []string{r.ID, r.ID[:12], r.ID[:4], "brave_anvil"} {
		got, err := Resolve(root, ref)
		if err != nil {
			t.Errorf("Resolve(%q) failed: %v", ref, err)
			continue
		}
		if got.ID != r.ID {
			t.Errorf("Resolve(%q) returned the wrong container", ref)
		}
	}
}

func TestResolveAmbiguousPrefixIsAnError(t *testing.T) {
	root := t.TempDir()
	// Hand-build two records sharing a prefix rather than hoping random ids collide.
	for _, id := range []string{"abcdef1111111111", "abcdef2222222222"} {
		r := &Record{ID: id, Name: "n" + id[6:8], Image: "busybox:latest"}
		if err := Create(root, r); err != nil {
			t.Fatal(err)
		}
	}
	_, err := Resolve(root, "abcdef")
	if err == nil {
		t.Fatal("an ambiguous prefix should be an error")
	}
	if !strings.Contains(err.Error(), "ambiguous") {
		t.Errorf("error should say ambiguous, got: %v", err)
	}
}

func TestResolveExactNameBeatsPrefix(t *testing.T) {
	root := t.TempDir()
	// A container literally named after another's id prefix.
	target := &Record{ID: "1111111111111111", Name: "abcd", Image: "busybox:latest"}
	if err := Create(root, target); err != nil {
		t.Fatal(err)
	}
	other := &Record{ID: "abcd222222222222", Name: "other", Image: "busybox:latest"}
	if err := Create(root, other); err != nil {
		t.Fatal(err)
	}

	got, err := Resolve(root, "abcd")
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != target.ID {
		t.Errorf("exact name match should win over an id prefix, got %s", got.ID)
	}
}

func TestRemoveDeletesEverything(t *testing.T) {
	root := t.TempDir()
	r := newRecord(t, root, "doomed")
	if err := os.WriteFile(r.LogPath(), []byte("output"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := Remove(r); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(r.Dir); !os.IsNotExist(err) {
		t.Error("container directory survived Remove")
	}
}

func TestGenerateNameAvoidsCollisions(t *testing.T) {
	root := t.TempDir()
	taken := GenerateName(root)
	r := &Record{ID: "aaaa", Name: taken, Image: "busybox:latest"}
	if err := Create(root, r); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		if got := GenerateName(root); got == taken {
			t.Fatalf("GenerateName returned an in-use name: %s", got)
		}
	}
}

func TestStatusRendering(t *testing.T) {
	r := &Record{State: StateExited, ExitCode: 42, FinishedAt: time.Now().UTC().Format(time.RFC3339)}
	if !strings.Contains(r.Status(), "Exited (42)") {
		t.Errorf("Status = %q", r.Status())
	}

	r = &Record{State: StateRunning, StartedAt: time.Now().Add(-90 * time.Second).UTC().Format(time.RFC3339)}
	if !strings.HasPrefix(r.Status(), "Up ") {
		t.Errorf("Status = %q", r.Status())
	}

	r = &Record{State: StateCreated}
	if r.Status() != "Created" {
		t.Errorf("Status = %q", r.Status())
	}
}

func TestCommandStringTruncates(t *testing.T) {
	r := &Record{Command: []string{"/bin/sh", "-c", strings.Repeat("x", 100)}}
	if len(r.CommandString()) > 25 {
		t.Errorf("CommandString not truncated: %q", r.CommandString())
	}
	r = &Record{Command: []string{"/bin/true"}}
	if r.CommandString() != "/bin/true" {
		t.Errorf("short commands should be intact, got %q", r.CommandString())
	}
}
