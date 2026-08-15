package cmd

import (
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"docksmith/internal/container"
)

func RunLogs(args []string) error {
	var follow bool
	fs := flag.NewFlagSet("logs", flag.ContinueOnError)
	fs.BoolVar(&follow, "f", false, "follow log output")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: docksmith logs [-f] <container>")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() == 0 {
		fs.Usage()
		return fmt.Errorf("logs: no container specified")
	}

	root := stateRoot()
	rec, err := container.Resolve(root, fs.Arg(0))
	if err != nil {
		return fmt.Errorf("logs: %w", err)
	}

	f, err := os.Open(rec.LogPath())
	if err != nil {
		if os.IsNotExist(err) {
			return nil // started but produced no output yet
		}
		return fmt.Errorf("logs: %w", err)
	}
	defer f.Close()

	if _, err := io.Copy(os.Stdout, f); err != nil {
		return err
	}
	if !follow {
		return nil
	}
	return followLog(f, rec)
}

// followLog tails the log until the container exits or the user interrupts.
// The log is an ordinary file being appended to, so this polls for growth
// rather than watching for events.
func followLog(f *os.File, rec *container.Record) error {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	root := stateRoot()
	for {
		select {
		case <-sigCh:
			return nil
		default:
		}

		n, err := io.Copy(os.Stdout, f)
		if err != nil {
			return err
		}
		if n > 0 {
			continue // there may be more already waiting
		}

		// Nothing new. Stop once the container is finished, but re-read the
		// record first: it may have exited between the last copy and now.
		if fresh, err := container.Load(root, rec.ID); err == nil {
			fresh.Reconcile()
			if fresh.State != container.StateRunning {
				// One final read, to catch anything written as it exited.
				io.Copy(os.Stdout, f)
				return nil
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
}
