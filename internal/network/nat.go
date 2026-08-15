package network

import (
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// Everything else in this package speaks netlink directly. Packet filtering
// does not: netfilter rules go over a different netlink family with a
// substantially larger wire format, and reimplementing it would be a project in
// itself rather than a part of one. So NAT shells out to iptables, and this
// file is the only place docksmith depends on an external binary.

// Rule is a single iptables rule, recorded on the container so teardown can
// delete exactly what was added rather than pattern-matching a live ruleset
// that other tools are also editing.
type Rule struct {
	Table string   `json:"table"`
	Chain string   `json:"chain"`
	Args  []string `json:"args"`
}

// PortMapping is a published port: hostPort -> containerPort.
type PortMapping struct {
	HostPort      int    `json:"hostPort"`
	ContainerPort int    `json:"containerPort"`
	Protocol      string `json:"protocol"` // tcp or udp
}

func (p PortMapping) String() string {
	return fmt.Sprintf("%d->%d/%s", p.HostPort, p.ContainerPort, p.Protocol)
}

// ParsePortMapping parses "hostPort:containerPort" or
// "hostPort:containerPort/proto".
func ParsePortMapping(spec string) (PortMapping, error) {
	proto := "tcp"
	if base, p, found := strings.Cut(spec, "/"); found {
		spec = base
		proto = strings.ToLower(p)
		if proto != "tcp" && proto != "udp" {
			return PortMapping{}, fmt.Errorf("invalid port %q: protocol must be tcp or udp", spec)
		}
	}

	hostStr, ctrStr, ok := strings.Cut(spec, ":")
	if !ok {
		// A bare host port means "publish to whatever the image EXPOSEs".
		// ContainerPort 0 is a placeholder the caller fills in; it is not a
		// valid port, so it cannot survive to the iptables rule by accident.
		host, err := parsePort(hostStr)
		if err != nil {
			return PortMapping{}, fmt.Errorf("invalid port %q: %w", spec, err)
		}
		return PortMapping{HostPort: host, ContainerPort: 0, Protocol: proto}, nil
	}
	host, err := parsePort(hostStr)
	if err != nil {
		return PortMapping{}, fmt.Errorf("invalid port %q: %w", spec, err)
	}
	ctr, err := parsePort(ctrStr)
	if err != nil {
		return PortMapping{}, fmt.Errorf("invalid port %q: %w", spec, err)
	}
	return PortMapping{HostPort: host, ContainerPort: ctr, Protocol: proto}, nil
}

func parsePort(s string) (int, error) {
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("%q is not a number", s)
	}
	if n < 1 || n > 65535 {
		return 0, fmt.Errorf("port %d out of range", n)
	}
	return n, nil
}

// iptables runs one iptables invocation.
func iptables(args ...string) error {
	out, err := exec.Command("iptables", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("iptables %s: %w: %s",
			strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// ruleExists reports whether a rule is already installed.
//
// iptables -C exits 1 when the rule is absent, but also exits 1 for some other
// failures such as a missing chain, and this host runs the nf_tables backend
// where that conflation is real. Any non-zero exit is therefore treated as
// "absent" — worst case a duplicate insert is attempted and fails loudly,
// which is far better than the alternative of never inserting at all.
func ruleExists(table, chain string, args []string) bool {
	full := append([]string{"-t", table, "-C", chain}, args...)
	return exec.Command("iptables", full...).Run() == nil
}

// ensureRule inserts a rule if it is not already present.
//
// Insertion position matters. Docker, if installed, puts its own rules at the
// head of FORWARD and commonly leaves that chain's policy set to DROP, so a
// rule appended with -A is never reached and containers get an address they
// cannot route from. -I puts docksmith's accepts ahead of that.
func ensureRule(table, chain string, args []string) (*Rule, error) {
	if ruleExists(table, chain, args) {
		return &Rule{Table: table, Chain: chain, Args: args}, nil
	}
	full := append([]string{"-t", table, "-I", chain}, args...)
	if err := iptables(full...); err != nil {
		return nil, err
	}
	return &Rule{Table: table, Chain: chain, Args: args}, nil
}

// deleteRule removes a rule, ignoring the case where it is already gone.
func deleteRule(r Rule) error {
	if !ruleExists(r.Table, r.Chain, r.Args) {
		return nil
	}
	full := append([]string{"-t", r.Table, "-D", r.Chain}, r.Args...)
	return iptables(full...)
}

// EnsureNAT installs the host-wide rules containers need for outbound traffic.
// Idempotent, so it runs on every container start.
func EnsureNAT() error {
	rules := []struct {
		table, chain string
		args         []string
	}{
		// Masquerade traffic leaving the subnet for anywhere but the bridge, so
		// replies come back to this host.
		{"nat", "POSTROUTING", []string{"-s", Subnet, "!", "-o", BridgeName, "-j", "MASQUERADE"}},
		// Let containers reach the outside world, and let replies return.
		{"filter", "FORWARD", []string{"-i", BridgeName, "-j", "ACCEPT"}},
		{"filter", "FORWARD", []string{"-o", BridgeName, "-m", "conntrack",
			"--ctstate", "RELATED,ESTABLISHED", "-j", "ACCEPT"}},
	}
	for _, r := range rules {
		if _, err := ensureRule(r.table, r.chain, r.args); err != nil {
			return err
		}
	}
	return nil
}

// PublishPorts installs DNAT rules for a container's published ports and
// returns them for later teardown.
func PublishPorts(containerIP string, ports []PortMapping) ([]Rule, error) {
	var installed []Rule
	for _, p := range ports {
		dest := fmt.Sprintf("%s:%d", containerIP, p.ContainerPort)
		hostPort := strconv.Itoa(p.HostPort)

		specs := []struct {
			chain string
			args  []string
		}{
			// Traffic arriving from outside the host.
			{"PREROUTING", []string{"-p", p.Protocol, "--dport", hostPort,
				"-j", "DNAT", "--to-destination", dest}},
			// Traffic originating on the host itself, including 127.0.0.1 —
			// which is why route_localnet is enabled on the bridge.
			{"OUTPUT", []string{"-p", p.Protocol, "--dport", hostPort,
				"-j", "DNAT", "--to-destination", dest}},
		}
		for _, s := range specs {
			rule, err := ensureRule("nat", s.chain, s.args)
			if err != nil {
				UnpublishPorts(installed)
				return nil, fmt.Errorf("publishing %s: %w", p, err)
			}
			installed = append(installed, *rule)
		}

		// Masquerade traffic that came from the host's loopback.
		//
		// DNAT rewrites only the destination, so a connection to 127.0.0.1
		// arrives at the container still carrying source 127.0.0.1. The
		// container's reply then goes to its own loopback and is lost — the
		// connection hangs rather than failing, which is worse. Rewriting the
		// source to the bridge address gives the reply somewhere to go, and
		// conntrack maps it back on the way out.
		//
		// This is the piece Docker sidesteps entirely by proxying localhost
		// connections in userspace with docker-proxy.
		snat, err := ensureRule("nat", "POSTROUTING", []string{
			"-s", "127.0.0.0/8", "-d", containerIP, "-p", p.Protocol,
			"--dport", strconv.Itoa(p.ContainerPort), "-j", "MASQUERADE"})
		if err != nil {
			UnpublishPorts(installed)
			return nil, fmt.Errorf("publishing %s: %w", p, err)
		}
		installed = append(installed, *snat)

		// Allow the forwarded traffic through to the container.
		fwd, err := ensureRule("filter", "FORWARD", []string{
			"-o", BridgeName, "-p", p.Protocol, "-d", containerIP,
			"--dport", strconv.Itoa(p.ContainerPort), "-j", "ACCEPT"})
		if err != nil {
			UnpublishPorts(installed)
			return nil, fmt.Errorf("publishing %s: %w", p, err)
		}
		installed = append(installed, *fwd)
	}
	return installed, nil
}

// UnpublishPorts removes rules recorded by PublishPorts.
func UnpublishPorts(rules []Rule) {
	for _, r := range rules {
		deleteRule(r)
	}
}

// HasIptables reports whether the iptables binary is available.
func HasIptables() bool {
	_, err := exec.LookPath("iptables")
	return err == nil
}
