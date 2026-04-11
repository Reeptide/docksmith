package cmd

import (
	"docksmith/internal/image"
	"docksmith/internal/store"
	"fmt"
	"os"
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
	for _, l := range m.Layers {
		if err := st.DeleteLayer(l.Digest); err != nil {
			fmt.Fprintf(os.Stderr, "warning: could not delete layer %s: %v\n", l.Digest, err)
		}
	}
	manifestPath := st.ImagesDir + "/" + image.ManifestFileName(name, tag)
	if err := os.Remove(manifestPath); err != nil {
		return fmt.Errorf("rmi: removing manifest: %w", err)
	}
	fmt.Printf("Removed %s:%s\n", name, tag)
	return nil
}
