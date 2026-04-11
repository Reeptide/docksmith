package cmd

import (
	"docksmith/internal/builder"
	"docksmith/internal/image"
	"docksmith/internal/runtime"
	"docksmith/internal/store"
	"fmt"
	"os"
	"strings"
)

func RunContainer(args []string) error {
	var envOverrides = make(map[string]string)
	var nameTag string
	var cmdOverride []string

	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "-e":
			if i+1 >= len(args) {
				return fmt.Errorf("-e requires KEY=VALUE")
			}
			i++
			parts := strings.SplitN(args[i], "=", 2)
			if len(parts) != 2 {
				return fmt.Errorf("-e: invalid format %q, expected KEY=VALUE", args[i])
			}
			envOverrides[parts[0]] = parts[1]
		case strings.HasPrefix(args[i], "-e="):
			kv := args[i][3:]
			parts := strings.SplitN(kv, "=", 2)
			if len(parts) != 2 {
				return fmt.Errorf("-e: invalid format %q", kv)
			}
			envOverrides[parts[0]] = parts[1]
		case nameTag == "":
			nameTag = args[i]
		default:
			cmdOverride = append(cmdOverride, args[i])
		}
	}

	if nameTag == "" {
		return fmt.Errorf("usage: docksmith run [-e KEY=VALUE] <name:tag> [cmd]")
	}

	name, tag := image.ParseNameTag(nameTag)
	st, err := store.NewState(stateRoot())
	if err != nil {
		return err
	}
	m, err := image.Load(st.ImagesDir, name, tag)
	if err != nil {
		return fmt.Errorf("run: %w", err)
	}

	// Determine command.
	finalCmd := m.Config.Cmd
	if len(cmdOverride) > 0 {
		finalCmd = cmdOverride
	}
	if len(finalCmd) == 0 {
		return fmt.Errorf("run: no CMD defined in image and no command given")
	}

	// Assemble rootfs.
	rootfs, err := builder.AssembleRootFS(m, st)
	if err != nil {
		return fmt.Errorf("run: assembling rootfs: %w", err)
	}
	defer os.RemoveAll(rootfs)

	// Build env map from image.
	envMap := make(map[string]string)
	for _, e := range m.Config.Env {
		parts := strings.SplitN(e, "=", 2)
		if len(parts) == 2 {
			envMap[parts[0]] = parts[1]
		}
	}

	exitCode, err := runtime.IsolatedRun(runtime.RunOptions{
		RootFS:       rootfs,
		Command:      finalCmd,
		WorkingDir:   m.Config.WorkingDir,
		Env:          envMap,
		EnvOverrides: envOverrides,
	})
	if err != nil {
		return fmt.Errorf("run: %w", err)
	}
	if exitCode != 0 {
		fmt.Fprintf(os.Stderr, "Container exited with code %d\n", exitCode)
		os.Exit(exitCode)
	}
	return nil
}
