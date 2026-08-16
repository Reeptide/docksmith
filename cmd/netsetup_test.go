package cmd

import (
	"testing"

	"docksmith/internal/network"
)

// A bare -p must resolve against an EXPOSE of the *same* protocol, and must
// never have its own protocol rewritten. Consulting exposed[0] unconditionally
// published `-p 5353/udp` as tcp/80, producing a DNAT rule that could not match
// a packet the container would ever see.
func TestApplyExposedDefaultsMatchesProtocol(t *testing.T) {
	ports := []network.PortMapping{
		{HostPort: 5353, ContainerPort: 0, Protocol: "udp"},
		{HostPort: 8080, ContainerPort: 0, Protocol: "tcp"},
	}
	got := applyExposedDefaults(ports, []string{"80/tcp", "53/udp"})

	if got[0].Protocol != "udp" || got[0].ContainerPort != 53 {
		t.Errorf("-p 5353/udp resolved to %d/%s, want 53/udp",
			got[0].ContainerPort, got[0].Protocol)
	}
	if got[1].Protocol != "tcp" || got[1].ContainerPort != 80 {
		t.Errorf("-p 8080 resolved to %d/%s, want 80/tcp",
			got[1].ContainerPort, got[1].Protocol)
	}
}

// Two bare ports must consume two EXPOSE entries. Mapping both to the first one
// produced two DNAT rules for the same destination, and parsePortArgs' duplicate
// check runs before this so nothing caught it.
func TestApplyExposedDefaultsConsumesSuccessiveEntries(t *testing.T) {
	ports := []network.PortMapping{
		{HostPort: 8080, Protocol: "tcp"},
		{HostPort: 9090, Protocol: "tcp"},
	}
	got := applyExposedDefaults(ports, []string{"80", "443"})
	if got[0].ContainerPort != 80 || got[1].ContainerPort != 443 {
		t.Errorf("got %d and %d, want 80 and 443", got[0].ContainerPort, got[1].ContainerPort)
	}
}

// An explicit -p is never touched, whatever the image declares.
func TestApplyExposedDefaultsLeavesExplicitMappingsAlone(t *testing.T) {
	ports := []network.PortMapping{{HostPort: 8080, ContainerPort: 3000, Protocol: "tcp"}}
	got := applyExposedDefaults(ports, []string{"80/tcp"})
	if got[0].ContainerPort != 3000 {
		t.Errorf("explicit mapping was rewritten to %d", got[0].ContainerPort)
	}
}

// Running out of EXPOSE entries leaves ContainerPort at 0, which run.go reports
// as an error naming the port. Inventing a mapping would be worse.
func TestApplyExposedDefaultsLeavesUnmatchedPortsUnresolved(t *testing.T) {
	ports := []network.PortMapping{
		{HostPort: 8080, Protocol: "tcp"},
		{HostPort: 9090, Protocol: "tcp"},
	}
	got := applyExposedDefaults(ports, []string{"80"})
	if got[0].ContainerPort != 80 {
		t.Errorf("first port = %d, want 80", got[0].ContainerPort)
	}
	if got[1].ContainerPort != 0 {
		t.Errorf("second port = %d, want 0 so the caller can report it", got[1].ContainerPort)
	}
}
