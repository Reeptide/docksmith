package cmd

import (
	"os"
	"os/user"
	"path/filepath"
)

// stateRoot returns the directory holding images, layers, cache, and
// containers.
//
// Resolution order, and why it is not simply $HOME:
//
//  1. $DOCKSMITH_ROOT — explicit override, mainly for tests and for anyone who
//     wants machine-wide state under /var/lib.
//  2. $SUDO_USER's home — build and run need root, but images, ps, and logs do
//     not. sudo resets HOME to /root under the default sudoers env_reset, so
//     naively trusting $HOME means `sudo docksmith build` writes to
//     /root/.docksmith while an unprivileged `docksmith images` reads
//     ~/.docksmith and reports an empty store. Anchoring to the invoking user's
//     home keeps one state directory per human regardless of privilege.
//  3. $HOME — the ordinary non-sudo case.
func stateRoot() string {
	if root := os.Getenv("DOCKSMITH_ROOT"); root != "" {
		return root
	}
	if home := sudoUserHome(); home != "" {
		return filepath.Join(home, ".docksmith")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "/tmp/.docksmith"
	}
	return filepath.Join(home, ".docksmith")
}

// sudoUserHome returns the home directory of the user who invoked sudo, or ""
// when not running under sudo (or when the user cannot be resolved).
func sudoUserHome() string {
	name := os.Getenv("SUDO_USER")
	if name == "" || name == "root" {
		return ""
	}
	u, err := user.Lookup(name)
	if err != nil || u.HomeDir == "" {
		return ""
	}
	return u.HomeDir
}
