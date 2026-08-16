package cmd

import (
	"flag"
	"fmt"
	"os"
	"text/tabwriter"

	"docksmith/internal/container"
)

func RunPs(args []string) error {
	var all bool
	fs := flag.NewFlagSet("ps", flag.ContinueOnError)
	fs.BoolVar(&all, "a", false, "show all containers, not just running ones")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: docksmith ps [-a]")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}

	root := stateRoot()
	records, err := container.List(root)
	if err != nil {
		return err
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
	fmt.Fprintln(w, "CONTAINER ID\tIMAGE\tCOMMAND\tCREATED\tSTATUS\tPORTS\tNAME")

	for _, r := range records {
		// A record can claim to be running while its process is long gone —
		// a SIGKILLed supervisor leaves exactly that. Correct it here so the
		// listing cannot show a permanent lie, and persist the correction.
		if r.Reconcile() {
			if err := container.Save(r); err != nil {
				fmt.Fprintf(os.Stderr, "warning: container %s: %v\n", container.ShortID(r.ID), err)
			}
		}
		if !all && r.State != container.StateRunning {
			continue
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			container.ShortID(r.ID), r.Image, r.CommandString(),
			r.CreatedAgo(), r.Status(), portsColumn(r.Ports), r.Name)
	}
	return w.Flush()
}
