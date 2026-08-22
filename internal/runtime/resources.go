package runtime

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// ResourceLimits describes cgroup v2 caps for a container. A nil pointer or a
// zero-value struct means "no limits" — no cgroup is ever created for such a
// container, so hosts without cgroup v2 (or without permission to use it) are
// unaffected unless the user actually asks for a limit.
type ResourceLimits struct {
	MemoryBytes int64   `json:"memoryBytes,omitempty"` // memory.max
	CPUQuota    float64 `json:"cpuQuota,omitempty"`    // fractional CPU count; cpu.max
	PidsLimit   int64   `json:"pidsLimit,omitempty"`   // pids.max
}

// Empty reports whether no limit was requested.
func (r *ResourceLimits) Empty() bool {
	return r == nil || (r.MemoryBytes == 0 && r.CPUQuota == 0 && r.PidsLimit == 0)
}

// cpuPeriod is cgroup v2's default cpu.max period, in microseconds. Fixing it
// means a CPU count converts to a quota by simple multiplication.
const cpuPeriod = 100000

// ParseMemorySize parses a memory limit such as "512m", "1g", "2048k", or a
// bare byte count. Suffixes are case-insensitive.
func ParseMemorySize(s string) (int64, error) {
	if s == "" {
		return 0, fmt.Errorf("invalid memory size %q: empty", s)
	}
	mult := int64(1)
	numPart := s
	switch s[len(s)-1] {
	case 'k', 'K':
		mult = 1024
		numPart = s[:len(s)-1]
	case 'm', 'M':
		mult = 1024 * 1024
		numPart = s[:len(s)-1]
	case 'g', 'G':
		mult = 1024 * 1024 * 1024
		numPart = s[:len(s)-1]
	}
	n, err := strconv.ParseInt(numPart, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid memory size %q: expected a number optionally followed by k, m or g", s)
	}
	if n <= 0 {
		return 0, fmt.Errorf("invalid memory size %q: must be positive", s)
	}
	if n > math.MaxInt64/mult {
		return 0, fmt.Errorf("invalid memory size %q: too large", s)
	}
	return n * mult, nil
}

// maxCPUs bounds --cpus so cpus*cpuPeriod cannot overflow int64 in
// CPUMaxLine; it is already far beyond any real core count.
const maxCPUs = float64(math.MaxInt64) / cpuPeriod

// ParseCPUs parses a fractional CPU count such as "1.5" or "0.5".
func ParseCPUs(s string) (float64, error) {
	n, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return 0, fmt.Errorf("invalid cpu count %q: expected a number", s)
	}
	if math.IsNaN(n) || math.IsInf(n, 0) {
		return 0, fmt.Errorf("invalid cpu count %q: must be finite", s)
	}
	if n <= 0 {
		return 0, fmt.Errorf("invalid cpu count %q: must be positive", s)
	}
	if n > maxCPUs {
		return 0, fmt.Errorf("invalid cpu count %q: too large", s)
	}
	return n, nil
}

// MinPidsLimit is the smallest --pids-limit docksmith accepts.
//
// Below it the value is not simply "no room for your command": docksmith's
// own in-process init (ChildMain's runInit, which forks the real command as
// PID 2 and stays as PID 1 to reap it and handle signals) is a Go program
// that needs several OS threads just to exist — GC, sysmon, and the thread
// os/signal.Notify starts for signal delivery — before the user's command
// ever runs. cgroup v2's pids controller counts every kernel task in the
// cgroup, init's threads included, so a limit under this floor was observed
// crashing init itself with a raw "runtime: failed to create new OS thread"
// panic instead of cleanly capping the container. The floor is set with
// headroom above the ~6-8 threads observed at failure.
const MinPidsLimit = 16

// ParsePidsLimit parses a --pids-limit value.
func ParsePidsLimit(s string) (int64, error) {
	n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid pids limit %q: expected a number", s)
	}
	if n <= 0 {
		return 0, fmt.Errorf("invalid pids limit %q: must be positive", s)
	}
	if n < MinPidsLimit {
		return 0, fmt.Errorf("invalid pids limit %q: must be at least %d (docksmith's own container init needs "+
			"several processes/threads to run; a lower limit crashes the container instead of limiting it)",
			s, MinPidsLimit)
	}
	return n, nil
}

// CPUMaxLine formats cpu.max's "<quota> <period>" content for a given
// fractional CPU count. cpus <= 0 means no limit.
func CPUMaxLine(cpus float64) string {
	if cpus <= 0 {
		return "max"
	}
	quota := int64(cpus * cpuPeriod)
	return fmt.Sprintf("%d %d", quota, cpuPeriod)
}

// PidsMaxLine formats pids.max's content. limit <= 0 means no limit.
func PidsMaxLine(limit int64) string {
	if limit <= 0 {
		return "max"
	}
	return strconv.FormatInt(limit, 10)
}

// MemoryMaxLine formats memory.max's content. bytes <= 0 means no limit.
func MemoryMaxLine(bytes int64) string {
	if bytes <= 0 {
		return "max"
	}
	return strconv.FormatInt(bytes, 10)
}
