package cmd

import (
	"docksmith/internal/builder"
	"docksmith/internal/store"
	"fmt"
	"os"
)

func RunBuild(args []string) error {
	var tag string
	var noCache bool
	contextDir := "."

	// Parse flags manually.
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-t":
			if i+1 >= len(args) {
				return fmt.Errorf("-t requires an argument")
			}
			i++
			tag = args[i]
		case "--no-cache":
			noCache = true
		default:
			contextDir = args[i]
		}
	}

	if tag == "" {
		return fmt.Errorf("usage: docksmith build -t <name:tag> <context>")
	}

	stateDir := stateRoot()
	st, err := store.NewState(stateDir)
	if err != nil {
		return err
	}

	return builder.Build(builder.BuildOptions{
		ContextDir: contextDir,
		Tag:        tag,
		NoCache:    noCache,
		State:      st,
	})
}

func stateRoot() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "/tmp/.docksmith"
	}
	return home + "/.docksmith"
}
