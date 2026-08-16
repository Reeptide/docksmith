package runtime

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseMountValid(t *testing.T) {
	cases := []struct {
		spec string
		want Mount
	}{
		{"/host/data:/data", Mount{Source: "/host/data", Target: "/data"}},
		{"/host/data:/data:rw", Mount{Source: "/host/data", Target: "/data"}},
		{"/host/data:/data:ro", Mount{Source: "/host/data", Target: "/data", ReadOnly: true}},
		{"/a/b/../c:/x/./y", Mount{Source: "/a/c", Target: "/x/y"}},
		{"/:/hostroot:ro", Mount{Source: "/", Target: "/hostroot", ReadOnly: true}},
	}
	for _, c := range cases {
		got, err := ParseMount(c.spec)
		if err != nil {
			t.Errorf("ParseMount(%q) unexpected error: %v", c.spec, err)
			continue
		}
		if got != c.want {
			t.Errorf("ParseMount(%q) = %+v, want %+v", c.spec, got, c.want)
		}
	}
}

func TestParseMountRejectsBadSpecs(t *testing.T) {
	cases := map[string]string{
		"/host/data":              "no container path",
		"":                        "empty",
		":/data":                  "empty host path",
		"/host:":                  "empty container path",
		"relative/path:/data":     "relative host path",
		"/host:relative":          "relative container path",
		"/host:/data:rx":          "unknown option",
		"/host:/data:ro:extra":    "too many fields",
		"/host:/data:ro:rw:extra": "too many fields",
	}
	for spec, why := range cases {
		if _, err := ParseMount(spec); err == nil {
			t.Errorf("ParseMount(%q) should fail (%s)", spec, why)
		}
	}
}

// The error has to name the offending spec — a run with several -v flags is
// unusable otherwise.
func TestParseMountErrorNamesTheSpec(t *testing.T) {
	_, err := ParseMount("bad/path:/data")
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "bad/path") {
		t.Errorf("error should quote the spec, got: %v", err)
	}
}

func TestEnsureSourceCreatesMissingDirectory(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(base, "created", "nested")

	m := Mount{Source: target, Target: "/data"}
	if err := EnsureSource(m); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(target)
	if err != nil {
		t.Fatalf("source was not created: %v", err)
	}
	if !info.IsDir() {
		t.Error("source should be a directory")
	}
}

func TestEnsureSourceLeavesExistingPathAlone(t *testing.T) {
	base := t.TempDir()
	file := filepath.Join(base, "data.txt")
	if err := os.WriteFile(file, []byte("original"), 0644); err != nil {
		t.Fatal(err)
	}

	if err := EnsureSource(Mount{Source: file, Target: "/data.txt"}); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "original" {
		t.Errorf("existing source was modified: %q", body)
	}
}

// Silently creating an empty directory for a read-only mount hides a typo as a
// mysteriously empty volume.
func TestEnsureSourceRefusesToCreateForReadOnly(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "typo")
	err := EnsureSource(Mount{Source: missing, Target: "/data", ReadOnly: true})
	if err == nil {
		t.Fatal("expected an error for a missing read-only source")
	}
	if _, statErr := os.Stat(missing); statErr == nil {
		t.Error("the source should not have been created")
	}
}

// The recursive read-only bind needs root, but the part that decides *what* to
// remount does not, and that is where the bug lived: MS_RDONLY on a remount
// applies to that mount alone, so `-v /mnt:/data:ro` where /mnt has anything
// mounted inside produced a read-only /data with fully writable holes in it —
// worse than an honest failure, because the user was told it was read-only.
//
// Driven from a synthetic mountinfo. Reading the real one made the outcome
// depend on what this machine has mounted: an implementation returning only the
// target passed, because on a host with nothing nested under the probe path
// that is also the right answer — and "submounts are missed" is the entire bug.
const sampleMountinfo = `21 30 0:20 / /proc rw,nosuid,nodev,noexec shared:5 - proc proc rw
22 30 0:21 / /sys rw,nosuid,nodev,noexec shared:6 - sysfs sysfs rw
99 30 0:44 / /mnt/data rw,relatime shared:9 - ext4 /dev/sdb1 rw
100 99 0:45 / /mnt/data/inner rw,relatime shared:10 - tmpfs tmpfs rw
101 100 0:46 / /mnt/data/inner/deeper rw,relatime shared:11 - tmpfs tmpfs rw
102 30 0:47 / /mnt/database rw,relatime shared:12 - tmpfs tmpfs rw
103 30 0:48 / /mnt/data\040dir rw,relatime shared:13 - tmpfs tmpfs rw
`

func TestParseSubmountsFindsNestedMountsDeepestFirst(t *testing.T) {
	got := parseSubmounts(strings.NewReader(sampleMountinfo), "/mnt/data")

	want := map[string]bool{
		"/mnt/data":              true,
		"/mnt/data/inner":        true,
		"/mnt/data/inner/deeper": true,
	}
	if len(got) != len(want) {
		t.Fatalf("parseSubmounts = %v, want exactly %d entries", got, len(want))
	}
	for _, p := range got {
		if !want[p] {
			t.Errorf("%q should not be in the result: %v", p, got)
		}
	}

	// Deepest first: a child must be made read-only before its parent, so a
	// failure partway down cannot undo an already-applied parent.
	if got[len(got)-1] != "/mnt/data" {
		t.Errorf("target should come last (deepest first), got %v", got)
	}
	for i := 1; i < len(got); i++ {
		if strings.Count(got[i-1], "/") < strings.Count(got[i], "/") {
			t.Fatalf("not ordered deepest-first: %v", got)
		}
	}
}

// A sibling sharing a prefix is not underneath: /mnt/data must not drag in
// /mnt/database, or an unrelated filesystem is silently made read-only.
func TestParseSubmountsIgnoresPrefixSiblings(t *testing.T) {
	for _, p := range parseSubmounts(strings.NewReader(sampleMountinfo), "/mnt/data") {
		if strings.HasPrefix(p, "/mnt/database") {
			t.Errorf("/mnt/data must not match /mnt/database")
		}
	}
}

// A path with no submounts still yields itself, or the caller remounts nothing
// and silently leaves the bind writable.
func TestParseSubmountsAlwaysIncludesTheTargetItself(t *testing.T) {
	got := parseSubmounts(strings.NewReader(sampleMountinfo), "/mnt/nothing-here")
	if len(got) != 1 || got[0] != "/mnt/nothing-here" {
		t.Errorf("parseSubmounts = %v, want just the target", got)
	}
}

// An escaped mount point is decoded before comparison, or it is neither matched
// as a submount nor remounted correctly.
func TestParseSubmountsDecodesEscapedPaths(t *testing.T) {
	got := parseSubmounts(strings.NewReader(sampleMountinfo), "/mnt")
	var found bool
	for _, p := range got {
		if p == "/mnt/data dir" {
			found = true
		}
	}
	if !found {
		t.Errorf("escaped mount point was not decoded: %v", got)
	}
}

// The live wrapper still has to work against the real kernel.
func TestSubmountsUnderReadsTheRealMountinfo(t *testing.T) {
	got, err := submountsUnder("/proc")
	if err != nil {
		t.Skipf("cannot read mountinfo: %v", err)
	}
	if len(got) == 0 || got[len(got)-1] != "/proc" {
		t.Errorf("submountsUnder(/proc) = %v, want /proc itself last", got)
	}
}

// mountinfo octal-escapes space, tab, newline and backslash in mount points.
// Failing to decode them targets the wrong path, which for a read-only remount
// means silently not applying it.
func TestUnescapeMountPath(t *testing.T) {
	cases := []struct{ in, want string }{
		{"/mnt/plain", "/mnt/plain"},
		{`/mnt/with\040space`, "/mnt/with space"},
		{`/mnt/with\011tab`, "/mnt/with\ttab"},
		{`/mnt/back\134slash`, `/mnt/back\slash`},
		{`/mnt/trailing\`, `/mnt/trailing\`},
	}
	for _, c := range cases {
		if got := unescapeMountPath(c.in); got != c.want {
			t.Errorf("unescapeMountPath(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
