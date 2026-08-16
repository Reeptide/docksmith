// Package safepath resolves paths inside a root directory without allowing
// them to escape it.
//
// Docksmith writes into directory trees it does not control: image layers
// during extraction, and a container's assembled rootfs during setup. Both run
// as root, and container setup runs in the host's mount namespace before
// pivot_root — so a path that escapes is a write to the real filesystem.
//
// filepath.Join is not sufficient. It collapses ".." lexically and says nothing
// about symlinks, so a layer containing "evil -> /etc" followed by an entry for
// "evil/passwd" passes every textual check and lands on the host's /etc/passwd.
// Containment has to be re-established after symlinks are resolved.
package safepath

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// Resolve returns the real path of rel inside root, following symlinks, and
// fails if the result lies outside root.
//
// rel need not exist: the deepest existing ancestor is resolved and the
// remaining components are appended, which is what makes this usable for paths
// that are about to be created.
func Resolve(root, rel string) (string, error) {
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		realRoot = filepath.Clean(root)
	}
	target := filepath.Join(realRoot, filepath.Clean("/"+rel))

	probe := target
	var trailing []string
	for {
		resolved, err := filepath.EvalSymlinks(probe)
		if err == nil {
			full := filepath.Join(append([]string{resolved}, trailing...)...)
			if !within(realRoot, full) {
				return "", fmt.Errorf("path %q escapes %s", rel, root)
			}
			return full, nil
		}
		if !os.IsNotExist(err) {
			return "", fmt.Errorf("resolving %q: %w", rel, err)
		}
		parent := filepath.Dir(probe)
		if parent == probe || len(probe) <= len(realRoot) {
			// Nothing along the chain exists yet, so there is no symlink to
			// follow and the lexical join is already contained.
			return target, nil
		}
		trailing = append([]string{filepath.Base(probe)}, trailing...)
		probe = parent
	}
}

// ResolveNoFollow is Resolve for paths that name an entry to be created,
// replaced or removed rather than read: every component *except the last* is
// resolved through symlinks, and the final one is left alone.
//
// The distinction is not cosmetic. A busybox rootfs is almost entirely symlinks
// into /bin/busybox, so resolving the last component turns an operation on
// bin/ls into the same operation on bin/busybox — deleting a whiteout's target
// takes the shell, and every other applet, with it. Following the last
// component is right only when the caller genuinely wants what the link points
// at, which for layer extraction is never.
func ResolveNoFollow(root, rel string) (string, error) {
	cleaned := strings.Trim(filepath.ToSlash(filepath.Clean("/"+rel)), "/")
	if cleaned == "" || cleaned == "." {
		return Resolve(root, "/")
	}
	dir, base := path.Split(cleaned)

	// The parent is resolved normally: traversing a symlinked directory is
	// legitimate, and Resolve is what enforces that it stays inside root.
	parent, err := Resolve(root, dir)
	if err != nil {
		return "", err
	}
	full := filepath.Join(parent, base)

	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		realRoot = filepath.Clean(root)
	}
	if !within(realRoot, full) {
		return "", fmt.Errorf("path %q escapes %s", rel, root)
	}
	return full, nil
}

// MkdirAll creates rel and its parents inside root, checking containment at
// every level so an existing symlink cannot be traversed on the way down.
func MkdirAll(root, rel string) (string, error) {
	cleaned := strings.Trim(filepath.ToSlash(filepath.Clean("/"+rel)), "/")
	target, err := Resolve(root, "/")
	if err != nil {
		return "", err
	}
	if cleaned == "" || cleaned == "." {
		return target, nil
	}
	var walked string
	for _, part := range strings.Split(cleaned, "/") {
		walked += "/" + part
		target, err = Resolve(root, walked)
		if err != nil {
			return "", err
		}
		if err := os.Mkdir(target, 0755); err != nil && !os.IsExist(err) {
			return "", err
		}
	}
	return target, nil
}

// within reports whether path is root itself or lies beneath it.
func within(root, path string) bool {
	if path == root {
		return true
	}
	return strings.HasPrefix(path, root+string(os.PathSeparator))
}
