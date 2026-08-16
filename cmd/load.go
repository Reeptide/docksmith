package cmd

import (
	"bufio"
	"flag"
	"fmt"
	"os"

	"docksmith/internal/image"
	"docksmith/internal/store"
)

func RunLoad(args []string) error {
	var in string
	fs := flag.NewFlagSet("load", flag.ContinueOnError)
	fs.StringVar(&in, "i", "", "read from a file instead of stdin")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: docksmith load [-i in.tar]")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}

	st, err := store.NewState(stateRoot())
	if err != nil {
		return err
	}

	var r *os.File
	if in == "" {
		if isTerminal(os.Stdin) {
			return fmt.Errorf("load: no input; use -i or pipe an archive to stdin")
		}
		r = os.Stdin
	} else {
		f, err := os.Open(in)
		if err != nil {
			return fmt.Errorf("load: %w", err)
		}
		defer f.Close()
		r = f
	}

	results, err := image.Import(st, bufio.NewReaderSize(r, 1<<20))
	if err != nil {
		return fmt.Errorf("load: %w", err)
	}

	for _, res := range results {
		if res.Skipped {
			fmt.Printf("Already present: %s\n", res.Ref)
			continue
		}
		fmt.Printf("Loaded: %s\n", res.Ref)
	}
	if len(results) == 0 {
		fmt.Println("Archive contained no images")
	}
	return nil
}
