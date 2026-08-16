package main

import (
	"docksmith/cmd"
	"docksmith/internal/runtime"
	"fmt"
	"os"
)

func main() {
	// Child re-exec entry point — MUST be first check.
	if len(os.Args) >= 2 && os.Args[1] == "__child__" {
		if runtime.ChildMain(os.Args[1:]) {
			return
		}
	}

	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	var err error
	switch os.Args[1] {
	case cmd.SupervisorSentinel:
		// Detached containers re-exec docksmith as their own supervisor, which
		// waits on the container and records its exit status. Not a user
		// command; see cmd/supervisor.go.
		err = cmd.RunSupervisor(os.Args[2:])
	case "build":
		err = cmd.RunBuild(os.Args[2:])
	case "images":
		err = cmd.RunImages()
	case "rmi":
		if len(os.Args) < 3 {
			fmt.Fprintln(os.Stderr, "usage: docksmith rmi <name:tag>")
			os.Exit(1)
		}
		err = cmd.RunRmi(os.Args[2])
	case "run":
		err = cmd.RunContainer(os.Args[2:])
	case "import":
		err = cmd.RunImport(os.Args[2:])
	case "save":
		err = cmd.RunSave(os.Args[2:])
	case "load":
		err = cmd.RunLoad(os.Args[2:])
	case "prune":
		err = cmd.RunPrune(os.Args[2:])
	case "ps":
		err = cmd.RunPs(os.Args[2:])
	case "logs":
		err = cmd.RunLogs(os.Args[2:])
	case "stop":
		err = cmd.RunStop(os.Args[2:])
	case "rm":
		err = cmd.RunRm(os.Args[2:])
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", os.Args[1])
		printUsage()
		os.Exit(1)
	}

	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Println(`Usage: docksmith <command> [options]

Commands:
  build -t <name:tag> [--no-cache] <context>   Build an image from a Docksmithfile
  images                                         List all images in local store
  rmi <name:tag>                                 Remove an image and its layers
  import <dir-or-tar> <name:tag>                 Import a base image into local store
  save [-o out.tar] <name:tag>...                Export images to a tar archive
  load [-i in.tar]                               Import images from a tar archive

Containers:
  run [opts] <name:tag> [cmd]                    Run a container
       -d                  run in the background, print the container id
       --rm                remove the container when it exits
       --name <name>       assign a name
       --net <mode>        bridge (default), none or host
       -e KEY=VALUE        set an environment variable (repeatable)
       -v host:ctr[:ro]    bind mount a host path (repeatable)
       -p host:ctr[/proto] publish a port (repeatable)
  ps [-a]                                        List containers
  logs [-f] <container>                          Show a container's output
  stop [-t secs] <container>...                  Stop a running container
  rm [-f] <container>...                         Remove a container

Maintenance:
  prune [-f] [--all]                             Reclaim disk from unused data`)
}
