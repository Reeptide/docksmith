// Package network sets up container networking: a host bridge, a veth pair per
// container, address allocation, NAT for outbound traffic, and DNAT for
// published ports.
package network

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"syscall"
)

// Defaults for the docksmith network. The subnet is deliberately unusual to
// avoid colliding with Docker's 172.17.0.0/16 or a home LAN's 192.168.x.
const (
	BridgeName  = "docksmith0"
	Subnet      = "172.30.0.0/16"
	GatewayIP   = "172.30.0.1"
	subnetMask  = 16
	firstHostIP = 2 // .1 is the bridge itself
)

// leases maps container id to allocated IP.
type leases struct {
	Entries map[string]string `json:"entries"`
}

// Dir returns the directory holding network state.
func Dir(stateRoot string) string { return filepath.Join(stateRoot, "network") }

func leasePath(stateRoot string) string { return filepath.Join(Dir(stateRoot), "ipam.json") }
func lockPath(stateRoot string) string  { return filepath.Join(Dir(stateRoot), ".lock") }

// withLock runs fn holding an exclusive lock on the IPAM state.
//
// Allocation is a read-modify-write of a single file, so two `docksmith run`
// invocations racing would otherwise hand the same address to both containers
// and neither would work. The lock lives in its own file so it is never
// truncated by the write it guards.
func withLock(stateRoot string, fn func() error) error {
	if err := os.MkdirAll(Dir(stateRoot), 0755); err != nil {
		return err
	}
	f, err := os.OpenFile(lockPath(stateRoot), os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("locking ipam: %w", err)
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return fn()
}

func loadLeases(stateRoot string) (*leases, error) {
	data, err := os.ReadFile(leasePath(stateRoot))
	if os.IsNotExist(err) {
		return &leases{Entries: map[string]string{}}, nil
	}
	if err != nil {
		return nil, err
	}
	var l leases
	if err := json.Unmarshal(data, &l); err != nil {
		// Deliberately fatal. Resetting to an empty set would forget every
		// lease and immediately re-hand those addresses out, so two live
		// containers end up with the same IP — and addresses are NOT
		// re-derivable, because the running containers still hold them.
		// Failing loudly is recoverable; silent duplicates are not.
		return nil, fmt.Errorf("lease file %s is corrupt (move it aside to reset): %w",
			leasePath(stateRoot), err)
	}
	if l.Entries == nil {
		l.Entries = map[string]string{}
	}
	return &l, nil
}

func saveLeases(stateRoot string, l *leases) error {
	data, err := json.MarshalIndent(l, "", "  ")
	if err != nil {
		return err
	}
	dir := Dir(stateRoot)
	tmp, err := os.CreateTemp(dir, "ipam.json.tmp-*")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	os.Chmod(tmp.Name(), 0644)
	return os.Rename(tmp.Name(), leasePath(stateRoot))
}

// Allocate reserves the lowest free address for a container and returns it in
// CIDR form.
func Allocate(stateRoot, containerID string) (string, error) {
	var result string
	err := withLock(stateRoot, func() error {
		l, err := loadLeases(stateRoot)
		if err != nil {
			return err
		}
		if existing, ok := l.Entries[containerID]; ok {
			result = fmt.Sprintf("%s/%d", existing, subnetMask)
			return nil
		}

		taken := make(map[string]bool, len(l.Entries))
		for _, ip := range l.Entries {
			taken[ip] = true
		}

		_, ipnet, err := net.ParseCIDR(Subnet)
		if err != nil {
			return err
		}
		base := binary.BigEndian.Uint32(ipnet.IP.To4())
		ones, bits := ipnet.Mask.Size()
		total := uint32(1) << uint(bits-ones)

		for offset := uint32(firstHostIP); offset < total-1; offset++ {
			candidate := make(net.IP, 4)
			binary.BigEndian.PutUint32(candidate, base+offset)
			s := candidate.String()
			if taken[s] {
				continue
			}
			l.Entries[containerID] = s
			if err := saveLeases(stateRoot, l); err != nil {
				return err
			}
			result = fmt.Sprintf("%s/%d", s, subnetMask)
			return nil
		}
		return fmt.Errorf("no free addresses left in %s", Subnet)
	})
	return result, err
}

// Release returns a container's address to the pool. Safe to call for a
// container that never had one.
func Release(stateRoot, containerID string) error {
	return withLock(stateRoot, func() error {
		l, err := loadLeases(stateRoot)
		if err != nil {
			return err
		}
		if _, ok := l.Entries[containerID]; !ok {
			return nil
		}
		delete(l.Entries, containerID)
		return saveLeases(stateRoot, l)
	})
}

// LeaseHolders returns every container id currently holding an address. Used
// by prune to find leases whose container no longer exists.
func LeaseHolders(stateRoot string) []string {
	l, err := loadLeases(stateRoot)
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(l.Entries))
	for id := range l.Entries {
		out = append(out, id)
	}
	return out
}

// Lookup returns a container's allocated address without its prefix, or "".
func Lookup(stateRoot, containerID string) string {
	l, err := loadLeases(stateRoot)
	if err != nil {
		return ""
	}
	return l.Entries[containerID]
}
