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
		if il.Match(p) {
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
		if !il.Match(p) {
			t.Errorf("%q should be excluded", p)
		}
	}
	for _, p := range kept {
		if il.Match(p) {
			t.Errorf("%q should be kept", p)
		}
	}
}

func TestIgnoreDirectoryRuleCoversSubtree(t *testing.T) {
	il := writeIgnore(t, "build/\n")
	for _, p := range []string{"build", "build/out", "build/out/app.bin"} {
		if !il.Match(p) {
			t.Errorf("%q should be excluded by 'build/'", p)
		}
	}
	// A directory rule must not match a sibling that merely shares a prefix.
	for _, p := range []string{"buildkit/x", "rebuild/y", "build.sh"} {
		if il.Match(p) {
			t.Errorf("%q should not be excluded by 'build/'", p)
		}
	}
}

func TestIgnoreNegationReincludes(t *testing.T) {
	il := writeIgnore(t, "*.log\n!important.log\n")
	if !il.Match("debug.log") {
		t.Error("debug.log should be excluded")
	}
	if il.Match("important.log") {
		t.Error("important.log should be re-included by the ! rule")
	}
}

func TestIgnoreLaterRulesWin(t *testing.T) {
	// Reverse order of the previous test: the re-include comes first, so the
	// broad exclusion afterwards should win.
	il := writeIgnore(t, "!important.log\n*.log\n")
	if !il.Match("important.log") {
		t.Error("a later broad rule should override an earlier negation")
	}
}

func TestIgnoreCommentsAndBlankLines(t *testing.T) {
	il := writeIgnore(t, "# a comment\n\n   \n*.tmp\n")
	if !il.Match("x.tmp") {
		t.Error("*.tmp should be excluded")
	}
	if il.Match("a comment") {
		t.Error("comment lines must not become patterns")
	}
}

func TestIgnoreDoubleStar(t *testing.T) {
	il := writeIgnore(t, "**/node_modules\n")
	for _, p := range []string{"node_modules", "a/node_modules", "a/b/node_modules/pkg/index.js"} {
		if !il.Match(p) {
			t.Errorf("%q should be excluded by '**/node_modules'", p)
		}
	}
	if il.Match("src/app.js") {
		t.Error("src/app.js should be kept")
	}
}

func TestIgnoreFileItselfAlwaysExcluded(t *testing.T) {
	il := writeIgnore(t, "*.log\n")
	if !il.Match(IgnoreFileName) {
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
