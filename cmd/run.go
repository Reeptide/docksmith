package cmd

import (
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

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
	var name, netMode string

	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.Var(&envArgs, "e", "set an environment variable (KEY=VALUE), repeatable")
	fs.Var(&volArgs, "v", "bind mount a host path (host:container[:ro]), repeatable")
	fs.Var(&portArgs, "p", "publish a port (hostPort:containerPort[/tcp|udp]), repeatable")
	fs.BoolVar(&detach, "d", false, "run in the background and print the container id")
	fs.BoolVar(&autoRm, "rm", false, "remove the container when it exits")
	fs.StringVar(&name, "name", "", "assign a name to the container")
	fs.StringVar(&netMode, "net", NetBridge, "network mode: bridge, none or host")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: docksmith run [-d] [--rm] [--name N] [--net M] [-e K=V] [-v host:ctr[:ro]] [-p host:ctr] <name:tag> [cmd [args...]]")
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
			rec.MarkStarted(pid)
			return container.Save(rec)
		},
	})

	stopOut()
	stopErr()

	teardownNetwork(root, rec)

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
