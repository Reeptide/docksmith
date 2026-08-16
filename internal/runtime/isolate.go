package runtime

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"docksmith/internal/network"
)

// File descriptors handed to the child. FD 0-2 are stdio; these follow.
//
//	FD 3 — error pipe, child to parent. The child writes a setup failure here
//	       and exits. It is CLOEXEC, so a successful exec closes it and the
//	       parent reads EOF, which is how "setup succeeded" is signalled.
//	FD 4 — sync barrier, parent to child. The child blocks reading one byte
//	       before touching anything, which gives the parent a window to do work
//	       that requires the child to already exist — moving a network
//	       interface into its namespace, for instance, which is addressed by
//	       pid. The parent always writes this byte, in every mode; closing
//	       without writing aborts the child.
const (
	errPipeFD  = 3
	syncPipeFD = 4
)

// Mount is a bind mount to set up inside the container before it starts.
type Mount struct {
	Source   string `json:"source"`
	Target   string `json:"target"`
	ReadOnly bool   `json:"readOnly"`
}

// NetworkConfig describes the container's network namespace setup. Populated
// by the networking layer; nil means "inherit the host network".
type NetworkConfig struct {
	Interface string `json:"interface,omitempty"`
	IP        string `json:"ip,omitempty"`
	Gateway   string `json:"gateway,omitempty"`
	MTU       int    `json:"mtu,omitempty"`
}

// ChildConfig is the complete instruction set handed to the re-executed child,
// serialised as a single JSON argv element.
//
// It replaced a positional argument list, which could not be extended without
// ambiguity once mounts and network settings entered the picture.
type ChildConfig struct {
	RootFS   string         `json:"rootfs"`
	WorkDir  string         `json:"workdir"`
	Command  []string       `json:"command"`
	Hostname string         `json:"hostname,omitempty"`
	UseInit  bool           `json:"useInit"`
	Mounts   []Mount        `json:"mounts,omitempty"`
	Network  *NetworkConfig `json:"network,omitempty"`
}

// RunOptions configures an isolated process.
type RunOptions struct {
	RootFS       string            // assembled rootfs directory
	Command      []string          // command + args
	WorkingDir   string            // working dir inside rootfs (defaults to /)
	Env          map[string]string // environment variables
	EnvOverrides map[string]string // -e overrides at runtime
	Hostname     string            // container hostname (CLONE_NEWUTS)
	Mounts       []Mount           // bind mounts, including user volumes
	Network      *NetworkConfig    // nil means share the host network namespace

	Stdout *os.File
	Stderr *os.File
	Stdin  *os.File

	// OnStart runs after the child exists but before it proceeds past the sync
	// barrier, so it can act on the child's pid — moving a veth endpoint into
	// its network namespace, or recording the pid for `docksmith ps`. Returning
	// an error aborts the container.
	OnStart func(pid int) error
}

// childConfigFor derives the child's configuration from the parent's options.
//
// Its own function so the derivation can be tested without cloning a namespace.
//
// UseInit is unconditional, and that is a decision rather than an oversight.
// The init is in-process — ChildMain is already PID 1 and forks the real
// command as PID 2 — so it costs nothing and leaves no trace in the rootfs,
// which is what an earlier design that bind-mounted an init binary could not
// say. Turning it off would break `stop`: per pid_namespaces(7) a PID 1 with no
// SIGTERM handler discards the signal, so every stop would degrade to SIGKILL
// after the full timeout. Build RUN steps get it too, where it reaps anything
// the command daemonises before the delta is taken.
func childConfigFor(opts RunOptions, workDir string) ChildConfig {
	return ChildConfig{
		RootFS:   opts.RootFS,
		WorkDir:  workDir,
		Command:  opts.Command,
		Hostname: opts.Hostname,
		UseInit:  true,
		Mounts:   opts.Mounts,
		Network:  opts.Network,
	}
}

// IsolatedRun executes a command inside rootfs using Linux namespaces.
// Requires root: it uses CLONE_NEWNS/NEWPID/NEWUTS/NEWIPC and pivot_root.
func IsolatedRun(opts RunOptions) (int, error) {
	if len(opts.Command) == 0 {
		return 1, fmt.Errorf("no command specified")
	}

	workDir := opts.WorkingDir
	if workDir == "" {
		workDir = "/"
	}

	self, err := os.Executable()
	if err != nil {
		return 1, fmt.Errorf("cannot find executable: %w", err)
	}

	cfg := childConfigFor(opts, workDir)
	cfgJSON, err := json.Marshal(cfg)
	if err != nil {
		return 1, fmt.Errorf("encoding child config: %w", err)
	}

	errR, errW, err := os.Pipe()
	if err != nil {
		return 1, fmt.Errorf("creating error pipe: %w", err)
	}
	defer errR.Close()

	syncR, syncW, err := os.Pipe()
	if err != nil {
		errW.Close()
		return 1, fmt.Errorf("creating sync pipe: %w", err)
	}
	defer syncW.Close()

	cloneFlags := syscall.CLONE_NEWNS |
		syscall.CLONE_NEWPID |
		syscall.CLONE_NEWUTS |
		syscall.CLONE_NEWIPC
	if opts.Network != nil {
		cloneFlags |= syscall.CLONE_NEWNET
	}

	cmd := exec.Command(self, "__child__", string(cfgJSON))
	cmd.Env = append(envSlice(opts.Env, opts.EnvOverrides), "__DOCKSMITH_CHILD__=1")
	cmd.ExtraFiles = []*os.File{errW, syncR} // become FD 3 and FD 4
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags: uintptr(cloneFlags),
		// If this process dies, take the container with it rather than leaving
		// an orphan running with no one to record its exit status.
		// Pdeathsig is per-thread in Go: it is tied to the OS thread that
		// called clone, and the goroutine running cmd.Start() is not locked to
		// one. If the runtime ever retires that M the container takes a
		// spurious SIGKILL. Go caches Ms aggressively so this is rare, and the
		// alternative — an orphaned container whose supervisor is gone and
		// whose exit code nothing will ever record — is worse. Known runc-era
		// footgun; noted rather than worked around.
		Pdeathsig: syscall.SIGKILL,
	}
	cmd.Stdout = orStd(opts.Stdout, os.Stdout)
	cmd.Stderr = orStd(opts.Stderr, os.Stderr)
	cmd.Stdin = orStd(opts.Stdin, os.Stdin)

	if err := cmd.Start(); err != nil {
		errW.Close()
		syncR.Close()
		return 1, fmt.Errorf("failed to start isolated process: %w", err)
	}

	// Close this process's copies of the child's ends. The error pipe in
	// particular must be closed here or the parent would never see EOF.
	errW.Close()
	syncR.Close()

	// Drain the error pipe in the background. It cannot be read inline: the
	// read blocks until the child execs, and the child is blocked on the sync
	// barrier below waiting for this process — which would deadlock.
	setupErr := make(chan string, 1)
	go func() {
		var buf bytes.Buffer
		io.Copy(&buf, errR)
		setupErr <- buf.String()
	}()

	// Work that needs the child to exist happens here, while it waits.
	var startErr error
	if opts.OnStart != nil {
		startErr = opts.OnStart(cmd.Process.Pid)
	}

	if startErr == nil {
		// Release the child. Written unconditionally in every mode — a missing
		// write would hang the container forever with no timeout.
		if _, err := syncW.Write([]byte{1}); err != nil {
			startErr = fmt.Errorf("releasing container: %w", err)
		}
	}
	syncW.Close() // closing without a write tells the child to abort

	if startErr != nil {
		cmd.Process.Kill()
		cmd.Wait()
		return 1, startErr
	}

	waitErr := cmd.Wait()

	if msg := <-setupErr; msg != "" {
		return 1, fmt.Errorf("container setup: %s", msg)
	}
	if waitErr != nil {
		if exitErr, ok := waitErr.(*exec.ExitError); ok {
			return exitStatus(exitErr), nil
		}
		return 1, waitErr
	}
	return 0, nil
}

// exitStatus maps a finished process to a shell-style exit code.
//
// ExitError.ExitCode() returns -1 when a process was killed by a signal rather
// than exiting, which is not a usable exit code: it reaches os.Exit as 255 and
// is recorded in config.json as "Exited (-1)". `docksmith stop` escalating to
// SIGKILL takes exactly this path. The init loop already reports 128+signal for
// the command it supervises; this applies the same convention one level up.
func exitStatus(e *exec.ExitError) int {
	if ws, ok := e.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return 128 + int(ws.Signal())
	}
	if code := e.ExitCode(); code >= 0 {
		return code
	}
	return 1
}

// ChildMain is the re-exec entry point. It performs namespace setup, enters the
// rootfs and runs the command. Returns false when not in child mode.
func ChildMain(args []string) bool {
	if os.Getenv("__DOCKSMITH_CHILD__") != "1" {
		return false
	}
	if len(args) < 2 {
		fmt.Fprintln(os.Stderr, "docksmith: invalid child args")
		os.Exit(1)
	}

	var cfg ChildConfig
	if err := json.Unmarshal([]byte(args[1]), &cfg); err != nil {
		fmt.Fprintf(os.Stderr, "docksmith: bad child config: %v\n", err)
		os.Exit(1)
	}

	// CLOEXEC so a successful exec closes it, which is how the parent learns
	// setup completed.
	errPipe := os.NewFile(errPipeFD, "errpipe")
	syscall.CloseOnExec(errPipeFD)
	syncPipe := os.NewFile(syncPipeFD, "syncpipe")
	syscall.CloseOnExec(syncPipeFD)

	fail := func(format string, a ...any) {
		fmt.Fprintf(errPipe, format, a...)
		errPipe.Close()
		os.Exit(1)
	}

	// Wait for the parent to finish anything that needed this process to exist.
	var release [1]byte
	if _, err := io.ReadFull(syncPipe, release[:]); err != nil {
		fail("aborted before start: %v", err)
	}
	syncPipe.Close()

	// A deterministic umask, so files created by RUN steps get the same
	// permissions regardless of how the host is configured — layer digests
	// depend on it.
	syscall.Umask(0022)

	if cfg.Hostname != "" {
		if err := syscall.Sethostname([]byte(cfg.Hostname)); err != nil {
			fmt.Fprintf(os.Stderr, "warning: sethostname: %v\n", err)
		}
	}

	// Network configuration happens first, while the process is still in its
	// own root: it is pure netlink over a socket and needs nothing from the
	// filesystem, and doing it here keeps the failure separate from mount
	// problems.
	if cfg.Network != nil {
		if err := configureNetwork(cfg.Network); err != nil {
			fail("%v", err)
		}
	}

	if err := makeMountsPrivate(); err != nil {
		fail("%v", err)
	}
	if err := mountProc(cfg.RootFS); err != nil {
		// Non-fatal: some environments disallow it, and most commands do not
		// need /proc.
		fmt.Fprintf(os.Stderr, "warning: mount proc: %v\n", err)
	}
	mountDevices(cfg.RootFS)
	if err := applyMounts(cfg.RootFS, cfg.Mounts); err != nil {
		fail("%v", err)
	}

	if err := enterRootFS(cfg.RootFS); err != nil {
		fail("%v", err)
	}

	if err := syscall.Chdir(cfg.WorkDir); err != nil {
		// Not fatal — Docker errors here, but a WORKDIR that the image does not
		// contain is a common enough authoring slip that killing the container
		// over it is unhelpful. It is announced, though: falling back silently
		// meant a container that ran in / while the user believed it was in
		// /app, and every relative path in the command quietly resolved
		// somewhere else.
		fmt.Fprintf(os.Stderr, "docksmith: cannot enter %s (%v); running in /\n",
			cfg.WorkDir, err)
		if err2 := syscall.Chdir("/"); err2 != nil {
			fail("chdir /: %v", err2)
		}
	}

	bin := lookPath(cfg.Command[0])
	argv := cfg.Command

	if cfg.UseInit {
		// The init loop does not exec, so the CLOEXEC trick cannot signal
		// success — close the pipe explicitly or the parent waits forever.
		errPipe.Close()

		wd, err := os.Getwd()
		if err != nil {
			wd = "/"
		}
		code, err := runInit(bin, argv, os.Environ(), wd)
		if err != nil {
			fmt.Fprintf(os.Stderr, "docksmith: %v\n", err)
			os.Exit(1)
		}
		os.Exit(code)
	}

	if err := syscall.Exec(bin, argv, os.Environ()); err != nil {
		fail("exec %v: %v", cfg.Command, err)
	}
	return true // unreachable
}

// configureNetwork brings up the container's interfaces from inside its
// network namespace.
//
// A non-nil Network means CLONE_NEWNET was set, so this process has its own
// empty network stack. An empty IP means --net=none: loopback only, which is
// still worth configuring because software routinely assumes 127.0.0.1 works.
func configureNetwork(n *NetworkConfig) error {
	if n.IP == "" {
		if err := network.EnableLoopback(); err != nil {
			return fmt.Errorf("configuring loopback: %w", err)
		}
		return nil
	}
	if err := network.ConfigureInside(n.Interface, n.IP, n.Gateway, n.MTU); err != nil {
		return fmt.Errorf("configuring container network: %w", err)
	}
	return nil
}

// applyMounts sets up bind mounts inside rootfs before the root is switched.
func applyMounts(rootfs string, mounts []Mount) error {
	for _, m := range mounts {
		if err := bindMount(rootfs, m); err != nil {
			return err
		}
	}
	return nil
}

// lookPath resolves a command against PATH inside the new root. Absolute paths
// are returned unchanged.
func lookPath(bin string) string {
	if strings.HasPrefix(bin, "/") {
		return bin
	}
	pathEnv := os.Getenv("PATH")
	if pathEnv == "" {
		pathEnv = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
	}
	for _, dir := range filepath.SplitList(pathEnv) {
		candidate := filepath.Join(dir, bin)
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	return bin
}

// envSlice merges image environment with runtime overrides, sorted so the
// child's environment is reproducible.
func envSlice(env, overrides map[string]string) []string {
	merged := make(map[string]string, len(env)+len(overrides))
	for k, v := range env {
		merged[k] = v
	}
	for k, v := range overrides {
		merged[k] = v
	}
	keys := make([]string, 0, len(merged))
	for k := range merged {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(merged))
	for _, k := range keys {
		out = append(out, k+"="+merged[k])
	}
	return out
}

func orStd(f *os.File, fallback *os.File) *os.File {
	if f != nil {
		return f
	}
	return fallback
}
