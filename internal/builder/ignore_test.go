package builder

import (
	"os"
	"path/filepath"
	"testing"
)

func writeIgnore(t *testing.T, contents string) *IgnoreList {
	t.Helper()
	dir := t.TempDir()
	if contents != "" {
		if err := os.WriteFile(filepath.Join(dir, IgnoreFileName), []byte(contents), 0644); err != nil {
			t.Fatal(err)
		}
	}
	il, err := LoadIgnoreList(dir)
	if err != nil {
		t.Fatal(err)
	}
	return il
}

func TestIgnoreMissingFileExcludesNothing(t *testing.T) {
	il := writeIgnore(t, "")
	for _, p := range []string{"main.go", "src/app.js", ".git/HEAD"} {
		if il.Match(p, false) {
			t.Errorf("with no %s present, %q should not be excluded", IgnoreFileName, p)
		}
	}
}

func TestIgnoreBasenamePatterns(t *testing.T) {
	il := writeIgnore(t, "*.log\n.git\n")
	excluded := []string{
		"debug.log",
		"src/nested/deep/trace.log",
		".git",
		".git/HEAD",
		".git/objects/ab/cdef",
		"vendor/.git/config",
	}
	kept := []string{"main.go", "src/app.js", "logs.txt", "gitconfig", "buildkit/x"}

	for _, p := range excluded {
		if !il.Match(p, false) {
			t.Errorf("%q should be excluded", p)
		}
	}
	for _, p := range kept {
		if il.Match(p, false) {
			t.Errorf("%q should be kept", p)
		}
	}
}

func TestIgnoreDirectoryRuleCoversSubtree(t *testing.T) {
	il := writeIgnore(t, "build/\n")

	// The directory itself, and anything under it. Entries below a matched
	// directory are excluded whatever their own kind.
	cases := []struct {
		path  string
		isDir bool
	}{
		{"build", true},
		{"build/out", true},
		{"build/out/app.bin", false},
	}
	for _, c := range cases {
		if !il.Match(c.path, c.isDir) {
			t.Errorf("%q (isDir=%v) should be excluded by 'build/'", c.path, c.isDir)
		}
	}
	// A directory rule must not match a sibling that merely shares a prefix.
	for _, p := range []string{"buildkit/x", "rebuild/y", "build.sh"} {
		if il.Match(p, false) {
			t.Errorf("%q should not be excluded by 'build/'", p)
		}
	}
}

func TestIgnoreNegationReincludes(t *testing.T) {
	il := writeIgnore(t, "*.log\n!important.log\n")
	if !il.Match("debug.log", false) {
		t.Error("debug.log should be excluded")
	}
	if il.Match("important.log", false) {
		t.Error("important.log should be re-included by the ! rule")
	}
}

func TestIgnoreLaterRulesWin(t *testing.T) {
	// Reverse order of the previous test: the re-include comes first, so the
	// broad exclusion afterwards should win.
	il := writeIgnore(t, "!important.log\n*.log\n")
	if !il.Match("important.log", false) {
		t.Error("a later broad rule should override an earlier negation")
	}
}

func TestIgnoreCommentsAndBlankLines(t *testing.T) {
	il := writeIgnore(t, "# a comment\n\n   \n*.tmp\n")
	if !il.Match("x.tmp", false) {
		t.Error("*.tmp should be excluded")
	}
	if il.Match("a comment", false) {
		t.Error("comment lines must not become patterns")
	}
}

func TestIgnoreDoubleStar(t *testing.T) {
	il := writeIgnore(t, "**/node_modules\n")
	for _, p := range []string{"node_modules", "a/node_modules", "a/b/node_modules/pkg/index.js"} {
		if !il.Match(p, false) {
			t.Errorf("%q should be excluded by '**/node_modules'", p)
		}
	}
	if il.Match("src/app.js", false) {
		t.Error("src/app.js should be kept")
	}
}

func TestIgnoreFileItselfAlwaysExcluded(t *testing.T) {
	il := writeIgnore(t, "*.log\n")
	if !il.Match(IgnoreFileName, false) {
		t.Errorf("%s must never be copied into an image", IgnoreFileName)
	}
}

// The point of the feature: an ignored file must not reach collectGlob, so it
// changes neither layer contents nor the COPY cache key.
func TestCollectGlobHonoursIgnoreList(t *testing.T) {
	ctx := t.TempDir()
	mustWrite := func(rel, body string) {
		t.Helper()
		p := filepath.Join(ctx, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0644); err != nil {
			t.Fatal(err)
		}
	}
	mustWrite("src/app.js", "app")
	mustWrite("src/debug.log", "noise")
	mustWrite(".git/HEAD", "ref: refs/heads/main")
	mustWrite(IgnoreFileName, "*.log\n.git\n")

	il, err := LoadIgnoreList(ctx)
	if err != nil {
		t.Fatal(err)
	}
	files, err := collectGlob(ctx, ".", il)
	if err != nil {
		t.Fatal(err)
	}

	got := map[string]bool{}
	for _, f := range files {
		got[filepath.ToSlash(f.RelPath)] = true
	}
	if !got["src/app.js"] {
		t.Error("src/app.js should have been collected")
	}
	for _, unwanted := range []string{"src/debug.log", ".git/HEAD", IgnoreFileName} {
		if got[unwanted] {
			t.Errorf("%q should have been ignored, got %v", unwanted, keys(got))
		}
	}
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// The trailing slash has to mean something. "build/" names a directory, so a
// regular file called "build" is not excluded by it — while the same rule
// without the slash excludes both. dirOnly was parsed and then never consulted,
// so the two spellings behaved identically and the slash was decorative.
func TestIgnoreDirectoryRuleDoesNotMatchAFileOfTheSameName(t *testing.T) {
	dirRule := writeIgnore(t, "build/\n")
	if dirRule.Match("build", false) {
		t.Error("'build/' must not exclude a regular file named build")
	}
	if !dirRule.Match("build", true) {
		t.Error("'build/' must exclude a directory named build")
	}

	plainRule := writeIgnore(t, "build\n")
	if !plainRule.Match("build", false) {
		t.Error("'build' (no slash) must exclude a regular file named build")
	}
	if !plainRule.Match("build", true) {
		t.Error("'build' (no slash) must exclude a directory named build")
	}
}
