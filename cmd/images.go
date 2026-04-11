package cmd

import (
	"docksmith/internal/image"
	"docksmith/internal/store"
	"fmt"
)

func RunImages() error {
	st, err := store.NewState(stateRoot())
	if err != nil {
		return err
	}
	manifests, err := image.ListAll(st.ImagesDir)
	if err != nil {
		return err
	}
	fmt.Printf("%-20s %-10s %-14s %-25s\n", "NAME", "TAG", "ID", "CREATED")
	for _, m := range manifests {
		id := m.Digest
		if len(id) > 19 {
			id = id[7:19] // skip "sha256:" and take 12 chars
		}
		fmt.Printf("%-20s %-10s %-14s %-25s\n", m.Name, m.Tag, id, m.Created)
	}
	return nil
}
