package runtime

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"

	"docksmith/internal/safepath"
)

// oldRootDir is where the previous root is parked during pivot_root. It exists
// only for the few syscalls between pivot_root and umount, and is removed
// immediately afterwards — if it ever survives into a build rootfs it would be
// captured by snapshotDelta and baked into a layer, so builder.isVirtualPath
// excludes it as a belt-and-braces measure.
const oldRootDir = ".oldroot"

// makeMountsPrivate stops mount events propagating between the container and
// the host.
//
// On a systemd host "/" is MS_SHARED, so without this the /proc and /dev bind
// mounts set up below would propagate back into the host's mount namespace and
// leak out of the container. It also has to happen before pivot_root, which
// fails with EINVAL when the new root or its parent is MS_SHARED.
func makeMountsPrivate() error {
	if err := syscall.Mount("", "/", "", syscall.MS_REC|syscall.MS_PRIVATE, ""); err != nil {
		return fmt.Errorf("making mounts private: %w", err)
	}
	return nil
}

// mountProc mounts a private /proc inside the rootfs so ps, top and
// /proc/self/* observe the container's PID namespace rather than the host's.
func mountProc(rootfs string) error {
	dir, err := safepath.MkdirAll(rootfs, "proc")
	if err != nil {
		return err
	}
	return syscall.Mount("proc", dir, "proc", 0, "")
}

// mountDevices bind-mounts the handful of device nodes that ordinary programs
// assume exist. Creating real nodes would need mknod and CAP_MKNOD, and these
// bind mounts live only in the container's mount namespace, so they disappear
// with it.
func mountDevices(rootfs string) {
	if _, err := safepath.MkdirAll(rootfs, "dev"); err != nil {
		fmt.Fprintf(os.Stderr, "warning: creating /dev: %v\n", err)
		return
	}
	for _, dev := range []string{"null", "zero", "full", "random", "urandom", "tty"} {
		host := "/dev/" + dev
		if _, err := os.Stat(host); err != nil {
			continue
		}
		// NoFollow: the mount point is the entry at this path, not whatever it
		// points at. An image shipping dev/null as a link elsewhere would
		// otherwise have the host's /dev/null bind-mounted over the link's
		// target instead.
		target, err := safepath.ResolveNoFollow(rootfs, "dev/"+dev)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: /dev/%s: %v\n", dev, err)
			continue
		}
		if err := ensureMountPointFile(target, 0666); err != nil {
			continue
		}
		if err := syscall.Mount(host, target, "", syscall.MS_BIND, ""); err != nil {
			fmt.Fprintf(os.Stderr, "warning: mount /dev/%s: %v\n", dev, err)
		}
	}
}

// ParseMount parses a -v specification: "host:container" or
// "host:container:ro" (":rw" is accepted and is the default).
//
// Both paths must be absolute. A relative host path would be resolved against
// whatever directory the daemonless `docksmith run` happened to be invoked
// from, which for a detached container outliving that shell is meaningless.
func ParseMount(spec string) (Mount, error) {
	parts := strings.Split(spec, ":")
	if len(parts) < 2 || len(parts) > 3 {
		return Mount{}, fmt.Errorf("invalid volume %q: expected host:container[:ro]", spec)
	}

	m := Mount{Source: parts[0], Target: parts[1]}
	if m.Source == "" || m.Target == "" {
		return Mount{}, fmt.Errorf("invalid volume %q: empty path", spec)
	}
	if !filepath.IsAbs(m.Source) {
		return Mount{}, fmt.Errorf("invalid volume %q: host path %q must be absolute", spec, m.Source)
	}
	if !filepath.IsAbs(m.Target) {
		return Mount{}, fmt.Errorf("invalid volume %q: container path %q must be absolute", spec, m.Target)
	}

	if len(parts) == 3 {
		switch parts[2] {
		case "ro":
			m.ReadOnly = true
		case "rw":
			m.ReadOnly = false
		default:
			return Mount{}, fmt.Errorf("invalid volume %q: unknown option %q, expected ro or rw", spec, parts[2])
		}
	}

	m.Source = filepath.Clean(m.Source)
	m.Target = filepath.Clean(m.Target)
	return m, nil
}

// EnsureSource creates the host side of a bind mount if it does not exist,
// matching the behaviour people expect from `docker run -v`.
func EnsureSource(m Mount) error {
	if _, err := os.Stat(m.Source); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("volume source %s: %w", m.Source, err)
	}
	if m.ReadOnly {
		// Creating an empty directory only to forbid writing to it is never
		// what was meant; far more likely the path was mistyped.
		return fmt.Errorf("volume source %s does not exist (refusing to create it for a read-only mount)", m.Source)
	}
	if err := os.MkdirAll(m.Source, 0755); err != nil {
		return fmt.Errorf("creating volume source %s: %w", m.Source, err)
	}
	return nil
}

// ensureMountPointFile makes sure a file exists at path to bind-mount onto.
//
// A symlink already sitting there is replaced rather than opened: OpenFile
// follows links even when the path was resolved without following, so a
// dangling one would create the file at the link's target and the bind mount
// would land in the wrong place. Anything else that already exists is left
// alone — the mount covers it, and deleting image content to make room for a
// mount point would be destructive for no gain.
func ensureMountPointFile(path string, mode os.FileMode) error {
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSymlink == 0 {
			return nil
		}
		if err := os.Remove(path); err != nil {
			return err
		}
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDONLY|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	return f.Close()
}

// remountReadOnlyTree makes a bind mount and everything mounted underneath it
// read-only.
//
// Two separate kernel behaviours make this more than one call. The first is
// that MS_RDONLY is ignored on the initial MS_BIND, so a second
// MS_BIND|MS_REMOUNT|MS_RDONLY call is always required — the classic mistake.
// The second is that the remount applies to that mount alone: MS_REC on a
// remount does not propagate read-only to submounts. So `-v /mnt:/data:ro`
// where /mnt has anything mounted inside it produced a read-only /data with
// fully writable holes in it, which is worse than an honest failure because the
// user has been told it is read-only.
//
// Submounts come from /proc/self/mountinfo, read after the recursive bind so
// the copies under target are already present. They are remounted deepest-first
// so a failure leaves the outermost mount writable rather than half-applied.
func remountReadOnlyTree(target string) error {
	const flags = syscall.MS_BIND | syscall.MS_REMOUNT | syscall.MS_RDONLY

	points, err := submountsUnder(target)
	if err != nil {
		// Fall back to the single remount rather than refusing the mount: a
		// read-only top level is still better than nothing, and mountinfo being
		// unreadable is not the user's problem to solve.
		fmt.Fprintf(os.Stderr, "warning: cannot enumerate submounts of %s: %v\n", target, err)
		return syscall.Mount("", target, "", flags, "")
	}
	for _, p := range points {
		if err := syscall.Mount("", p, "", flags, ""); err != nil {
			return fmt.Errorf("%s: %w", p, err)
		}
	}
	return nil
}

// submountsUnder returns target and every mount point beneath it, deepest
// first. target is included even when it does not appear in mountinfo, so the
// caller always has something to remount.
func submountsUnder(target string) ([]string, error) {
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return parseSubmounts(f, target), nil
}

// parseSubmounts is the logic of submountsUnder, split out so it can be tested
// against a synthetic mountinfo.
//
// Reading the real /proc/self/mountinfo makes the test's outcome depend on what
// this machine happens to have mounted: an implementation that returned only
// the target and found nothing beneath it passed, because on a host with no
// nested mounts under the probe path that is also the correct answer. The bug
// this guards against is precisely "submounts are missed", so the input has to
// be one where submounts definitely exist.
func parseSubmounts(r io.Reader, target string) []string {
	data, err := io.ReadAll(r)
	if err != nil {
		return []string{target}
	}
	seen := map[string]bool{target: true}
	out := []string{target}
	for _, line := range strings.Split(string(data), "\n") {
		// Mount point is field 5 (1-indexed) and is octal-escaped.
		fields := strings.Fields(line)
		if len(fields) < 5 {
			continue
		}
		point := unescapeMountPath(fields[4])
		if point == target || !strings.HasPrefix(point, target+"/") || seen[point] {
			continue
		}
		seen[point] = true
		out = append(out, point)
	}
	// Deepest first: a child must be made read-only before its parent, or the
	// parent's remount can be undone by a later failure partway down.
	sort.Slice(out, func(i, j int) bool {
		return strings.Count(out[i], "/") > strings.Count(out[j], "/")
	})
	return out
}

// unescapeMountPath decodes the octal escapes mountinfo uses for space, tab,
// newline and backslash in a mount point.
func unescapeMountPath(s string) string {
	if !strings.Contains(s, "\\") {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if v, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(v))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// bindMount binds a host path to a path inside rootfs.
//
// Read-only needs two calls, not one. The kernel ignores every flag except
// MS_REC when MS_BIND is given (mount(2): "The remaining bits ... are also
// ignored"), so a single MS_BIND|MS_RDONLY silently produces a writable mount.
// The per-mountpoint flags can only be changed by a follow-up remount.
func bindMount(rootfs string, m Mount) error {
	info, err := os.Stat(m.Source)
	if err != nil {
		return fmt.Errorf("volume source %s: %w", m.Source, err)
	}

	var target string
	if info.IsDir() {
		target, err = safepath.MkdirAll(rootfs, m.Target)
		if err != nil {
			return fmt.Errorf("volume target %s: %w", m.Target, err)
		}
	} else {
		if _, err := safepath.MkdirAll(rootfs, filepath.Dir(m.Target)); err != nil {
			return fmt.Errorf("volume target %s: %w", m.Target, err)
		}
		// NoFollow for the same reason as /dev: -v host:/data must mount at
		// /data, even when the image ships /data as a symlink to somewhere
		// else in the rootfs.
		target, err = safepath.ResolveNoFollow(rootfs, m.Target)
		if err != nil {
			return fmt.Errorf("volume target %s: %w", m.Target, err)
		}
		if err := ensureMountPointFile(target, 0644); err != nil {
			return fmt.Errorf("volume target %s: %w", m.Target, err)
		}
	}

	if err := syscall.Mount(m.Source, target, "", syscall.MS_BIND|syscall.MS_REC, ""); err != nil {
		return fmt.Errorf("bind %s -> %s: %w", m.Source, m.Target, err)
	}
	if m.ReadOnly {
		if err := remountReadOnlyTree(target); err != nil {
			return fmt.Errorf("remounting %s read-only: %w", m.Target, err)
		}
	}
	return nil
}

// pivotRoot replaces the process's root filesystem with rootfs.
//
// It is used in preference to chroot because chroot only changes the path
// resolution root: a process holding a directory file descriptor from outside
// can still walk out of it. pivot_root swaps the mount itself and the old root
// is then unmounted, so there is nothing left to escape to.
//
// The returned bool reports whether the pivot already took effect. Once it has,
// the caller must NOT fall back to chroot — the process is running in a
// half-configured root, and a failure to unmount the old root leaves the entire
// host filesystem readable at /.oldroot, which is strictly worse than the
// chroot this replaces. Failures before that point are safe to fall back from.
func pivotRoot(rootfs string) (pivoted bool, err error) {
	// pivot_root requires the new root to be a mount point; bind rootfs onto
	// itself to make one.
	if err := syscall.Mount(rootfs, rootfs, "", syscall.MS_BIND|syscall.MS_REC, ""); err != nil {
		return false, fmt.Errorf("bind rootfs onto itself: %w", err)
	}
	if err := syscall.Chdir(rootfs); err != nil {
		return false, fmt.Errorf("chdir %s: %w", rootfs, err)
	}
	if err := os.MkdirAll(oldRootDir, 0700); err != nil {
		return false, fmt.Errorf("mkdir %s: %w", oldRootDir, err)
	}

	if err := syscall.PivotRoot(".", oldRootDir); err != nil {
		return false, fmt.Errorf("pivot_root: %w", err)
	}

	// Past this point the process is in the new root and cannot go back.
	if err := syscall.Chdir("/"); err != nil {
		return true, fmt.Errorf("chdir / after pivot_root: %w", err)
	}
	old := "/" + oldRootDir
	if err := syscall.Unmount(old, syscall.MNT_DETACH); err != nil {
		return true, fmt.Errorf("unmounting old root (host filesystem would stay visible at %s): %w", old, err)
	}
	if err := os.Remove(old); err != nil {
		// Not fatal: the mount is already detached, so this is an empty
		// directory rather than a view of the host. Still worth reporting,
		// because a stray .oldroot in a build rootfs ends up in a layer.
		fmt.Fprintf(os.Stderr, "warning: could not remove %s: %v\n", old, err)
	}
	return true, nil
}

// enterRootFS installs rootfs as the process root, preferring pivot_root and
// falling back to chroot only when the pivot never took effect.
func enterRootFS(rootfs string) error {
	pivoted, err := pivotRoot(rootfs)
	if err == nil {
		return nil
	}
	if pivoted {
		// Fatal by construction — see pivotRoot's contract. Falling back is not
		// even coherent here: pivot_root reports EINVAL when the current root is
		// not a mount point, which is exactly the state chroot leaves behind.
		return fmt.Errorf("pivot_root failed after taking effect: %w", err)
	}

	fmt.Fprintf(os.Stderr,
		"warning: pivot_root unavailable (%v); falling back to chroot, which is weaker isolation\n", err)
	if err := syscall.Chroot(rootfs); err != nil {
		return fmt.Errorf("chroot %s: %w", rootfs, err)
	}
	if err := syscall.Chdir("/"); err != nil {
		return fmt.Errorf("chdir / after chroot: %w", err)
	}
	return nil
}
