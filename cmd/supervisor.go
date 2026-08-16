package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"

	"docksmith/internal/container"
	"docksmith/internal/runtime"
)

// SupervisorSentinel is the argv[1] marker identifying a supervisor re-exec.
const SupervisorSentinel = "__supervisor__"

// startDetached launches a supervisor process for the container and returns
// immediately, printing the container id.
//
// The supervisor exists because there is no daemon. Simply starting the
// container and exiting would orphan it: nothing would be left to wait on the
// process, so its exit status would be lost and `ps` could only ever guess.
// Instead docksmith re-execs itself detached; that copy blocks in
// IsolatedRun and writes the final state back to config.json.
func startDetached(root string, rec *container.Record) error {
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("run: %w", err)
	}

	logFile, err := os.OpenFile(rec.LogPath(), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		return fmt.Errorf("run: opening log: %w", err)
	}
	defer logFile.Close()

	cmd := exec.Command(self, SupervisorSentinel, rec.ID)
	cmd.Env = append(os.Environ(), "DOCKSMITH_ROOT="+root)
	cmd.Stdin = nil
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{
		// New session: detach from this terminal so the container is not killed
		// by a SIGHUP when the invoking shell goes away.
		Setsid: true,
	}

	if err := cmd.Start(); err != nil {
		container.Remove(rec)
		return fmt.Errorf("run: starting supervisor: %w", err)
	}
	// Release the supervisor rather than waiting for it; it outlives this
	// process by design.
	if err := cmd.Process.Release(); err != nil {
		fmt.Fprintf(os.Stderr, "warning: releasing supervisor: %v\n", err)
	}

	fmt.Println(rec.ID)
	return nil
}

// RunSupervisor is the entry point for a detached container's supervisor.
// It runs the container to completion and records the result.
func RunSupervisor(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("supervisor: no container id")
	}
	root := stateRoot()
	rec, err := container.Load(root, args[0])
	if err != nil {
		return fmt.Errorf("supervisor: %w", err)
	}

	exitCode, runErr := runtime.IsolatedRun(runtime.RunOptions{
		RootFS:     rec.RootFSPath(),
		Command:    rec.Command,
		WorkingDir: rec.WorkingDir,
		Env:        rec.Env,
		Hostname:   container.ShortID(rec.ID),
		Mounts:     rec.Mounts,
		Network:    rec.Network,
		Stdout:     os.Stdout, // already pointed at the container log
		Stderr:     os.Stderr,
		OnStart: func(pid int) error {
			if err := attachNetwork(root, rec, pid); err != nil {
				return err
			}
			rec.MarkStarted(pid)
			return container.Save(rec)
		},
	})

	teardownNetwork(root, rec)

	if runErr != nil {
		fmt.Fprintf(os.Stderr, "docksmith: %v\n", runErr)
		rec.MarkExited(255)
		container.Save(rec)
		return nil
	}

	rec.MarkExited(exitCode)
	if err := container.Save(rec); err != nil {
		fmt.Fprintf(os.Stderr, "docksmith: recording exit status: %v\n", err)
	}
	if rec.AutoRm {
		container.Remove(rec)
	}
	return nil
}
