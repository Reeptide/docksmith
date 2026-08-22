package cmd

import (
	"flag"
	"fmt"
	"os"
	"syscall"
	"time"

	"docksmith/internal/container"
)

func RunRm(args []string) error {
	var force bool
	fs := flag.NewFlagSet("rm", flag.ContinueOnError)
	fs.BoolVar(&force, "f", false, "kill and remove a running container")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: docksmith rm [-f] <container>...")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() == 0 {
		fs.Usage()
		return fmt.Errorf("rm: no container specified")
	}

	root := stateRoot()
	var firstErr error
	for _, ref := range fs.Args() {
		if err := removeOne(root, ref, force); err != nil {
			fmt.Fprintf(os.Stderr, "rm: %v\n", err)
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		fmt.Println(ref)
	}
	return firstErr
}

func removeOne(root, ref string, force bool) error {
	rec, err := container.Resolve(root, ref)
	if err != nil {
		return err
	}
	if rec.Reconcile() {
		container.Save(rec)
	}

	if rec.State == container.StateRunning {
		if !force {
			return fmt.Errorf("container %s is running (use -f to force)", container.ShortID(rec.ID))
		}
		if err := rec.Signal(syscall.SIGKILL); err != nil {
			return err
		}
		for i := 0; i < 50 && rec.IsAlive(); i++ {
			time.Sleep(100 * time.Millisecond)
		}
		// Removing the record anyway would be the worst outcome: the container
		// keeps running with nothing left that names its pid, its address or
		// its iptables rules, so it can never be stopped or cleaned up by any
		// docksmith command again. SIGKILL is unblockable, so surviving this
		// means uninterruptible sleep — a wedged NFS mount or a stuck device —
		// and the right answer is to say so and leave the record alone.
		if rec.IsAlive() {
			return fmt.Errorf("container %s did not die after SIGKILL; "+
				"it is likely blocked in the kernel. Refusing to remove the "+
				"record, which is all that still tracks it",
				container.ShortID(rec.ID))
		}
		rec.MarkExited(137) // 128 + SIGKILL
	}

	// Release the address lease and delete the iptables rules recorded for
	// this container before the record that names them is gone.
	teardownNetwork(root, rec)
	teardownCgroup(rec)

	return container.Remove(rec)
}
