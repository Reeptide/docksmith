package cmd

import (
	"bufio"
	"flag"
	"fmt"
	"os"

	"docksmith/internal/image"
	"docksmith/internal/store"
)

func RunSave(args []string) error {
	var out string
	fs := flag.NewFlagSet("save", flag.ContinueOnError)
	fs.StringVar(&out, "o", "", "write to a file instead of stdout")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: docksmith save [-o out.tar] <name:tag>...")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() == 0 {
		fs.Usage()
		return fmt.Errorf("save: no images specified")
	}

	st, err := store.NewState(stateRoot())
	if err != nil {
		return err
	}

	var w *os.File
	if out == "" {
		// Refusing to write a tar to a terminal saves someone a mangled
		// scrollback and a confusing bug report.
		if isTerminal(os.Stdout) {
			return fmt.Errorf("save: refusing to write an archive to the terminal; use -o or redirect stdout")
		}
		w = os.Stdout
	} else {
		f, err := os.Create(out)
		if err != nil {
			return fmt.Errorf("save: %w", err)
		}
		defer f.Close()
		w = f
	}

	buf := bufio.NewWriterSize(w, 1<<20)
	if err := image.Export(st, fs.Args(), buf); err != nil {
		return fmt.Errorf("save: %w", err)
	}
	if err := buf.Flush(); err != nil {
		return fmt.Errorf("save: %w", err)
	}

	if out != "" {
		info, err := os.Stat(out)
		if err == nil {
			fmt.Fprintf(os.Stderr, "Saved %d image(s) to %s (%s)\n",
				fs.NArg(), out, humanBytes(info.Size()))
		}
	}
	return nil
}

// isTerminal reports whether f is a character device, which is how a terminal
// presents itself.
func isTerminal(f *os.File) bool {
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGT"[exp])
}
