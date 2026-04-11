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
  run [-e KEY=VALUE] <name:tag> [cmd]            Run a container
  import <dir-or-tar> <name:tag>                 Import a base image into local store`)
}
