package cmd

import (
	"os"
	"strings"
	"testing"

	"docksmith/internal/container"
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

// Two containers publishing the same host port is silently broken, not an
// error: both DNAT rules exist, the newer sits ahead of the older at the head
// of the chain, and the first container stops receiving traffic while `ps`
// still shows it owning the port. Nothing reports it — it reads as the
// application breaking.
func TestCheckHostPortConflictsRejectsAPortAnotherContainerHolds(t *testing.T) {
	root := t.TempDir()
	existing := &container.Record{
		ID:      "aaaa000000000000",
		Name:    "web",
		NetMode: NetBridge,
		Ports:   []network.PortMapping{{HostPort: 8080, ContainerPort: 80, Protocol: "tcp"}},
	}
	if err := container.Create(root, existing); err != nil {
		t.Fatal(err)
	}
	existing.MarkStarted(os.Getpid())
	if err := container.Save(existing); err != nil {
		t.Fatal(err)
	}

	want := []network.PortMapping{{HostPort: 8080, ContainerPort: 3000, Protocol: "tcp"}}
	err := checkHostPortConflicts(root, want, "")
	if err == nil {
		t.Fatal("publishing a host port another running container holds should be refused")
	}
	if !strings.Contains(err.Error(), "web") {
		t.Errorf("error should name the container holding the port, got: %v", err)
	}

	// A different protocol on the same number is not a conflict.
	udp := []network.PortMapping{{HostPort: 8080, ContainerPort: 80, Protocol: "udp"}}
	if err := checkHostPortConflicts(root, udp, ""); err != nil {
		t.Errorf("udp/8080 does not conflict with tcp/8080: %v", err)
	}

	// Neither is a free port.
	free := []network.PortMapping{{HostPort: 9090, ContainerPort: 80, Protocol: "tcp"}}
	if err := checkHostPortConflicts(root, free, ""); err != nil {
		t.Errorf("an unused port should be allowed: %v", err)
	}

	// The container's own record must not block its own restart.
	if err := checkHostPortConflicts(root, want, existing.ID); err != nil {
		t.Errorf("a container must not conflict with itself: %v", err)
	}
}

// An exited container releases its ports; its record still lists them.
func TestCheckHostPortConflictsIgnoresExitedContainers(t *testing.T) {
	root := t.TempDir()
	dead := &container.Record{
		ID:      "bbbb000000000000",
		Name:    "old",
		NetMode: NetBridge,
		Ports:   []network.PortMapping{{HostPort: 8080, ContainerPort: 80, Protocol: "tcp"}},
	}
	if err := container.Create(root, dead); err != nil {
		t.Fatal(err)
	}
	dead.MarkExited(0)
	if err := container.Save(dead); err != nil {
		t.Fatal(err)
	}

	ports := []network.PortMapping{{HostPort: 8080, ContainerPort: 80, Protocol: "tcp"}}
	if err := checkHostPortConflicts(root, ports, ""); err != nil {
		t.Errorf("an exited container must not hold its ports: %v", err)
	}
}
