package runtime

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

// RunOptions configures an isolated process.
type RunOptions struct {
	RootFS       string            // assembled rootfs directory
	Command      []string          // command + args
	WorkingDir   string            // working dir inside rootfs (defaults to /)
	Env          map[string]string // environment variables
	EnvOverrides map[string]string // -e overrides at runtime
	Stdout       *os.File
	Stderr       *os.File
	Stdin        *os.File
}

// IsolatedRun executes a command inside rootfs using Linux namespaces.
// Uses CLONE_NEWPID + CLONE_NEWNS + chroot via /proc/self/exe re-exec.
// Requires CAP_SYS_ADMIN or user namespaces.
func IsolatedRun(opts RunOptions) (int, error) {
	if len(opts.Command) == 0 {
		return 1, fmt.Errorf("no command specified")
	}

	// Build env slice: image env first, then overrides.
	envMap := make(map[string]string)
	for k, v := range opts.Env {
		envMap[k] = v
	}
	for k, v := range opts.EnvOverrides {
		envMap[k] = v
	}
	var envSlice []string
	for k, v := range envMap {
		envSlice = append(envSlice, k+"="+v)
	}

	workDir := opts.WorkingDir
	if workDir == "" {
		workDir = "/"
	}

	// We use the re-exec pattern: this binary re-invokes itself with a special
	// env var set so it can perform the child-side setup (chroot, chdir, exec).
	self, err := os.Executable()
	if err != nil {
		return 1, fmt.Errorf("cannot find executable: %w", err)
	}

	// Create an error pipe so the child can report setup failures (chroot, chdir,
	// exec) back to the parent as structured messages instead of just exit code 1.
	// When exec succeeds the write-end is closed automatically (CLOEXEC), so the
	// parent reads EOF and knows setup completed.
	errR, errW, err := os.Pipe()
	if err != nil {
		return 1, fmt.Errorf("creating error pipe: %w", err)
	}

	// Build child args: __child__ <rootfs> <workdir> <cmd...>
	childArgs := append([]string{"__child__", opts.RootFS, workDir}, opts.Command...)

	cmd := exec.Command(self, childArgs...)
	cmd.Env = append(envSlice, "__DOCKSMITH_CHILD__=1")
	cmd.ExtraFiles = []*os.File{errW} // child receives as FD 3
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags: syscall.CLONE_NEWNS |
			syscall.CLONE_NEWPID |
			syscall.CLONE_NEWUTS |
			syscall.CLONE_NEWIPC,
	}

	if opts.Stdout != nil {
		cmd.Stdout = opts.Stdout
	} else {
		cmd.Stdout = os.Stdout
	}
	if opts.Stderr != nil {
		cmd.Stderr = opts.Stderr
	} else {
		cmd.Stderr = os.Stderr
	}
	if opts.Stdin != nil {
		cmd.Stdin = opts.Stdin
	} else {
		cmd.Stdin = os.Stdin
	}

	if err := cmd.Start(); err != nil {
		errR.Close()
		errW.Close()
		return 1, fmt.Errorf("failed to start isolated process: %w", err)
	}

	// Close parent's copy of the write-end so reads return EOF when the child
	// closes its copy (either explicitly on error or via CLOEXEC on exec).
	errW.Close()

	// Read any setup error message from the child.
	var errBuf bytes.Buffer
	io.Copy(&errBuf, errR)
	errR.Close()

	waitErr := cmd.Wait()

	// If the child reported a setup error, surface it.
	if errBuf.Len() > 0 {
		return 1, fmt.Errorf("container setup: %s", errBuf.String())
	}

	if waitErr != nil {
		if exitErr, ok := waitErr.(*exec.ExitError); ok {
			return exitErr.ExitCode(), nil
		}
		return 1, waitErr
	}
	return 0, nil
}

// ChildMain is called when this binary is re-executed as the container child.
// It performs chroot + chdir + exec of the real command.
// Returns true if we are in child mode (and handles execution), false otherwise.
func ChildMain(args []string) bool {
	if os.Getenv("__DOCKSMITH_CHILD__") != "1" {
		return false
	}
	// args: ["__child__", rootfs, workdir, cmd...]
	if len(args) < 4 {
		fmt.Fprintln(os.Stderr, "docksmith: invalid child args")
		os.Exit(1)
	}
	rootfs := args[1]
	workdir := args[2]
	command := args[3:]

	// Error pipe (FD 3) for reporting setup errors back to the parent.
	// Set CLOEXEC so the pipe is automatically closed when exec succeeds,
	// signaling to the parent that setup completed without error.
	errPipe := os.NewFile(3, "errpipe")
	syscall.CloseOnExec(int(errPipe.Fd()))

	// Set a deterministic umask so that files created by RUN commands
	// always get the same default permissions (0644 for files, 0755 for dirs),
	// regardless of the host's umask setting.
	syscall.Umask(0022)

	// Mount /proc inside rootfs so ps, top, etc. work.
	procDir := filepath.Join(rootfs, "proc")
	os.MkdirAll(procDir, 0555)
	if err := syscall.Mount("proc", procDir, "proc", 0, ""); err != nil {
		// Non-fatal — some environments don't allow this.
		fmt.Fprintf(os.Stderr, "warning: mount proc: %v\n", err)
	}

	// Bind-mount essential /dev nodes from the host into the rootfs.
	// Many programs expect /dev/null, /dev/zero, /dev/urandom to exist.
	// These bind mounts live in the child's mount namespace only and are
	// automatically cleaned up when the namespace is destroyed.
	devDir := filepath.Join(rootfs, "dev")
	os.MkdirAll(devDir, 0755)
	for _, dev := range []string{"null", "zero", "urandom", "random"} {
		hostDev := "/dev/" + dev
		containerDev := filepath.Join(devDir, dev)
		if _, err := os.Stat(hostDev); err != nil {
			continue // Host device not available, skip.
		}
		f, err := os.Create(containerDev)
		if err != nil {
			continue
		}
		f.Close()
		if err := syscall.Mount(hostDev, containerDev, "", syscall.MS_BIND, ""); err != nil {
			fmt.Fprintf(os.Stderr, "warning: mount /dev/%s: %v\n", dev, err)
		}
	}

	// Chroot into the assembled rootfs.
	if err := syscall.Chroot(rootfs); err != nil {
		fmt.Fprintf(errPipe, "chroot %s: %v", rootfs, err)
		errPipe.Close()
		os.Exit(1)
	}

	// Set working directory.
	if err := syscall.Chdir(workdir); err != nil {
		// Fallback to root.
		if err2 := syscall.Chdir("/"); err2 != nil {
			fmt.Fprintf(errPipe, "chdir /: %v", err2)
			errPipe.Close()
			os.Exit(1)
		}
	}

	// Find the binary inside the chrooted env.
	bin := command[0]
	cmdArgs := command[1:]

	// Try to find in PATH inside chroot if not absolute.
	if !strings.HasPrefix(bin, "/") {
		pathEnv := os.Getenv("PATH")
		if pathEnv == "" {
			pathEnv = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
		}
		for _, dir := range filepath.SplitList(pathEnv) {
			candidate := filepath.Join(dir, bin)
			if _, err := os.Stat(candidate); err == nil {
				bin = candidate
				break
			}
		}
	}

	// Exec replaces this process. CLOEXEC on errPipe means the pipe is
	// automatically closed on successful exec, signaling success to the parent.
	if err := syscall.Exec(bin, append([]string{command[0]}, cmdArgs...), os.Environ()); err != nil {
		fmt.Fprintf(errPipe, "exec %v: %v", command, err)
		errPipe.Close()
		os.Exit(1)
	}
	return true // unreachable
}
