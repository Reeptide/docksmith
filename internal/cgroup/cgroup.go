// Package cgroup applies cgroup v2 resource limits to containers.
//
// Membership is set purely by pid, from outside any namespace, by writing to
// cgroup.procs — unlike mounts or network configuration this needs no
// cooperation from the child and no ChildConfig plumbing at all.
package cgroup

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"docksmith/internal/runtime"
)

// Root is the parent cgroup for every container docksmith creates.
const Root = "/sys/fs/cgroup/docksmith"

// cgroup2SuperMagic is CGROUP2_SUPER_MAGIC from linux/magic.h, used to confirm
// /sys/fs/cgroup is mounted unified rather than the legacy v1 hierarchy.
const cgroup2SuperMagic = 0x63677270

// Dir returns the cgroup path for a container, keyed by its short id — the
// same derivation already used for its network interface name and hostname.
func Dir(shortID string) string {
	return filepath.Join(Root, shortID)
}

// Available reports whether /sys/fs/cgroup is a cgroup v2 (unified) mount.
func Available() bool {
	var stat syscall.Statfs_t
	if err := syscall.Statfs("/sys/fs/cgroup", &stat); err != nil {
		return false
	}
	return stat.Type == cgroup2SuperMagic
}

// EnsureParent creates the docksmith parent cgroup and delegates the
// controllers limits actually needs down to it.
//
// A child cgroup only gets memory.max/cpu.max/pids.max files once the
// corresponding controller is enabled in its parent's cgroup.subtree_control
// — that has to happen at the true root as well as at Root itself, since
// Root's own ability to enable a controller for its children depends on that
// controller being enabled for Root by ITS parent.
//
// Only the controllers the caller's limits actually use are requested: a
// pids-only limit must not fail on a host where cpu or memory delegation is
// unavailable, since pids alone would have worked fine.
func EnsureParent(limits *runtime.ResourceLimits) error {
	if !Available() {
		return fmt.Errorf("cgroup v2 not available at /sys/fs/cgroup")
	}
	if err := os.MkdirAll(Root, 0755); err != nil {
		return fmt.Errorf("creating %s: %w", Root, err)
	}
	controllers := neededControllers(limits)
	trueRoot := filepath.Dir(Root)
	if err := enableControllers(filepath.Join(trueRoot, "cgroup.subtree_control"), controllers); err != nil {
		return err
	}
	if err := enableControllers(filepath.Join(Root, "cgroup.subtree_control"), controllers); err != nil {
		return err
	}
	return nil
}

func neededControllers(limits *runtime.ResourceLimits) string {
	var parts []string
	if limits.MemoryBytes != 0 {
		parts = append(parts, "+memory")
	}
	if limits.CPUQuota != 0 {
		parts = append(parts, "+cpu")
	}
	if limits.PidsLimit != 0 {
		parts = append(parts, "+pids")
	}
	return strings.Join(parts, " ")
}

func enableControllers(subtreeControl, controllers string) error {
	return writeCgroupFile(subtreeControl, controllers)
}

// Create makes the per-container cgroup directory and writes its limit files.
// On any failure the directory is removed so the container is never started
// half-configured.
func Create(shortID string, limits *runtime.ResourceLimits) error {
	if limits.Empty() {
		return nil
	}
	dir := Dir(shortID)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("creating cgroup %s: %w", dir, err)
	}
	if err := writeLimits(dir, limits); err != nil {
		os.Remove(dir)
		return err
	}
	return nil
}

func writeLimits(dir string, limits *runtime.ResourceLimits) error {
	if limits.MemoryBytes != 0 {
		if err := writeCgroupFile(filepath.Join(dir, "memory.max"), runtime.MemoryMaxLine(limits.MemoryBytes)); err != nil {
			return err
		}
		// memory.max alone bounds RSS, not total footprint: with swap on, a
		// container can keep growing past -m by swapping instead of being
		// OOM-killed at the promised limit. Disabling swap for the cgroup
		// makes -m actually mean what it says. A host with swap accounting
		// compiled out has no memory.swap.max file at all — that is a host
		// limitation, not a docksmith misconfiguration, so it is the one part
		// of a resource limit allowed to degrade rather than hard-fail.
		swapPath := filepath.Join(dir, "memory.swap.max")
		if err := os.WriteFile(swapPath, []byte("0"), 0644); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("writing %s: %w", swapPath, err)
		}
	}
	if limits.CPUQuota != 0 {
		if err := writeCgroupFile(filepath.Join(dir, "cpu.max"), runtime.CPUMaxLine(limits.CPUQuota)); err != nil {
			return err
		}
	}
	if limits.PidsLimit != 0 {
		if err := writeCgroupFile(filepath.Join(dir, "pids.max"), runtime.PidsMaxLine(limits.PidsLimit)); err != nil {
			return err
		}
	}
	return nil
}

// Attach moves pid into the container's cgroup.
func Attach(shortID string, pid int) error {
	return writeCgroupFile(filepath.Join(Dir(shortID), "cgroup.procs"), strconv.Itoa(pid))
}

// Remove deletes the container's cgroup directory. Safe to call more than
// once and for a container that never had one — the kernel refuses rmdir
// while cgroup.procs is non-empty, which by the time teardown runs is already
// guaranteed: stop and rm both wait for the container's process to exit
// before releasing its resources.
func Remove(shortID string) error {
	if err := os.Remove(Dir(shortID)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("removing cgroup %s: %w", Dir(shortID), err)
	}
	return nil
}

func writeCgroupFile(path, value string) error {
	if err := os.WriteFile(path, []byte(value), 0644); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	return nil
}
