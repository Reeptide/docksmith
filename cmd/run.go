package cmd

import (
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"sync/atomic"
	"syscall"

	"docksmith/internal/builder"
	"docksmith/internal/container"
	"docksmith/internal/image"
	"docksmith/internal/network"
	"docksmith/internal/runtime"
	"docksmith/internal/store"
)

// repeatedFlag collects a flag that may be given more than once, such as
// -e KEY=VALUE or -v host:container.
type repeatedFlag []string

func (r *repeatedFlag) String() string { return strings.Join(*r, ",") }

func (r *repeatedFlag) Set(v string) error {
	*r = append(*r, v)
	return nil
}

func RunContainer(args []string) error {
	var envArgs, volArgs, portArgs repeatedFlag
	var detach, autoRm bool
	var name, netMode, memory, cpus, pidsLimit string

	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.Var(&envArgs, "e", "set an environment variable (KEY=VALUE), repeatable")
	fs.Var(&volArgs, "v", "bind mount a host path (host:container[:ro]), repeatable")
	fs.Var(&portArgs, "p", "publish a port (hostPort:containerPort[/tcp|udp]), repeatable")
	fs.BoolVar(&detach, "d", false, "run in the background and print the container id")
	fs.BoolVar(&autoRm, "rm", false, "remove the container when it exits")
	fs.StringVar(&name, "name", "", "assign a name to the container")
	fs.StringVar(&netMode, "net", NetBridge, "network mode: bridge, none or host")
	fs.StringVar(&memory, "memory", "", "memory limit (e.g. 512m, 1g)")
	fs.StringVar(&memory, "m", "", "shorthand for --memory")
	fs.StringVar(&cpus, "cpus", "", "CPU limit as a fractional core count (e.g. 1.5)")
	fs.StringVar(&pidsLimit, "pids-limit", "", fmt.Sprintf("maximum number of processes (minimum %d; docksmith's own container init needs headroom)", runtime.MinPidsLimit))
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: docksmith run [-d] [--rm] [--name N] [--net M] [-e K=V] [-v host:ctr[:ro]] [-p host:ctr] [-m size] [--cpus N] [--pids-limit N] <name:tag> [cmd [args...]]")
		fs.PrintDefaults()
	}
	// Parse stops at the first non-flag argument, which gives exactly the
	// "run [flags] image [cmd...]" grammar — everything after the image name
	// belongs to the container, not to docksmith.
	if err := fs.Parse(args); err != nil {
		return err
	}

	rest := fs.Args()
	if len(rest) == 0 {
		fs.Usage()
		return fmt.Errorf("run: no image specified")
	}
	nameTag := rest[0]
	cmdOverride := rest[1:]

	envOverrides, err := parseEnvArgs(envArgs)
	if err != nil {
		return err
	}
	mounts, err := parseVolumeArgs(volArgs)
	if err != nil {
		return err
	}
	ports, err := parsePortArgs(portArgs)
	if err != nil {
		return err
	}
	if err := validateNetMode(netMode, ports); err != nil {
		return err
	}
	resources, err := parseResourceArgs(memory, cpus, pidsLimit)
	if err != nil {
		return err
	}

	root := stateRoot()
	st, err := store.NewState(root)
	if err != nil {
		return err
	}

	imgName, tag := image.ParseNameTag(nameTag)
	m, err := image.Load(st.ImagesDir, imgName, tag)
	if err != nil {
		return fmt.Errorf("run: %w", err)
	}

	ports = applyExposedDefaults(ports, m.Config.ExposedPorts)
	for _, p := range ports {
		if p.ContainerPort == 0 {
			return fmt.Errorf("run: -p %d has no container port and %s declares no EXPOSE",
				p.HostPort, nameTag)
		}
	}

	finalCmd := m.Config.Cmd
	if len(cmdOverride) > 0 {
		finalCmd = cmdOverride
	}
	if len(finalCmd) == 0 {
		return fmt.Errorf("run: no CMD defined in image and no command given")
	}

	if err := checkHostPortConflicts(root, ports, ""); err != nil {
		return fmt.Errorf("run: %w", err)
	}

	if name == "" {
		name = container.GenerateName(root)
	} else if existing, _ := container.Resolve(root, name); existing != nil && existing.Name == name {
		return fmt.Errorf("run: the container name %q is already in use by %s",
			name, container.ShortID(existing.ID))
	}

	id, err := container.NewID()
	if err != nil {
		return err
	}

	envMap := make(map[string]string)
	for _, e := range m.Config.Env {
		if k, v, ok := strings.Cut(e, "="); ok {
			envMap[k] = v
		}
	}
	for k, v := range envOverrides {
		envMap[k] = v
	}

	rec := &container.Record{
		ID:         id,
		Name:       name,
		Image:      nameTag,
		ImageID:    m.Digest,
		Command:    finalCmd,
		Env:        envMap,
		WorkingDir: m.Config.WorkingDir,
		Mounts:     mounts,
		Detached:   detach,
		AutoRm:     autoRm,
		NetMode:    netMode,
		Ports:      ports,
		Resources:  resources,
	}
	if err := container.Create(root, rec); err != nil {
		return fmt.Errorf("run: creating container: %w", err)
	}

	// Assemble into the container's own directory rather than a temp dir, so
	// the filesystem survives for `docksmith logs` and post-mortem inspection.
	if err := builder.AssembleRootFSInto(m, st, rec.RootFSPath()); err != nil {
		container.Remove(rec)
		return fmt.Errorf("run: assembling rootfs: %w", err)
	}

	// Allocate the address and prepare the host side before starting, so the
	// child can be told its address up front. Attaching the interface has to
	// wait for the pid; see attachNetwork.
	netCfg, err := prepareNetwork(root, rec)
	if err != nil {
		teardownNetwork(root, rec)
		container.Remove(rec)
		return fmt.Errorf("run: %w", err)
	}
	rec.Network = netCfg
	if netMode != NetHost {
		if err := network.WriteHostFiles(rec.RootFSPath(), container.ShortID(rec.ID), rec.IP); err != nil {
			teardownNetwork(root, rec)
			container.Remove(rec)
			return fmt.Errorf("run: %w", err)
		}
	}
	if err := container.Save(rec); err != nil {
		// Same cleanup as every other failure between Create and start. Without
		// it this path leaves a record stuck in "created" holding a rootfs and
		// an IP lease that nothing will ever release.
		teardownNetwork(root, rec)
		container.Remove(rec)
		return fmt.Errorf("run: %w", err)
	}

	if detach {
		return startDetached(root, rec)
	}
	return runForeground(root, rec)
}

// runForeground runs the container attached to this terminal, teeing output to
// the log so `docksmith logs` works for foreground containers too.
func runForeground(root string, rec *container.Record) error {
	logFile, err := os.OpenFile(rec.LogPath(), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		return fmt.Errorf("run: opening log: %w", err)
	}
	defer logFile.Close()

	stdout, stopOut := teeToLog(os.Stdout, logFile)
	stderr, stopErr := teeToLog(os.Stderr, logFile)

	// Ctrl-C must not kill docksmith outright.
	//
	// The terminal sends SIGINT to the whole foreground process group, which
	// contains both this process and the container. Under the default
	// disposition docksmith dies first, so the IP lease is never released, the
	// container's DNAT rules stay installed pointing at an address that is
	// about to be handed to someone else, and the record is left claiming to be
	// running. Catching the signal keeps this process alive long enough to run
	// exactly the same teardown an ordinary exit runs.
	//
	// The signal is also forwarded explicitly, because a signal sent to this
	// process alone (`kill` from a script, rather than a terminal) never
	// reaches the container. A second signal escalates to SIGKILL, so an init
	// that ignores SIGTERM cannot hold the terminal hostage.
	var childPid atomic.Int64
	stopSignals := forwardSignals(&childPid)

	exitCode, runErr := runtime.IsolatedRun(runtime.RunOptions{
		RootFS:     rec.RootFSPath(),
		Command:    rec.Command,
		WorkingDir: rec.WorkingDir,
		Env:        rec.Env,
		Hostname:   container.ShortID(rec.ID),
		Mounts:     rec.Mounts,
		Network:    rec.Network,
		Stdout:     stdout,
		Stderr:     stderr,
		OnStart: func(pid int) error {
			// Runs while the container waits on the sync barrier: the veth peer
			// is moved by pid, so the process must exist, but the interface has
			// to be there before it runs.
			if err := attachNetwork(root, rec, pid); err != nil {
				return err
			}
			if err := attachCgroup(rec, pid); err != nil {
				return err
			}
			childPid.Store(int64(pid))
			rec.MarkStarted(pid)
			return container.Save(rec)
		},
	})

	stopSignals()
	stopOut()
	stopErr()

	teardownNetwork(root, rec)
	teardownCgroup(rec)

	if runErr != nil {
		rec.MarkExited(255)
		container.Save(rec)
		return fmt.Errorf("run: %w", runErr)
	}

	rec.MarkExited(exitCode)
	if err := container.Save(rec); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not record exit status: %v\n", err)
	}
	if rec.AutoRm {
		if err := container.Remove(rec); err != nil {
			fmt.Fprintf(os.Stderr, "warning: could not remove container: %v\n", err)
		}
	}

	if exitCode != 0 {
		fmt.Fprintf(os.Stderr, "Container exited with code %d\n", exitCode)
		os.Exit(exitCode)
	}
	return nil
}

// forwardSignals catches the signals that would otherwise kill docksmith while
// a foreground container is running and relays them to the container instead.
// The returned function stops the relay; it is safe to call exactly once.
//
// pid is read through an atomic because it is only known once the container has
// been cloned, which happens on the caller's goroutine while this one is
// already listening. Zero means "not started yet": the signal is dropped rather
// than sent to process group 0, which would deliver it to docksmith itself and
// every one of its siblings.
func forwardSignals(pid *atomic.Int64) func() {
	ch := make(chan os.Signal, 4)
	signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)

	go func() {
		var seen int
		for s := range ch {
			target := int(pid.Load())
			if target <= 0 {
				continue
			}
			seen++
			sig := syscall.SIGKILL
			if seen == 1 {
				if unixSig, ok := s.(syscall.Signal); ok {
					sig = unixSig
				} else {
					sig = syscall.SIGTERM
				}
			}
			syscall.Kill(target, sig)
		}
	}()

	return func() {
		// Stop before close: after Notify has been undone no further sends can
		// happen, so closing cannot race a delivery.
		signal.Stop(ch)
		close(ch)
	}
}

// teeToLog returns a pipe whose writes reach both the terminal and the log
// file. IsolatedRun needs *os.File for the child's stdio, so an io.MultiWriter
// cannot be handed over directly — a pipe bridges the two.
func teeToLog(term *os.File, log *os.File) (*os.File, func()) {
	pr, pw, err := os.Pipe()
	if err != nil {
		return term, func() {} // fall back to the terminal alone
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		io.Copy(io.MultiWriter(term, log), pr)
		pr.Close()
	}()
	return pw, func() {
		pw.Close()
		<-done
	}
}

func parseEnvArgs(args []string) (map[string]string, error) {
	out := make(map[string]string, len(args))
	for _, a := range args {
		k, v, ok := strings.Cut(a, "=")
		if !ok || k == "" {
			return nil, fmt.Errorf("-e: invalid format %q, expected KEY=VALUE", a)
		}
		out[k] = v
	}
	return out, nil
}

func parseVolumeArgs(args []string) ([]runtime.Mount, error) {
	seen := make(map[string]string, len(args))
	out := make([]runtime.Mount, 0, len(args))
	for _, a := range args {
		m, err := runtime.ParseMount(a)
		if err != nil {
			return nil, err
		}
		// Two mounts on the same target would silently shadow each other, and
		// which one wins depends on ordering — reject it instead.
		if prev, dup := seen[m.Target]; dup {
			return nil, fmt.Errorf("-v: %q and %q both mount onto %s", prev, a, m.Target)
		}
		seen[m.Target] = a

		if err := runtime.EnsureSource(m); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, nil
}

// parseResourceArgs turns the raw -m/--cpus/--pids-limit flag values into a
// runtime.ResourceLimits, or nil if none were given — rec.Resources then
// stays nil and no cgroup is ever created, the same nil-means-off convention
// used for *runtime.NetworkConfig.
func parseResourceArgs(memory, cpus, pidsLimit string) (*runtime.ResourceLimits, error) {
	if memory == "" && cpus == "" && pidsLimit == "" {
		return nil, nil
	}
	var lim runtime.ResourceLimits
	if memory != "" {
		b, err := runtime.ParseMemorySize(memory)
		if err != nil {
			return nil, fmt.Errorf("-m: %w", err)
		}
		lim.MemoryBytes = b
	}
	if cpus != "" {
		c, err := runtime.ParseCPUs(cpus)
		if err != nil {
			return nil, fmt.Errorf("--cpus: %w", err)
		}
		lim.CPUQuota = c
	}
	if pidsLimit != "" {
		n, err := runtime.ParsePidsLimit(pidsLimit)
		if err != nil {
			return nil, fmt.Errorf("--pids-limit: %w", err)
		}
		lim.PidsLimit = n
	}
	return &lim, nil
}
