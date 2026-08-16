package cmd

import (
	"os"
	"os/user"
	"path/filepath"
	"testing"
)

func TestStateRootExplicitOverrideWins(t *testing.T) {
	t.Setenv("DOCKSMITH_ROOT", "/custom/location")
	t.Setenv("SUDO_USER", "somebody")
	t.Setenv("HOME", "/root")
	if got := stateRoot(); got != "/custom/location" {
		t.Errorf("stateRoot() = %q, want /custom/location", got)
	}
}

func TestStateRootUsesHomeWithoutSudo(t *testing.T) {
	t.Setenv("DOCKSMITH_ROOT", "")
	t.Setenv("SUDO_USER", "")
	t.Setenv("HOME", "/home/example")
	if got := stateRoot(); got != filepath.Join("/home/example", ".docksmith") {
		t.Errorf("stateRoot() = %q, want /home/example/.docksmith", got)
	}
}

// The bug this fixes: sudo resets HOME to /root, so `sudo docksmith build`
// wrote to /root/.docksmith while an unprivileged `docksmith images` read
// ~/.docksmith and reported an empty store.
func TestStateRootFollowsSudoUserNotRootHome(t *testing.T) {
	u, err := user.Current()
	if err != nil {
		t.Skip("cannot resolve current user")
	}
	if u.Username == "root" {
		t.Skip("test is meaningless when actually running as root")
	}
	t.Setenv("DOCKSMITH_ROOT", "")
	t.Setenv("SUDO_USER", u.Username)
	t.Setenv("HOME", "/root") // what sudo actually does

	want := filepath.Join(u.HomeDir, ".docksmith")
	if got := stateRoot(); got != want {
		t.Errorf("stateRoot() = %q, want %q (must not follow HOME=/root)", got, want)
	}
}

// Running as root directly is not sudo, and must not be redirected anywhere.
func TestStateRootIgnoresSudoUserRoot(t *testing.T) {
	t.Setenv("DOCKSMITH_ROOT", "")
	t.Setenv("SUDO_USER", "root")
	t.Setenv("HOME", "/root")
	if got := stateRoot(); got != filepath.Join("/root", ".docksmith") {
		t.Errorf("stateRoot() = %q, want /root/.docksmith", got)
	}
}

func TestStateRootUnknownSudoUserFallsBackToHome(t *testing.T) {
	t.Setenv("DOCKSMITH_ROOT", "")
	t.Setenv("SUDO_USER", "no-such-user-hopefully-"+filepath.Base(os.TempDir()))
	t.Setenv("HOME", "/home/fallback")
	if got := stateRoot(); got != filepath.Join("/home/fallback", ".docksmith") {
		t.Errorf("stateRoot() = %q, want /home/fallback/.docksmith", got)
	}
}
