package cmd

import (
	"flag"
	"fmt"
	"os"
	"syscall"
	"time"

	"docksmith/internal/container"
)

func RunStop(args []string) error {
	var timeout int
	fs := flag.NewFlagSet("stop", flag.ContinueOnError)
	fs.IntVar(&timeout, "t", 10, "seconds to wait before sending SIGKILL")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: docksmith stop [-t seconds] <container>...")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() == 0 {
		fs.Usage()
		return fmt.Errorf("stop: no container specified")
	}

	root := stateRoot()
	var firstErr error
	for _, ref := range fs.Args() {
		if err := stopOne(root, ref, timeout); err != nil {
			fmt.Fprintf(os.Stderr, "stop: %v\n", err)
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		fmt.Println(ref)
	}
	return firstErr
}

func stopOne(root, ref string, timeout int) error {
	rec, err := container.Resolve(root, ref)
	if err != nil {
		return err
	}
	if rec.Reconcile() {
		container.Save(rec)
	}
	if rec.State != container.StateRunning {
		return fmt.Errorf("container %s is not running", container.ShortID(rec.ID))
	}

	// The recorded pid is the container's init, which is PID 1 inside its own
	// PID namespace. Killing it tears down every process in that namespace, so
	// there is nothing else to clean up. It also only receives SIGTERM at all
	// because that init installs a handler — see internal/runtime/init.go.
	if err := rec.Signal(syscall.SIGTERM); err != nil {
		return err
	}

	deadline := time.Now().Add(time.Duration(timeout) * time.Second)
	for time.Now().Before(deadline) {
		if !rec.IsAlive() {
			finishStop(root, rec)
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}

	if err := rec.Signal(syscall.SIGKILL); err != nil {
		finishStop(root, rec) // it exited between the check and the signal
		return nil
	}
	// SIGKILL cannot be caught, so this is a bounded wait.
	for i := 0; i < 50; i++ {
		if !rec.IsAlive() {
			finishStop(root, rec)
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("container %s did not stop", container.ShortID(rec.ID))
}

// finishStop releases network resources and settles the record.
//
// The record must be re-read rather than written from the copy loaded before
// the signal: a detached container's supervisor observes the exit and records
// the real status, and saving the stale in-memory copy would overwrite it with
// state=running and exitCode=0, destroying the exit code on every stop.
func finishStop(root string, rec *container.Record) {
	teardownNetwork(root, rec)
	teardownCgroup(rec)

	fresh, err := container.Load(root, rec.ID)
	if err != nil {
		return
	}
	// Carry over the network teardown, then let whatever the supervisor
	// recorded stand. Only reconcile if nothing has settled it yet.
	fresh.Rules = nil
	fresh.IP = ""
	if fresh.State == container.StateRunning {
		fresh.Reconcile()
	}
	container.Save(fresh)
}
