package network

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestAllocateReturnsAddressesInSubnet(t *testing.T) {
	root := t.TempDir()
	_, subnet, err := net.ParseCIDR(Subnet)
	if err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 10; i++ {
		cidr, err := Allocate(root, fmt.Sprintf("container-%d", i))
		if err != nil {
			t.Fatal(err)
		}
		ip, _, err := net.ParseCIDR(cidr)
		if err != nil {
			t.Fatalf("Allocate returned %q, which is not valid CIDR: %v", cidr, err)
		}
		if !subnet.Contains(ip) {
			t.Errorf("%s is outside %s", ip, Subnet)
		}
		if ip.String() == GatewayIP {
			t.Error("allocated the gateway address to a container")
		}
	}
}

func TestAllocateIsStableForTheSameContainer(t *testing.T) {
	root := t.TempDir()
	first, err := Allocate(root, "abc")
	if err != nil {
		t.Fatal(err)
	}
	second, err := Allocate(root, "abc")
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Errorf("re-allocating gave a different address: %s then %s", first, second)
	}
}

func TestAllocateDoesNotReuseLiveAddresses(t *testing.T) {
	root := t.TempDir()
	seen := make(map[string]string)
	for i := 0; i < 50; i++ {
		id := fmt.Sprintf("c%d", i)
		cidr, err := Allocate(root, id)
		if err != nil {
			t.Fatal(err)
		}
		if prev, dup := seen[cidr]; dup {
			t.Fatalf("%s handed to both %s and %s", cidr, prev, id)
		}
		seen[cidr] = id
	}
}

func TestReleaseFreesAddressForReuse(t *testing.T) {
	root := t.TempDir()
	first, err := Allocate(root, "a")
	if err != nil {
		t.Fatal(err)
	}
	if err := Release(root, "a"); err != nil {
		t.Fatal(err)
	}
	if got := Lookup(root, "a"); got != "" {
		t.Errorf("Lookup after Release = %q, want empty", got)
	}
	// The freed address is the lowest free one, so it comes back next.
	second, err := Allocate(root, "b")
	if err != nil {
		t.Fatal(err)
	}
	if second != first {
		t.Errorf("released address %s was not reused (got %s)", first, second)
	}
}

func TestReleaseUnknownContainerIsNotAnError(t *testing.T) {
	if err := Release(t.TempDir(), "never-existed"); err != nil {
		t.Errorf("releasing an unknown container should be a no-op: %v", err)
	}
}

// Allocation is a read-modify-write of one file. Without the flock, two
// concurrent `docksmith run` invocations would hand the same address to both
// containers and neither would work.
func TestAllocateIsSafeUnderConcurrency(t *testing.T) {
	root := t.TempDir()
	const n = 30

	var mu sync.Mutex
	results := make(map[string]string)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := fmt.Sprintf("c%d", i)
			cidr, err := Allocate(root, id)
			if err != nil {
				t.Errorf("Allocate: %v", err)
				return
			}
			mu.Lock()
			defer mu.Unlock()
			if prev, dup := results[cidr]; dup {
				t.Errorf("%s allocated twice: %s and %s", cidr, prev, id)
			}
			results[cidr] = id
		}(i)
	}
	wg.Wait()
	if len(results) != n {
		t.Errorf("got %d distinct addresses, want %d", len(results), n)
	}
}

// A corrupt lease file must be a hard error.
//
// The previous behaviour was to reset to an empty lease set and carry on, which
// meant every live container's address was forgotten and immediately re-handed
// out — two containers with the same IP and no diagnostic. This test asserted
// only that allocation did not error, so it passed while the invariant it was
// supposed to protect was violated.
func TestCorruptLeaseFileIsAnError(t *testing.T) {
	root := t.TempDir()
	first, err := Allocate(root, "a")
	if err != nil {
		t.Fatal(err)
	}

	for _, corruption := range []string{"{not json", "", "[]"} {
		if err := os.WriteFile(leasePath(root), []byte(corruption), 0644); err != nil {
			t.Fatal(err)
		}
		second, err := Allocate(root, "b")
		if err == nil {
			t.Errorf("corruption %q: expected an error, got address %s", corruption, second)
			if second == first {
				t.Errorf("  and it duplicated the live address %s", first)
			}
		}
	}
}

// The property the old test should have checked: whatever happens, two
// containers never hold the same address.
func TestAllocateNeverDuplicatesALiveAddress(t *testing.T) {
	root := t.TempDir()
	first, err := Allocate(root, "a")
	if err != nil {
		t.Fatal(err)
	}
	second, err := Allocate(root, "b")
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatalf("two containers got %s", first)
	}
}

func TestVethNamesFitKernelLimit(t *testing.T) {
	// Interface names are capped at 15 characters; a longer one is rejected
	// with an unhelpful EINVAL from the kernel.
	host, peer := vethNames(strings.Repeat("a", 64))
	if len(host) > 15 {
		t.Errorf("host name %q is %d chars, limit is 15", host, len(host))
	}
	if len(peer) > 15 {
		t.Errorf("peer name %q is %d chars, limit is 15", peer, len(peer))
	}
	if host == peer {
		t.Error("both ends of the veth pair got the same name")
	}
}

func TestVethNamesAreDistinctPerContainer(t *testing.T) {
	a, _ := vethNames("aaaaaaaaaaaa")
	b, _ := vethNames("bbbbbbbbbbbb")
	if a == b {
		t.Error("different containers produced the same interface name")
	}
}

func TestParsePortMapping(t *testing.T) {
	cases := []struct {
		spec string
		want PortMapping
	}{
		{"8080:80", PortMapping{HostPort: 8080, ContainerPort: 80, Protocol: "tcp"}},
		{"53:53/udp", PortMapping{HostPort: 53, ContainerPort: 53, Protocol: "udp"}},
		{"8080:80/tcp", PortMapping{HostPort: 8080, ContainerPort: 80, Protocol: "tcp"}},
		// A bare host port defers the container port to the image's EXPOSE.
		{"8080", PortMapping{HostPort: 8080, ContainerPort: 0, Protocol: "tcp"}},
	}
	for _, c := range cases {
		got, err := ParsePortMapping(c.spec)
		if err != nil {
			t.Errorf("ParsePortMapping(%q): %v", c.spec, err)
			continue
		}
		if got != c.want {
			t.Errorf("ParsePortMapping(%q) = %+v, want %+v", c.spec, got, c.want)
		}
	}
}

func TestParsePortMappingRejectsBadSpecs(t *testing.T) {
	for _, bad := range []string{
		"", "abc", "0:80", "80:0", "65536:80", "80:65536",
		"-1:80", "8080:80/sctp", "8080:80:90",
	} {
		if _, err := ParsePortMapping(bad); err == nil {
			t.Errorf("ParsePortMapping(%q) should fail", bad)
		}
	}
}

func TestPortMappingString(t *testing.T) {
	p := PortMapping{HostPort: 8080, ContainerPort: 80, Protocol: "tcp"}
	if got := p.String(); got != "8080->80/tcp" {
		t.Errorf("String() = %q", got)
	}
}

// A loopback resolver in the host's resolv.conf points at a stub listener in
// the host's network namespace. Copied into a container, 127.0.0.1 is the
// container itself, where nothing is listening — DNS then times out silently
// instead of failing loudly.
//
// Driven from fixed input rather than the host's real /etc/resolv.conf. Reading
// the real file makes the test's outcome depend on the machine: under
// systemd-resolved it contains only 127.0.0.53, so the assertion inside the
// loop is reached for zero addresses and the test passes no matter what the
// filter does.
func TestParseNameserversSkipsLoopback(t *testing.T) {
	const resolvConf = `# Generated by something
nameserver 127.0.0.53
nameserver 8.8.8.8
nameserver ::1
options edns0 trust-ad
nameserver 192.168.1.1
search lan
nameserver
`
	got := parseNameservers(strings.NewReader(resolvConf))
	want := []string{"8.8.8.8", "192.168.1.1"}
	if len(got) != len(want) {
		t.Fatalf("parseNameservers = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("parseNameservers = %v, want %v", got, want)
		}
	}
}

// The order in resolv.conf is the query order, so a filter that reorders
// entries silently changes which resolver a container asks first.
func TestParseNameserversPreservesOrder(t *testing.T) {
	got := parseNameservers(strings.NewReader(
		"nameserver 1.1.1.1\nnameserver 8.8.8.8\n"))
	if len(got) != 2 || got[0] != "1.1.1.1" || got[1] != "8.8.8.8" {
		t.Errorf("parseNameservers = %v, want [1.1.1.1 8.8.8.8]", got)
	}
}

// With every resolver filtered out there is nothing usable left, and the
// container falls back to a public resolver rather than shipping an empty
// resolv.conf that fails every lookup.
func TestParseNameserversReturnsNothingWhenAllAreLoopback(t *testing.T) {
	if got := parseNameservers(strings.NewReader("nameserver 127.0.0.53\n")); len(got) != 0 {
		t.Errorf("parseNameservers = %v, want empty so the fallback applies", got)
	}
}

func TestWriteHostFilesGeneratesAllThree(t *testing.T) {
	rootfs := t.TempDir()
	if err := WriteHostFiles(rootfs, "abc123", "172.30.0.5"); err != nil {
		t.Fatal(err)
	}

	resolv := readFile(t, filepath.Join(rootfs, "etc/resolv.conf"))
	if !strings.Contains(resolv, "nameserver") {
		t.Errorf("resolv.conf has no nameserver:\n%s", resolv)
	}
	for _, line := range strings.Split(resolv, "\n") {
		if strings.HasPrefix(line, "nameserver 127.") {
			t.Errorf("resolv.conf contains a loopback resolver: %q", line)
		}
	}

	if got := strings.TrimSpace(readFile(t, filepath.Join(rootfs, "etc/hostname"))); got != "abc123" {
		t.Errorf("hostname = %q", got)
	}

	hosts := readFile(t, filepath.Join(rootfs, "etc/hosts"))
	if !strings.Contains(hosts, "127.0.0.1") {
		t.Error("hosts is missing localhost")
	}
	if !strings.Contains(hosts, "172.30.0.5\tabc123") {
		t.Errorf("hosts is missing the container's own entry:\n%s", hosts)
	}
}

func TestWriteHostFilesCreatesEtcWhenImageHasNone(t *testing.T) {
	rootfs := t.TempDir()
	if err := WriteHostFiles(rootfs, "h", "172.30.0.9"); err != nil {
		t.Fatalf("should create /etc when the image lacks one: %v", err)
	}
	if _, err := os.Stat(filepath.Join(rootfs, "etc/resolv.conf")); err != nil {
		t.Error(err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
