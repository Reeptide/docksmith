package image

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func sampleManifest() *Manifest {
	return &Manifest{
		Name:    "myapp",
		Tag:     "latest",
		Created: "2026-01-01T00:00:00Z",
		Config: Config{
			Env:        []string{"A=1", "B=2"},
			Cmd:        []string{"/bin/sh", "-c", "echo hi"},
			WorkingDir: "/app",
		},
		Layers: []LayerEntry{
			{Digest: "sha256:aaa", Size: 100, CreatedBy: "COPY a b"},
			{Digest: "sha256:bbb", Size: 200, CreatedBy: "RUN echo hi"},
		},
	}
}

func TestComputeDigestIsStable(t *testing.T) {
	a, err := ComputeDigest(sampleManifest())
	if err != nil {
		t.Fatal(err)
	}
	b, err := ComputeDigest(sampleManifest())
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Errorf("digest not reproducible: %s vs %s", a, b)
	}
	if !strings.HasPrefix(a, "sha256:") {
		t.Errorf("digest %q should be sha256-prefixed", a)
	}
}

// The digest is computed over the manifest with the digest field cleared, so a
// previously-stamped digest must not feed back into the next computation.
func TestComputeDigestIgnoresExistingDigestField(t *testing.T) {
	clean, err := ComputeDigest(sampleManifest())
	if err != nil {
		t.Fatal(err)
	}

	stamped := sampleManifest()
	stamped.Digest = "sha256:previously-computed-value"
	got, err := ComputeDigest(stamped)
	if err != nil {
		t.Fatal(err)
	}
	if got != clean {
		t.Errorf("digest changed when the digest field was populated: %s vs %s", got, clean)
	}
	// ComputeDigest must restore what it temporarily cleared.
	if stamped.Digest != "sha256:previously-computed-value" {
		t.Errorf("ComputeDigest clobbered the caller's digest field: %q", stamped.Digest)
	}
}

func TestComputeDigestSensitiveToContent(t *testing.T) {
	base, err := ComputeDigest(sampleManifest())
	if err != nil {
		t.Fatal(err)
	}
	mutations := map[string]func(*Manifest){
		"name":       func(m *Manifest) { m.Name = "other" },
		"tag":        func(m *Manifest) { m.Tag = "v2" },
		"created":    func(m *Manifest) { m.Created = "2027-01-01T00:00:00Z" },
		"env":        func(m *Manifest) { m.Config.Env = []string{"A=changed"} },
		"cmd":        func(m *Manifest) { m.Config.Cmd = []string{"/bin/true"} },
		"workingdir": func(m *Manifest) { m.Config.WorkingDir = "/other" },
		"layer":      func(m *Manifest) { m.Layers[0].Digest = "sha256:changed" },
		"layerorder": func(m *Manifest) { m.Layers[0], m.Layers[1] = m.Layers[1], m.Layers[0] },
	}
	for name, mutate := range mutations {
		m := sampleManifest()
		mutate(m)
		got, err := ComputeDigest(m)
		if err != nil {
			t.Fatal(err)
		}
		if got == base {
			t.Errorf("changing %s did not change the manifest digest", name)
		}
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	m := sampleManifest()
	if err := Save(m, dir); err != nil {
		t.Fatal(err)
	}
	if m.Digest == "" {
		t.Error("Save should stamp the digest onto the manifest")
	}

	got, err := Load(dir, "myapp", "latest")
	if err != nil {
		t.Fatal(err)
	}
	if got.Digest != m.Digest {
		t.Errorf("loaded digest %s, want %s", got.Digest, m.Digest)
	}
	if got.Config.WorkingDir != "/app" || len(got.Layers) != 2 {
		t.Errorf("round-trip lost data: %+v", got)
	}

	// The stored digest must still verify against the stored content.
	recomputed, err := ComputeDigest(got)
	if err != nil {
		t.Fatal(err)
	}
	if recomputed != got.Digest {
		t.Errorf("stored digest %s does not verify (recomputed %s)", got.Digest, recomputed)
	}
}

func TestLoadMissingImageReportsNameAndTag(t *testing.T) {
	_, err := Load(t.TempDir(), "ghost", "v1")
	if err == nil {
		t.Fatal("expected an error for a missing image")
	}
	if !strings.Contains(err.Error(), "ghost") || !strings.Contains(err.Error(), "v1") {
		t.Errorf("error should name the image, got: %v", err)
	}
}

func TestListAllIgnoresNonManifestFiles(t *testing.T) {
	dir := t.TempDir()
	if err := Save(sampleManifest(), dir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("ignore me"), 0644); err != nil {
		t.Fatal(err)
	}
	// A temp file from an in-flight Save is not a manifest yet.
	if err := os.WriteFile(filepath.Join(dir, ".tmp-manifest-123"), []byte("{partial"), 0644); err != nil {
		t.Fatal(err)
	}

	all, err := ListAll(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 {
		t.Errorf("ListAll returned %d manifests, want 1", len(all))
	}
}

// A manifest that cannot be parsed must be a hard error, not a skip.
// referencedLayers builds the garbage-collection live set from ListAll, so
// treating an unreadable manifest as "references no layers" makes prune and
// rmi delete layers that a live image still needs.
func TestListAllRejectsCorruptManifest(t *testing.T) {
	dir := t.TempDir()
	if err := Save(sampleManifest(), dir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "broken.json"), []byte("{not json"), 0644); err != nil {
		t.Fatal(err)
	}

	all, err := ListAll(dir)
	if err == nil {
		t.Fatalf("a corrupt manifest must be an error, got %d manifests", len(all))
	}
	if !strings.Contains(err.Error(), "broken.json") {
		t.Errorf("error should name the offending file, got: %v", err)
	}
}

// Manifests arrive from untrusted archives during `docksmith load`, and their
// name and tag decide the path they are written to.
func TestManifestFileNameCannotTraverse(t *testing.T) {
	cases := []struct{ name, tag string }{
		{"evil", "../../../../tmp/pwned"},
		{"../../etc/passwd", "latest"},
		{"a/b", "c/d"},
		{"..", ".."},
		{"", ""},
		{"ok", "with space"},
	}
	for _, c := range cases {
		got := ManifestFileName(c.name, c.tag)
		if strings.ContainsAny(got, `/\`) {
			t.Errorf("ManifestFileName(%q, %q) = %q contains a path separator", c.name, c.tag, got)
		}
		// ".." as a substring of a longer filename is harmless; what matters is
		// that the result is a single path component that cannot walk upwards.
		if filepath.Base(got) != got || got == "." || got == ".." {
			t.Errorf("ManifestFileName(%q, %q) = %q is not a safe bare filename", c.name, c.tag, got)
		}
		if joined := filepath.Join("/images", got); filepath.Dir(joined) != "/images" {
			t.Errorf("ManifestFileName(%q, %q) = %q escapes when joined: %s", c.name, c.tag, got, joined)
		}
	}
}

func TestValidRefRejectsHostileReferences(t *testing.T) {
	bad := []struct{ name, tag string }{
		{"evil", "../../tmp/x"},
		{"..", "latest"},
		{"ok", "a/b"},
		{"", "latest"},
		{"ok", ""},
		{"ok", "with\x00null"},
	}
	for _, c := range bad {
		if err := ValidRef(c.name, c.tag); err == nil {
			t.Errorf("ValidRef(%q, %q) should fail", c.name, c.tag)
		}
	}
	good := []struct{ name, tag string }{
		{"busybox", "latest"},
		{"team/app", "v1.2.3"},
		{"my-app_2", "1.0"},
	}
	for _, c := range good {
		if err := ValidRef(c.name, c.tag); err != nil {
			t.Errorf("ValidRef(%q, %q) should pass: %v", c.name, c.tag, err)
		}
	}
}

// A name containing '/' must not turn into a nested path on disk.
func TestManifestFileNameFlattensSlashes(t *testing.T) {
	got := ManifestFileName("registry/team/app", "v1")
	if strings.Contains(got, "/") {
		t.Errorf("ManifestFileName = %q, must not contain a path separator", got)
	}
	if !strings.HasPrefix(got, "registry_team_app_v1_") || !strings.HasSuffix(got, ".json") {
		t.Errorf("ManifestFileName = %q, want the readable reference kept as a prefix", got)
	}
}

// Flattening is lossy, so it cannot be the whole file name. ValidRef permits
// "/" in a name, which means these three distinct references all sanitise to
// the same string: without a disambiguator one image silently overwrites
// another's manifest, `rmi` on either garbage-collects the other's layers, and
// `docksmith load` of an untrusted archive can clobber a local image just by
// picking a reference that collides.
func TestManifestFileNameDoesNotCollideAcrossDistinctReferences(t *testing.T) {
	refs := [][2]string{
		{"team/app", "v1"},
		{"team_app", "v1"},
		{"team", "app_v1"},
		{"team/app", "v-1"},
	}
	seen := make(map[string][2]string, len(refs))
	for _, r := range refs {
		got := ManifestFileName(r[0], r[1])
		if prev, dup := seen[got]; dup {
			t.Errorf("%v and %v both map to %s", prev, r, got)
		}
		seen[got] = r
	}
}

// The name must stay a pure function of the reference, or an image saved by one
// build is unfindable by the next.
func TestManifestFileNameIsStable(t *testing.T) {
	if a, b := ManifestFileName("team/app", "v1"), ManifestFileName("team/app", "v1"); a != b {
		t.Errorf("ManifestFileName is not deterministic: %q then %q", a, b)
	}
}

func TestParseNameTag(t *testing.T) {
	cases := []struct{ in, name, tag string }{
		{"busybox", "busybox", "latest"},
		{"busybox:latest", "busybox", "latest"},
		{"myapp:v1.2.3", "myapp", "v1.2.3"},
		{"team/app:dev", "team/app", "dev"},
	}
	for _, c := range cases {
		name, tag := ParseNameTag(c.in)
		if name != c.name || tag != c.tag {
			t.Errorf("ParseNameTag(%q) = (%q, %q), want (%q, %q)", c.in, name, tag, c.name, c.tag)
		}
	}
}

// Manifests are JSON on disk and are read back by `load` in a later step, so
// the shape has to survive an external round-trip unchanged.
func TestManifestJSONShape(t *testing.T) {
	data, err := json.Marshal(sampleManifest())
	if err != nil {
		t.Fatal(err)
	}
	var back Manifest
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatal(err)
	}
	if back.Config.Cmd[2] != "echo hi" || back.Layers[1].Size != 200 {
		t.Errorf("JSON round-trip altered the manifest: %+v", back)
	}
}
