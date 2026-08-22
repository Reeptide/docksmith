package cmd

import (
	"fmt"
	"os"

	"docksmith/internal/cgroup"
	"docksmith/internal/container"
)

// attachCgroup creates the container's cgroup (if resource limits were
// requested) and moves pid into it. Mirrors attachNetwork's shape: create,
// then attach, unwinding on failure.
func attachCgroup(rec *container.Record, pid int) error {
	if rec.Resources.Empty() {
		return nil
	}
	shortID := container.ShortID(rec.ID)
	if err := cgroup.EnsureParent(rec.Resources); err != nil {
		return err
	}
	if err := cgroup.Create(shortID, rec.Resources); err != nil {
		return err
	}
	if err := cgroup.Attach(shortID, pid); err != nil {
		cgroup.Remove(shortID)
		return err
	}
	return nil
}

// teardownCgroup removes the container's cgroup. Safe to call more than once,
// and for a container that never had one.
func teardownCgroup(rec *container.Record) {
	if rec.Resources.Empty() {
		return
	}
	if err := cgroup.Remove(container.ShortID(rec.ID)); err != nil {
		fmt.Fprintf(os.Stderr, "warning: removing cgroup: %v\n", err)
	}
}
