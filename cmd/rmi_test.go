package cmd

import (
	"testing"

	"docksmith/internal/image"
	"docksmith/internal/store"
)

// The bug this guards against: rmi used to delete every layer its manifest
// named, with no regard for other images. Since every image built FROM a
// common base shares that base's layer files, removing one derived image
// destroyed the base image and every sibling built on it.
func TestReferencedLayersSpansAllImages(t *testing.T) {
	st, err := store.NewState(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	base := image.LayerEntry{Digest: "sha256:base", Size: 1, CreatedBy: "base"}
	appA := image.LayerEntry{Digest: "sha256:a", Size: 1, CreatedBy: "COPY a"}
	appB := image.LayerEntry{Digest: "sha256:b", Size: 1, CreatedBy: "COPY b"}

	for _, m := range []*image.Manifest{
		{Name: "busybox", Tag: "latest", Layers: []image.LayerEntry{base}},
		{Name: "appa", Tag: "1", Layers: []image.LayerEntry{base, appA}},
		{Name: "appb", Tag: "1", Layers: []image.LayerEntry{base, appB}},
	} {
		if err := image.Save(m, st.ImagesDir); err != nil {
			t.Fatal(err)
		}
	}

	inUse, err := referencedLayers(st)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{"sha256:base", "sha256:a", "sha256:b"} {
		if !inUse[d] {
			t.Errorf("%s should be reported as in use", d)
		}
	}
}

func TestReferencedLayersIgnoresRemovedImages(t *testing.T) {
	st, err := store.NewState(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	base := image.LayerEntry{Digest: "sha256:base", Size: 1}
	only := image.LayerEntry{Digest: "sha256:only", Size: 1}

	if err := image.Save(&image.Manifest{
		Name: "keep", Tag: "1", Layers: []image.LayerEntry{base},
	}, st.ImagesDir); err != nil {
		t.Fatal(err)
	}

	inUse, err := referencedLayers(st)
	if err != nil {
		t.Fatal(err)
	}
	if !inUse[base.Digest] {
		t.Error("a layer belonging to a surviving image must be in use")
	}
	if inUse[only.Digest] {
		t.Error("a layer no image references must not be in use")
	}
}

func TestReferencedLayersEmptyStore(t *testing.T) {
	st, err := store.NewState(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	inUse, err := referencedLayers(st)
	if err != nil {
		t.Fatal(err)
	}
	if len(inUse) != 0 {
		t.Errorf("empty store reported %d layers in use", len(inUse))
	}
}
