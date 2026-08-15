package netlink

import (
	"os"
	"testing"
)

// Listing links is read-only, so this exercises the socket, the dump loop and
// attribute parsing against the real kernel without needing root.
func TestListLinksAgainstKernel(t *testing.T) {
	links, err := ListLinks()
	if err != nil {
		t.Fatalf("ListLinks: %v", err)
	}
	if len(links) == 0 {
		t.Fatal("no interfaces returned; every namespace has at least loopback")
	}

	var foundLo bool
	for _, l := range links {
		if l.Name == "lo" {
			foundLo = true
			if l.Index != 1 {
				t.Errorf("loopback index = %d, want 1", l.Index)
			}
		}
	}
	if !foundLo {
		t.Errorf("loopback missing from %d interfaces: %v", len(links), names(links))
	}
}

// Regression test. Payloads used to alias the receive buffer, so a multi-part
// dump spanning more than one Recvfrom overwrote entries already collected —
// producing duplicated interfaces, blank names, and a missing loopback.
func TestListLinksReturnsDistinctInterfaces(t *testing.T) {
	links, err := ListLinks()
	if err != nil {
		t.Fatalf("ListLinks: %v", err)
	}

	seenIndex := make(map[int32]string)
	for _, l := range links {
		if l.Name == "" {
			t.Errorf("interface %d has an empty name (buffer aliasing?)", l.Index)
		}
		if l.Index == 0 {
			t.Error("interface index 0 is not valid")
		}
		if prev, dup := seenIndex[l.Index]; dup {
			t.Errorf("index %d returned twice (%q and %q)", l.Index, prev, l.Name)
		}
		seenIndex[l.Index] = l.Name
	}
}

func TestLinkByNameFindsLoopback(t *testing.T) {
	l, err := LinkByName("lo")
	if err != nil {
		t.Fatalf("LinkByName(lo): %v", err)
	}
	if l.Name != "lo" {
		t.Errorf("name = %q", l.Name)
	}
	if !l.Up() {
		t.Error("loopback should be up")
	}
}

func TestLinkByNameMissingInterface(t *testing.T) {
	if _, err := LinkByName("docksmith-no-such-if"); err == nil {
		t.Error("expected an error for a missing interface")
	}
	if LinkExists("docksmith-no-such-if") {
		t.Error("LinkExists should be false for a missing interface")
	}
}

func TestDialAndClose(t *testing.T) {
	c, err := Dial()
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	if c.nextSeq() == c.nextSeq() {
		t.Error("sequence numbers must not repeat")
	}
	if err := c.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
}

// Mutating operations need root, so they only assert that a permission failure
// surfaces as an error rather than being silently swallowed.
func TestMutationWithoutPrivilegesReportsAnError(t *testing.T) {
	if isRoot() {
		t.Skip("running as root; this checks the unprivileged failure path")
	}
	if err := AddBridge("docksmith-test-br"); err == nil {
		DeleteLinkByName(t, "docksmith-test-br")
		t.Error("creating a bridge without privileges should fail")
	}
}

func names(links []Link) []string {
	out := make([]string, 0, len(links))
	for _, l := range links {
		out = append(out, l.Name)
	}
	return out
}

func isRoot() bool { return os.Geteuid() == 0 }

// DeleteLinkByName is a test helper for cleaning up after an unexpected
// success.
func DeleteLinkByName(t *testing.T, name string) {
	t.Helper()
	if l, err := LinkByName(name); err == nil {
		DeleteLink(l.Index)
	}
}
