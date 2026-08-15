package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseEnvArgs(t *testing.T) {
	got, err := parseEnvArgs([]string{"A=1", "B=", "CONN=postgres://h/db?x=y"})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"A": "1", "B": "", "CONN": "postgres://h/db?x=y"}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
}

func TestParseEnvArgsRejectsMalformed(t *testing.T) {
	for _, bad := range []string{"NOEQUALS", "=novalue", ""} {
		if _, err := parseEnvArgs([]string{bad}); err == nil {
			t.Errorf("parseEnvArgs(%q) should fail", bad)
		}
	}
}

func TestParseVolumeArgsRejectsDuplicateTargets(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a")
	b := filepath.Join(dir, "b")

	_, err := parseVolumeArgs([]string{a + ":/data", b + ":/data"})
	if err == nil {
		t.Fatal("two volumes on the same target should be rejected")
	}
	if !strings.Contains(err.Error(), "/data") {
		t.Errorf("error should name the conflicting target, got: %v", err)
	}
}

func TestParseVolumeArgsAcceptsDistinctTargets(t *testing.T) {
	dir := t.TempDir()
	// A read-only source is never auto-created, so it has to exist already.
	roSource := filepath.Join(dir, "two")
	if err := os.MkdirAll(roSource, 0755); err != nil {
		t.Fatal(err)
	}
	specs := []string{
		filepath.Join(dir, "one") + ":/one", // writable, created on demand
		roSource + ":/two:ro",
	}
	got, err := parseVolumeArgs(specs)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d mounts, want 2", len(got))
	}
	if got[0].ReadOnly {
		t.Error("first mount should be writable")
	}
	if !got[1].ReadOnly {
		t.Error("second mount should be read-only")
	}
}

func TestParseVolumeArgsEmpty(t *testing.T) {
	got, err := parseVolumeArgs(nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("got %d mounts, want 0", len(got))
	}
}
