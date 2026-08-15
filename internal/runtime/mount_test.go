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
