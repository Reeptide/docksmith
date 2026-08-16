package cmd

import (
	"fmt"
	"strconv"
	"strings"

	"docksmith/internal/container"
	"docksmith/internal/network"
	"docksmith/internal/runtime"
)

// Network modes accepted by --net.
const (
	NetBridge = "bridge"
	NetNone   = "none"
	NetHost   = "host"
)

// validateNetMode checks --net and its interaction with -p.
func validateNetMode(mode string, ports []network.PortMapping) error {
	switch mode {
	case NetBridge:
		return nil
	case NetNone, NetHost:
		if len(ports) > 0 {
			return fmt.Errorf("-p requires --net=bridge (got --net=%s)", mode)
		}
		return nil
	default:
		return fmt.Errorf("invalid --net=%s, expected bridge, none or host", mode)
	}
}

// prepareNetwork allocates an address and prepares the host side, returning the
// child's network configuration.
//
// The veth pair itself is not created here: it has to be moved into the
// container's network namespace, which is addressed by pid, so that step waits
// until the process exists. See attachNetwork.
func prepareNetwork(root string, rec *container.Record) (*runtime.NetworkConfig, error) {
	switch rec.NetMode {
	case NetHost:
		// No CLONE_NEWNET at all: the container shares the host's stack.
		return nil, nil
	case NetNone:
		// A network namespace with nothing but loopback.
		return &runtime.NetworkConfig{}, nil
	}

	if !network.HasIptables() {
		return nil, fmt.Errorf("iptables not found; use --net=none or --net=host")
	}
	if err := network.EnsureBridge(); err != nil {
		return nil, err
	}
	if err := network.EnsureNAT(); err != nil {
		return nil, err
	}

	cidr, err := network.Allocate(root, rec.ID)
	if err != nil {
		return nil, err
	}
	rec.IP = strings.Split(cidr, "/")[0]

	return &runtime.NetworkConfig{
		Interface: network.PeerName(rec.ID),
		IP:        cidr,
		Gateway:   network.GatewayIP,
		MTU:       1500,
	}, nil
}

// attachNetwork wires the container into the bridge once its pid is known, and
// publishes any requested ports. Called from OnStart, while the container waits
// on the sync barrier.
func attachNetwork(root string, rec *container.Record, pid int) error {
	if rec.NetMode != NetBridge {
		return nil
	}
	if err := network.AttachContainer(rec.ID, pid); err != nil {
		return err
	}
	if len(rec.Ports) == 0 {
		return nil
	}
	rules, err := network.PublishPorts(rec.IP, rec.Ports)
	if err != nil {
		network.DetachContainer(rec.ID)
		return err
	}
	rec.Rules = rules
	return nil
}

// teardownNetwork releases everything a container holds. Safe to call more than
// once, and for containers that never had a network.
func teardownNetwork(root string, rec *container.Record) {
	if rec.NetMode != NetBridge {
		return
	}
	network.UnpublishPorts(rec.Rules)
	rec.Rules = nil
	// The kernel deletes the veth pair when the namespace goes away; this
	// covers a container that failed during setup.
	network.DetachContainer(rec.ID)
	network.Release(root, rec.ID)
	rec.IP = ""
}

// parsePortArgs parses repeated -p flags, rejecting duplicate host ports.
func parsePortArgs(args []string) ([]network.PortMapping, error) {
	seen := make(map[string]string, len(args))
	out := make([]network.PortMapping, 0, len(args))
	for _, a := range args {
		p, err := network.ParsePortMapping(a)
		if err != nil {
			return nil, err
		}
		key := fmt.Sprintf("%d/%s", p.HostPort, p.Protocol)
		if prev, dup := seen[key]; dup {
			return nil, fmt.Errorf("-p: %q and %q both bind host port %d/%s",
				prev, a, p.HostPort, p.Protocol)
		}
		seen[key] = a
		out = append(out, p)
	}
	return out, nil
}

// ExposedPort parses an image's "80/tcp" entry into a port and protocol.
func ExposedPort(s string) (int, string, error) {
	port, proto := s, "tcp"
	if base, p, found := strings.Cut(s, "/"); found {
		port, proto = base, p
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return 0, "", fmt.Errorf("invalid exposed port %q", s)
	}
	return n, proto, nil
}

// applyExposedDefaults fills in the container side of a -p flag given as a bare
// host port, using the image's EXPOSE declaration.
//
// `-p 8080` is only meaningful when the image says what it listens on, so this
// is where EXPOSE stops being documentation and does something.
//
// Two rules that were wrong when this only ever consulted exposed[0]:
//
// Bare ports are matched by protocol. `-p 5353/udp` against an image declaring
// `EXPOSE 80/tcp 53/udp` must resolve to 53, not to 80 — and it must never have
// its protocol rewritten to tcp, which silently published the wrong protocol
// and produced a DNAT rule that could not match a single packet.
//
// Successive bare ports consume successive EXPOSE entries. `-p 8080 -p 9090`
// against `EXPOSE 80 443` means 80 and 443; mapping both to 80 produced two
// DNAT rules for the same destination, and the duplicate-host-port check in
// parsePortArgs runs earlier so nothing caught it.
func applyExposedDefaults(ports []network.PortMapping, exposed []string) []network.PortMapping {
	if len(exposed) == 0 {
		return ports
	}

	// Exposed ports grouped by protocol, in declaration order.
	byProto := make(map[string][]int)
	for _, e := range exposed {
		port, proto, err := ExposedPort(e)
		if err != nil {
			continue
		}
		byProto[proto] = append(byProto[proto], port)
	}

	next := make(map[string]int)
	for i := range ports {
		if ports[i].ContainerPort != 0 {
			continue
		}
		proto := ports[i].Protocol
		candidates := byProto[proto]
		idx := next[proto]
		if idx >= len(candidates) {
			// Nothing left for this protocol. Leave ContainerPort at 0; the
			// caller in run.go reports it as "-p N has no container port and
			// the image declares no EXPOSE", which is accurate and beats
			// inventing a mapping.
			continue
		}
		ports[i].ContainerPort = candidates[idx]
		next[proto] = idx + 1
	}
	return ports
}

// portsColumn renders the PORTS column for `docksmith ps`.
func portsColumn(ports []network.PortMapping) string {
	if len(ports) == 0 {
		return ""
	}
	parts := make([]string, 0, len(ports))
	for _, p := range ports {
		parts = append(parts, fmt.Sprintf("0.0.0.0:%d->%d/%s", p.HostPort, p.ContainerPort, p.Protocol))
	}
	return strings.Join(parts, ", ")
}
