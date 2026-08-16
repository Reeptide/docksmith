package safepath

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The escape this package exists to prevent: a layer ships "evil -> /etc" and
// then an entry for "evil/passwd". Both names are textually innocent, and
// filepath.Join alone lands the write on the host's /etc/passwd as root.
func TestResolveRejectsTraversalThroughASymlink(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "evil")); err != nil {
		t.Fatal(err)
	}

	for _, rel := range []string{"evil/passwd", "evil", "../escape", "a/../../escape"} {
		if got, err := Resolve(root, rel); err == nil && !strings.HasPrefix(got, root) {
			t.Errorf("Resolve(%q) = %q, which is outside %s", rel, got, root)
		}
	}
}

func TestResolveAllowsPathsThatDoNotExistYet(t *testing.T) {
	root := t.TempDir()
	got, err := Resolve(root, "a/b/c/new.txt")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(got, root) {
		t.Errorf("Resolve = %q, want a path under %s", got, root)
	}
}

// The distinction the two functions exist to draw. Resolve follows the final
// component, which is what a reader wants; ResolveNoFollow does not, which is
// what anything creating, replacing or deleting an entry needs.
func TestResolveNoFollowLeavesTheFinalComponentAlone(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "target"), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("target", filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}

	followed, err := Resolve(root, "link")
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(followed) != "target" {
		t.Errorf("Resolve(link) = %q, want it to follow through to target", followed)
	}

	direct, err := ResolveNoFollow(root, "link")
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(direct) != "link" {
		t.Errorf("ResolveNoFollow(link) = %q, want the link itself", direct)
	}
}

// Not following the last component must not weaken containment: the parent is
// still resolved, so a symlinked directory in the middle of the path is caught.
func TestResolveNoFollowStillRejectsEscapes(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "evil")); err != nil {
		t.Fatal(err)
	}
	if _, err := ResolveNoFollow(root, "evil/passwd"); err == nil {
		t.Error("ResolveNoFollow traversed a symlink pointing outside the root")
	}
	// A final component that itself points outside is fine to name — the caller
	// operates on the link, not on what it points at.
	got, err := ResolveNoFollow(root, "evil")
	if err != nil {
		t.Fatal(err)
	}
	if got != filepath.Join(mustEval(t, root), "evil") {
		t.Errorf("ResolveNoFollow(evil) = %q", got)
	}
}

func TestMkdirAllCreatesEveryLevelInsideRoot(t *testing.T) {
	root := t.TempDir()
	got, err := MkdirAll(root, "a/b/c")
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(got)
	if err != nil || !info.IsDir() {
		t.Fatalf("MkdirAll did not create a directory: %v", err)
	}
	if !strings.HasPrefix(got, mustEval(t, root)) {
		t.Errorf("MkdirAll = %q, outside %s", got, root)
	}
}

func TestMkdirAllWillNotDescendThroughAnEscapingSymlink(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "evil")); err != nil {
		t.Fatal(err)
	}
	if _, err := MkdirAll(root, "evil/sub"); err == nil {
		t.Error("MkdirAll created a directory outside the root")
	}
	if _, err := os.Stat(filepath.Join(outside, "sub")); err == nil {
		t.Error("a directory was created on the host side of the symlink")
	}
}

func mustEval(t *testing.T, p string) string {
	t.Helper()
	real, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatal(err)
	}
	return real
}
