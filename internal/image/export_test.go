package image

import (
	"archive/tar"
	"bytes"
	"io"
	"strings"
	"testing"

	"docksmith/internal/store"
)

// buildStore creates a store holding a base image and two images derived from
// it, so layer sharing is exercised.
func buildStore(t *testing.T) (*store.State, string) {
	t.Helper()
	st, err := store.NewState(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	baseData, err := store.BuildTar([]store.TarFile{
		{Path: "bin/", Mode: 0755, IsDir: true},
		{Path: "bin/sh", Mode: 0755, Content: []byte("shell")},
	})
	if err != nil {
		t.Fatal(err)
	}
	baseDigest, err := st.WriteLayer(baseData)
	if err != nil {
		t.Fatal(err)
	}
	base := LayerEntry{Digest: baseDigest, Size: int64(len(baseData)), CreatedBy: "base"}

	mkApp := func(name, body string) {
		t.Helper()
		data, err := store.BuildTar([]store.TarFile{
			{Path: "app/", Mode: 0755, IsDir: true},
			{Path: "app/main", Mode: 0644, Content: []byte(body)},
		})
		if err != nil {
			t.Fatal(err)
		}
		digest, err := st.WriteLayer(data)
		if err != nil {
			t.Fatal(err)
		}
		m := &Manifest{
			Name: name, Tag: "1", Created: "2026-01-01T00:00:00Z",
			Config: Config{Cmd: []string{"/bin/sh"}, WorkingDir: "/"},
			Layers: []LayerEntry{base, {Digest: digest, Size: int64(len(data)), CreatedBy: "COPY"}},
		}
		if err := Save(m, st.ImagesDir); err != nil {
			t.Fatal(err)
		}
	}

	if err := Save(&Manifest{
		Name: "busybox", Tag: "latest", Created: "2026-01-01T00:00:00Z",
		Layers: []LayerEntry{base},
	}, st.ImagesDir); err != nil {
		t.Fatal(err)
	}
	mkApp("appa", "a")
	mkApp("appb", "b")

	return st, baseDigest
}

func exportTo(t *testing.T, st *store.State, refs ...string) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := Export(st, refs, &buf); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func archiveNames(t *testing.T, data []byte) []string {
	t.Helper()
	var out []string
	tr := tar.NewReader(bytes.NewReader(data))
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, hdr.Name)
	}
	return out
}

func TestExportArchiveLayout(t *testing.T) {
	st, _ := buildStore(t)
	names := archiveNames(t, exportTo(t, st, "appa:1"))

	if len(names) == 0 || names[0] != "index.json" {
		t.Errorf("index.json should come first, got %v", names)
	}
	var manifests, layers int
	for _, n := range names {
		switch {
		case strings.HasPrefix(n, "manifests/"):
			manifests++
		case strings.HasPrefix(n, "layers/"):
			layers++
		}
	}
	if manifests != 1 {
		t.Errorf("got %d manifests, want 1", manifests)
	}
	if layers != 2 {
		t.Errorf("got %d layers, want 2 (base + app)", layers)
	}
}

// Two images sharing a base must not carry that base twice.
func TestExportDeduplicatesSharedLayers(t *testing.T) {
	st, _ := buildStore(t)
	names := archiveNames(t, exportTo(t, st, "appa:1", "appb:1"))

	seen := make(map[string]bool)
	for _, n := range names {
		if !strings.HasPrefix(n, "layers/") {
			continue
		}
		if seen[n] {
			t.Errorf("layer %s appears twice in the archive", n)
		}
		seen[n] = true
	}
	// base + appa + appb = 3 distinct layers.
	if len(seen) != 3 {
		t.Errorf("got %d distinct layers, want 3: %v", len(seen), names)
	}
}

func TestExportUnknownImageFails(t *testing.T) {
	st, _ := buildStore(t)
	var buf bytes.Buffer
	if err := Export(st, []string{"nosuch:1"}, &buf); err == nil {
		t.Error("exporting a missing image should fail")
	}
}

func TestExportNoImagesFails(t *testing.T) {
	st, _ := buildStore(t)
	var buf bytes.Buffer
	if err := Export(st, nil, &buf); err == nil {
		t.Error("exporting nothing should fail")
	}
}

// The core round trip: save, wipe the store, load, and get the same image back.
func TestRoundTripThroughEmptyStore(t *testing.T) {
	src, _ := buildStore(t)
	archive := exportTo(t, src, "appa:1", "appb:1")

	dst, err := store.NewState(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	results, err := Import(dst, bytes.NewReader(archive))
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 {
		t.Fatalf("imported %d images, want 2", len(results))
	}

	for _, ref := range []string{"appa", "appb"} {
		orig, err := Load(src.ImagesDir, ref, "1")
		if err != nil {
			t.Fatal(err)
		}
		got, err := Load(dst.ImagesDir, ref, "1")
		if err != nil {
			t.Fatalf("%s missing after import: %v", ref, err)
		}
		if got.Digest != orig.Digest {
			t.Errorf("%s digest changed: %s -> %s", ref, orig.Digest, got.Digest)
		}
		for _, l := range got.Layers {
			if !dst.LayerExists(l.Digest) {
				t.Errorf("%s: layer %s not present after import", ref, l.Digest)
			}
			a, _ := src.ReadLayer(l.Digest)
			b, _ := dst.ReadLayer(l.Digest)
			if !bytes.Equal(a, b) {
				t.Errorf("%s: layer %s bytes differ after round trip", ref, l.Digest)
			}
		}
	}
}

func TestImportIsIdempotent(t *testing.T) {
	src, _ := buildStore(t)
	archive := exportTo(t, src, "appa:1")

	dst, err := store.NewState(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Import(dst, bytes.NewReader(archive)); err != nil {
		t.Fatal(err)
	}
	results, err := Import(dst, bytes.NewReader(archive))
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || !results[0].Skipped {
		t.Errorf("re-importing should report the image as already present, got %+v", results)
	}
}

// This is the payoff of content addressing: a tampered layer is detected, not
// silently trusted.
func TestImportRejectsCorruptLayer(t *testing.T) {
	src, _ := buildStore(t)
	archive := exportTo(t, src, "appa:1")

	corrupted := corruptLayerBytes(t, archive)

	dst, err := store.NewState(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	_, err = Import(dst, bytes.NewReader(corrupted))
	if err == nil {
		t.Fatal("importing a corrupt layer should fail")
	}
	if !strings.Contains(err.Error(), "corrupt") {
		t.Errorf("error should say the layer is corrupt, got: %v", err)
	}
}

// A failed import must leave the store untouched rather than half-updated.
func TestImportCommitsNothingOnFailure(t *testing.T) {
	src, _ := buildStore(t)
	archive := exportTo(t, src, "appa:1")
	corrupted := corruptLayerBytes(t, archive)

	dst, err := store.NewState(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Import(dst, bytes.NewReader(corrupted)); err == nil {
		t.Fatal("expected failure")
	}
	if manifests, _ := ListAll(dst.ImagesDir); len(manifests) != 0 {
		t.Errorf("a failed import wrote %d manifests", len(manifests))
	}
}

func TestImportRejectsTamperedManifest(t *testing.T) {
	src, _ := buildStore(t)
	archive := exportTo(t, src, "appa:1")

	// Rewrite the manifest's command without recomputing its digest, which is
	// exactly what an attacker editing an archive would do.
	tampered := rewriteArchiveFile(t, archive, "manifests/", func(b []byte) []byte {
		return bytes.Replace(b, []byte(`"/bin/sh"`), []byte(`"/evil"`), 1)
	})

	dst, err := store.NewState(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	_, err = Import(dst, bytes.NewReader(tampered))
	if err == nil {
		t.Fatal("importing a tampered manifest should fail")
	}
	if !strings.Contains(err.Error(), "corrupt") {
		t.Errorf("error should report corruption, got: %v", err)
	}
}

func TestImportRejectsNonArchive(t *testing.T) {
	dst, err := store.NewState(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var empty bytes.Buffer
	tw := tar.NewWriter(&empty)
	tw.WriteHeader(&tar.Header{Name: "random.txt", Size: 3, Typeflag: tar.TypeReg})
	tw.Write([]byte("hi\n"))
	tw.Close()

	if _, err := Import(dst, bytes.NewReader(empty.Bytes())); err == nil {
		t.Error("a tar without an index should be rejected")
	}
}

func TestImportRejectsMissingLayer(t *testing.T) {
	src, _ := buildStore(t)
	archive := exportTo(t, src, "appa:1")
	stripped := dropArchiveFile(t, archive, "layers/")

	dst, err := store.NewState(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Import(dst, bytes.NewReader(stripped)); err == nil {
		t.Error("an archive missing a layer it needs should be rejected")
	}
}

// ─── archive surgery helpers ────────────────────────────────────────────────

func rebuild(t *testing.T, in []byte, fn func(hdr *tar.Header, body []byte) (*tar.Header, []byte, bool)) []byte {
	t.Helper()
	var out bytes.Buffer
	tw := tar.NewWriter(&out)
	tr := tar.NewReader(bytes.NewReader(in))
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		newHdr, newBody, keep := fn(hdr, body)
		if !keep {
			continue
		}
		newHdr.Size = int64(len(newBody))
		if err := tw.WriteHeader(newHdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(newBody); err != nil {
			t.Fatal(err)
		}
	}
	tw.Close()
	return out.Bytes()
}

func corruptLayerBytes(t *testing.T, archive []byte) []byte {
	t.Helper()
	done := false
	return rebuild(t, archive, func(hdr *tar.Header, body []byte) (*tar.Header, []byte, bool) {
		if !done && strings.HasPrefix(hdr.Name, "layers/") && len(body) > 0 {
			done = true
			flipped := append([]byte(nil), body...)
			flipped[len(flipped)/2] ^= 0xff
			return hdr, flipped, true
		}
		return hdr, body, true
	})
}

func rewriteArchiveFile(t *testing.T, archive []byte, prefix string, fn func([]byte) []byte) []byte {
	t.Helper()
	return rebuild(t, archive, func(hdr *tar.Header, body []byte) (*tar.Header, []byte, bool) {
		if strings.HasPrefix(hdr.Name, prefix) {
			return hdr, fn(body), true
		}
		return hdr, body, true
	})
}

func dropArchiveFile(t *testing.T, archive []byte, prefix string) []byte {
	t.Helper()
	dropped := false
	return rebuild(t, archive, func(hdr *tar.Header, body []byte) (*tar.Header, []byte, bool) {
		if !dropped && strings.HasPrefix(hdr.Name, prefix) {
			dropped = true
			return hdr, body, false
		}
		return hdr, body, true
	})
}
