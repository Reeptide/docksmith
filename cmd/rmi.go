package cmd

import (
	"fmt"
	"os"
	"path/filepath"

	"docksmith/internal/image"
	"docksmith/internal/store"
)

func RunRmi(nameTag string) error {
	name, tag := image.ParseNameTag(nameTag)
	st, err := store.NewState(stateRoot())
	if err != nil {
		return err
	}
	m, err := image.Load(st.ImagesDir, name, tag)
	if err != nil {
		return fmt.Errorf("rmi: %w", err)
	}

	// Remove the manifest first, so the layer scan below sees the store as it
	// will be, not as it was.
	manifestPath := filepath.Join(st.ImagesDir, image.ManifestFileName(name, tag))
	if err := os.Remove(manifestPath); err != nil {
		return fmt.Errorf("rmi: removing manifest: %w", err)
	}

	// Only delete layers no surviving image still references.
	//
	// Layers are content-addressed, so every image built FROM a common base
	// shares that base's layer files. Deleting them unconditionally destroys
	// the base image and every sibling built on it — removing one derived image
	// silently breaks everything else in the store.
	inUse, err := referencedLayers(st)
	if err != nil {
		return fmt.Errorf("rmi: %w", err)
	}

	var freed, kept int
	for _, l := range m.Layers {
		if inUse[l.Digest] {
			kept++
			continue
		}
		if err := st.DeleteLayer(l.Digest); err != nil && !os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr, "warning: could not delete layer %s: %v\n", l.Digest, err)
			continue
		}
		freed++
	}

	fmt.Printf("Removed %s:%s\n", name, tag)
	if kept > 0 {
		fmt.Printf("  %d layer(s) deleted, %d still shared with other images\n", freed, kept)
	}
	return nil
}

// referencedLayers returns the set of layer digests any stored image still
// needs. Also used to decide what `prune` may reclaim.
func referencedLayers(st *store.State) (map[string]bool, error) {
	manifests, err := image.ListAll(st.ImagesDir)
	if err != nil {
		return nil, err
	}
	inUse := make(map[string]bool)
	for _, m := range manifests {
		for _, l := range m.Layers {
			inUse[l.Digest] = true
		}
	}
	return inUse, nil
}
