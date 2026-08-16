package store

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWhiteoutPathRoundTrip(t *testing.T) {
	cases := map[string]string{
		"app/config.txt": "app/.wh.config.txt",
		"bin/ls":         "bin/.wh.ls",
		"toplevel":       ".wh.toplevel",
		".config":        ".wh..config",
		"a/b/c/d.txt":    "a/b/c/.wh.d.txt",
	}
	for rel, want := range cases {
		got := WhiteoutPath(rel)
		if got != want {
			t.Errorf("WhiteoutPath(%q) = %q, want %q", rel, got, want)
		}
		back, ok := IsWhiteout(got)
		if !ok {
			t.Errorf("IsWhiteout(%q) = false, want true", got)
			continue
		}
		if back != rel {
			t.Errorf("IsWhiteout(%q) = %q, want %q", got, back, rel)
		}
	}
}

func TestIsWhiteoutRejectsOrdinaryPaths(t *testing.T) {
	for _, name := range []string{"app/config.txt", "bin/ls", ".config", "a/.whatever"} {
		if target, ok := IsWhiteout(name); ok {
			t.Errorf("IsWhiteout(%q) = (%q, true), want false", name, target)
		}
	}
}

// A whiteout must delete the path it shadows and must not itself land on disk.
func TestExtractTarAppliesWhiteout(t *testing.T) {
	base, err := BuildTar([]TarFile{
		{Path: "app/", Mode: 0755, IsDir: true},
		{Path: "app/keep.txt", Mode: 0644, Content: []byte("keep")},
		{Path: "app/gone.txt", Mode: 0644, Content: []byte("gone")},
	})
	if err != nil {
		t.Fatal(err)
	}
	delta, err := BuildTar([]TarFile{
		{Path: "app/gone.txt", Mode: 0644, IsWhiteout: true},
	})
	if err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	if err := ExtractTar(base, dir); err != nil {
		t.Fatal(err)
	}
	if err := ExtractTar(delta, dir); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Lstat(filepath.Join(dir, "app/gone.txt")); !os.IsNotExist(err) {
		t.Errorf("app/gone.txt survived the whiteout (err=%v)", err)
	}
	if _, err := os.Lstat(filepath.Join(dir, "app/keep.txt")); err != nil {
		t.Errorf("app/keep.txt should be untouched: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(dir, "app/.wh.gone.txt")); !os.IsNotExist(err) {
		t.Error("the whiteout marker itself was written to disk")
	}
}

// One marker removes a whole subtree.
func TestExtractTarWhiteoutDirectory(t *testing.T) {
	base, err := BuildTar([]TarFile{
		{Path: "data/", Mode: 0755, IsDir: true},
		{Path: "data/nested/", Mode: 0755, IsDir: true},
		{Path: "data/nested/deep.txt", Mode: 0644, Content: []byte("deep")},
	})
	if err != nil {
		t.Fatal(err)
	}
	delta, err := BuildTar([]TarFile{{Path: "data", Mode: 0644, IsWhiteout: true}})
	if err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	if err := ExtractTar(base, dir); err != nil {
		t.Fatal(err)
	}
	if err := ExtractTar(delta, dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(dir, "data")); !os.IsNotExist(err) {
		t.Errorf("data/ subtree survived the whiteout (err=%v)", err)
	}
}

// The regression the plan review caught: byte-wise, ".config" sorts BEFORE
// ".wh..config", so relying on lexical order alone would extract the file and
// then immediately delete it. Whiteouts must be ordered ahead of all content.
func TestWhiteoutOrderingBeatsLexicalSortForDotfiles(t *testing.T) {
	for _, name := range []string{".config", ".git", ".bashrc", "-dash", "normal"} {
		data, err := BuildTar([]TarFile{
			{Path: name, Mode: 0644, Content: []byte("recreated")},
			{Path: name, Mode: 0644, IsWhiteout: true},
		})
		if err != nil {
			t.Fatal(err)
		}
		dir := t.TempDir()
		if err := ExtractTar(data, dir); err != nil {
			t.Fatal(err)
		}
		// Whiteout is applied first, so the recreated content must survive.
		got, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Errorf("%s: whiteout deleted content written later in the same layer: %v", name, err)
			continue
		}
		if string(got) != "recreated" {
			t.Errorf("%s: content = %q, want %q", name, got, "recreated")
		}
	}
}
