package cgroup

import (
	"testing"

	"docksmith/internal/runtime"
)

func TestDir(t *testing.T) {
	got := Dir("abc123")
	want := "/sys/fs/cgroup/docksmith/abc123"
	if got != want {
		t.Errorf("Dir(%q) = %q, want %q", "abc123", got, want)
	}
}

// neededControllers must request only the controllers a limit actually uses:
// a pids-only limit must not need cpu or memory delegation to succeed.
func TestNeededControllers(t *testing.T) {
	cases := []struct {
		limits *runtime.ResourceLimits
		want   string
	}{
		{&runtime.ResourceLimits{PidsLimit: 10}, "+pids"},
		{&runtime.ResourceLimits{MemoryBytes: 1}, "+memory"},
		{&runtime.ResourceLimits{CPUQuota: 0.5}, "+cpu"},
		{&runtime.ResourceLimits{MemoryBytes: 1, CPUQuota: 0.5, PidsLimit: 10}, "+memory +cpu +pids"},
	}
	for _, c := range cases {
		if got := neededControllers(c.limits); got != c.want {
			t.Errorf("neededControllers(%+v) = %q, want %q", c.limits, got, c.want)
		}
	}
}
