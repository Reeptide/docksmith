package runtime

import (
	"fmt"
	"os"
	"os/signal"
	"syscall"
)

// forwardedSignals are relayed from container PID 1 to the real command.
//
// Deliberately an explicit list rather than signal.Notify(ch) with no filter:
// catching everything would also catch SIGURG, which the Go runtime uses for
// goroutine preemption many times a second, and SIGCHLD, which is handled by
// the reaper loop below. SIGKILL and SIGSTOP are absent because they cannot be
// caught by anyone.
var forwardedSignals = []os.Signal{
	syscall.SIGTERM, syscall.SIGINT, syscall.SIGQUIT, syscall.SIGHUP,
	syscall.SIGUSR1, syscall.SIGUSR2, syscall.SIGWINCH, syscall.SIGPIPE,
	syscall.SIGALRM, syscall.SIGTSTP, syscall.SIGCONT,
}

// runInit runs cmd as a child of this process and acts as the container's
// init, returning the command's exit status.
//
// Being PID 1 of a PID namespace is a special job, and skipping it breaks two
// things that look unrelated:
//
//   - Signals. Per pid_namespaces(7), a namespace's init receives only signals
//     it has installed a handler for — and that restriction applies to signals
//     sent from an ancestor namespace too, not just from inside. So `docksmith
//     stop` sending SIGTERM to a shell running as PID 1 is silently discarded,
//     and stop degrades to a SIGKILL after the full timeout, every time.
//   - Zombies. Orphaned processes are reparented to PID 1, and if PID 1 never
//     reaps them they accumulate as zombies for the life of the container.
//
// Running the command as PID 2 with this loop as PID 1 fixes both.
func runInit(bin string, argv []string, env []string, dir string) (int, error) {
	sigCh := make(chan os.Signal, 64)
	signal.Notify(sigCh, forwardedSignals...)
	defer signal.Stop(sigCh)

	pid, err := syscall.ForkExec(bin, argv, &syscall.ProcAttr{
		Dir:   dir,
		Env:   env,
		Files: []uintptr{0, 1, 2},
		Sys: &syscall.SysProcAttr{
			// Own process group, so signals can be delivered to the command and
			// everything it spawns with a single kill(-pgid).
			Setpgid: true,
		},
	})
	if err != nil {
		return 1, fmt.Errorf("starting %s: %w", bin, err)
	}

	go func() {
		for sig := range sigCh {
			s, ok := sig.(syscall.Signal)
			if !ok {
				continue
			}
			// Negative pid targets the process group. Errors are ignored: the
			// command may have already exited, which is a race we cannot avoid
			// and do not need to report.
			_ = syscall.Kill(-pid, s)
		}
	}()

	// Reap everything, not just the command: orphans reparent to PID 1 and
	// would otherwise pile up as zombies. Only the command's own status is
	// returned.
	for {
		var ws syscall.WaitStatus
		wpid, err := syscall.Wait4(-1, &ws, 0, nil)
		switch err {
		case nil:
		case syscall.EINTR:
			continue
		case syscall.ECHILD:
			// Nothing left to wait for and the command was never seen exiting.
			return 1, fmt.Errorf("container process vanished without reporting a status")
		default:
			return 1, fmt.Errorf("wait4: %w", err)
		}

		if wpid != pid {
			continue // reaped an orphan
		}
		switch {
		case ws.Exited():
			return ws.ExitStatus(), nil
		case ws.Signaled():
			// Same convention as a shell: 128 + signal number.
			return 128 + int(ws.Signal()), nil
		default:
			// Stopped or continued — not an exit, keep waiting.
			continue
		}
	}
}
