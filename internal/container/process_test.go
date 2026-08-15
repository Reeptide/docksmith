package container

import (
	"os"
	"os/exec"
	"testing"
)

func TestReadStartTimeForSelf(t *testing.T) {
	got, err := ReadStartTime(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if got == 0 {
		t.Error("start time should be non-zero")
	}
	again, err := ReadStartTime(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if got != again {
		t.Errorf("start time is not stable: %d then %d", got, again)
	}
}

// /proc/<pid>/stat's second field is the executable name in parentheses and
// may itself contain spaces and parentheses, which naive field splitting gets
// wrong — every field after it would be off by one.
func TestReadStartTimeHandlesAwkwardProcessNames(t *testing.T) {
	dir := t.TempDir()
	weird := dir + "/we ird (name)"
	if err := os.WriteFile(weird, []byte("#!/bin/sh\nsleep 5\n"), 0755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(weird)
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot start helper: %v", err)
	}
	defer func() {
		cmd.Process.Kill()
		cmd.Wait()
	}()

	got, err := ReadStartTime(cmd.Process.Pid)
	if err != nil {
		t.Fatalf("ReadStartTime failed for a process with a parenthesised name: %v", err)
	}
	if got == 0 {
		t.Error("start time should be non-zero")
	}
}

func TestReadStartTimeMissingProcess(t *testing.T) {
	// Pid 0 never has a /proc entry.
	if _, err := ReadStartTime(0); err == nil {
		t.Error("expected an error for a non-existent process")
	}
}

func TestIsAliveTrueForRunningProcess(t *testing.T) {
	st, err := ReadStartTime(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	r := &Record{Pid: os.Getpid(), StartTime: st}
	if !r.IsAlive() {
		t.Error("IsAlive should be true for this process")
	}
}

func TestIsAliveFalseWithoutPid(t *testing.T) {
	if (&Record{}).IsAlive() {
		t.Error("a record with no pid cannot be alive")
	}
}

// The reason start time is recorded at all: a stale record whose pid has been
// recycled must not be treated as live, or `stop` would signal an unrelated
// process as root.
func TestIsAliveDetectsRecycledPid(t *testing.T) {
	st, err := ReadStartTime(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	r := &Record{Pid: os.Getpid(), StartTime: st + 12345} // as if recorded earlier
	if r.IsAlive() {
		t.Error("a pid whose start time differs must not be considered the recorded process")
	}
}

func TestReconcileCorrectsDeadRunningRecord(t *testing.T) {
	// A pid that is certainly gone: start a process and wait for it.
	cmd := exec.Command("/bin/true")
	if err := cmd.Run(); err != nil {
		t.Skipf("cannot run helper: %v", err)
	}
	dead := cmd.Process.Pid

	r := &Record{State: StateRunning, Pid: dead, StartTime: 999999999}
	if !r.Reconcile() {
		t.Fatal("Reconcile should report a change for a dead running record")
	}
	if r.State != StateExited {
		t.Errorf("state = %s, want exited", r.State)
	}
	if r.Pid != 0 {
		t.Errorf("pid should be cleared, got %d", r.Pid)
	}
	if r.ExitCode != 255 {
		t.Errorf("exit code = %d, want 255 for an unobserved exit", r.ExitCode)
	}
	if r.FinishedAt == "" {
		t.Error("FinishedAt should be set")
	}
}

func TestReconcileLeavesLiveRecordAlone(t *testing.T) {
	st, err := ReadStartTime(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	r := &Record{State: StateRunning, Pid: os.Getpid(), StartTime: st}
	if r.Reconcile() {
		t.Error("Reconcile should not change a genuinely running container")
	}
	if r.State != StateRunning {
		t.Errorf("state = %s, want running", r.State)
	}
}

func TestReconcileIgnoresNonRunningRecords(t *testing.T) {
	r := &Record{State: StateExited, ExitCode: 3}
	if r.Reconcile() {
		t.Error("Reconcile should not touch an already-exited record")
	}
	if r.ExitCode != 3 {
		t.Errorf("exit code was overwritten: %d", r.ExitCode)
	}
}

func TestMarkStartedAndExited(t *testing.T) {
	r := &Record{}
	r.MarkStarted(os.Getpid())
	if r.State != StateRunning || r.Pid != os.Getpid() {
		t.Errorf("MarkStarted: %+v", r)
	}
	if r.StartTime == 0 {
		t.Error("MarkStarted should record the process start time")
	}
	if r.StartedAt == "" {
		t.Error("MarkStarted should record a timestamp")
	}

	r.MarkExited(42)
	if r.State != StateExited || r.ExitCode != 42 {
		t.Errorf("MarkExited: %+v", r)
	}
	if r.Pid != 0 || r.StartTime != 0 {
		t.Error("MarkExited should clear the process handle")
	}
}

func TestSignalRefusesDeadContainer(t *testing.T) {
	r := &Record{ID: "abc", Pid: 0}
	if err := r.Signal(9); err == nil {
		t.Error("signalling a container with no live process should fail")
	}
}
