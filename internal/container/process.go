package container

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// ReadStartTime returns field 22 of /proc/<pid>/stat, the process start time in
// clock ticks since boot.
//
// Pids alone are not stable handles: Linux recycles them, and pid_max wraps in
// hours on a busy machine. Pairing the pid with its start time makes a stale
// record impossible to confuse with whatever process later inherited the
// number — which matters because the alternative is `docksmith stop` sending
// SIGTERM to an unrelated process as root.
func ReadStartTime(pid int) (uint64, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0, err
	}

	// Field 2 is the executable name in parentheses and may itself contain
	// spaces or parentheses, so parse from the last ')' rather than splitting
	// the whole line.
	end := strings.LastIndex(string(data), ")")
	if end < 0 {
		return 0, fmt.Errorf("malformed /proc/%d/stat", pid)
	}
	fields := strings.Fields(string(data)[end+1:])
	// After the name, field 3 is at index 0, so field 22 is at index 19.
	const startTimeOffset = 19
	if len(fields) <= startTimeOffset {
		return 0, fmt.Errorf("malformed /proc/%d/stat: %d fields", pid, len(fields))
	}
	return strconv.ParseUint(fields[startTimeOffset], 10, 64)
}

// IsAlive reports whether the recorded process is still running, comparing
// both pid and start time so a recycled pid is not mistaken for the original.
func (r *Record) IsAlive() bool {
	if r.Pid <= 0 {
		return false
	}
	start, err := ReadStartTime(r.Pid)
	if err != nil {
		return false // process is gone
	}
	// A record with no recorded start time cannot be proved to still own its
	// pid, so it is reported dead rather than falling back to a bare pid check.
	//
	// The fallback was the reuse-unsafe behaviour this field exists to prevent.
	// Being wrong in this direction costs a container reported as Exited while
	// it is really running, which prune and rm handle. Being wrong in the other
	// direction means `stop` sends SIGTERM, and then SIGKILL, to whatever
	// unrelated process now holds that pid — as root.
	if r.StartTime == 0 || start != r.StartTime {
		return false
	}
	return true
}

// Reconcile corrects a record whose process died without anything updating it,
// which happens when a supervisor is killed outright. Returns true if the
// record changed and needs saving.
func (r *Record) Reconcile() bool {
	if r.State != StateRunning {
		return false
	}
	if r.IsAlive() {
		return false
	}
	r.State = StateExited
	r.Pid = 0
	r.StartTime = 0
	if r.FinishedAt == "" {
		r.FinishedAt = time.Now().UTC().Format(time.RFC3339)
	}
	// The exit status is unknowable here: whoever would have recorded it is
	// gone. 255 is the conventional stand-in for "died without reporting".
	r.ExitCode = 255
	return true
}

// Signal sends a signal to the container's process, refusing to act on a
// record whose process is no longer the one that was recorded.
func (r *Record) Signal(sig syscall.Signal) error {
	if !r.IsAlive() {
		return fmt.Errorf("container %s is not running", ShortID(r.ID))
	}
	return syscall.Kill(r.Pid, sig)
}

// MarkStarted records a live process against the container.
func (r *Record) MarkStarted(pid int) {
	r.Pid = pid
	r.State = StateRunning
	r.StartedAt = time.Now().UTC().Format(time.RFC3339)
	st, err := ReadStartTime(pid)
	if err != nil {
		// IsAlive now treats a zero start time as "cannot verify, assume dead",
		// so a silent failure here would make a running container invisible to
		// ps and unstoppable. It should not happen — the caller has just cloned
		// this pid — but if it does, say so rather than leaving the record in a
		// state nothing can explain.
		fmt.Fprintf(os.Stderr,
			"docksmith: warning: cannot read start time for pid %d (%v); "+
				"this container will be reported as exited\n", pid, err)
	}
	r.StartTime = st
}

// MarkExited records a finished process and its status.
func (r *Record) MarkExited(code int) {
	r.State = StateExited
	r.ExitCode = code
	r.Pid = 0
	r.StartTime = 0
	r.FinishedAt = time.Now().UTC().Format(time.RFC3339)
}
