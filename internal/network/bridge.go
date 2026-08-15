package network

import (
	"fmt"
	"os"

	"docksmith/internal/netlink"
)

// vethNames derives the two ends of a container's veth pair from its id.
//
// Interface names are limited to 15 characters, so the id is truncated hard.
// The host end is prefixed to make it obvious in `ip link` on the host which
// container an interface belongs to.
func vethNames(containerID string) (host, peer string) {
	short := containerID
	if len(short) > 8 {
		short = short[:8]
	}
	return "dsv" + short, "dsp" + short
}

// EnsureBridge creates and configures docksmith0 if it does not already exist.
// Safe to call on every container start.
func EnsureBridge() error {
	if !netlink.LinkExists(BridgeName) {
		if err := netlink.AddBridge(BridgeName); err != nil {
			return fmt.Errorf("creating bridge %s: %w", BridgeName, err)
		}
	}

	br, err := netlink.LinkByName(BridgeName)
	if err != nil {
		return fmt.Errorf("looking up bridge %s: %w", BridgeName, err)
	}

	hasAddr, err := netlink.AddrExists(br.Index)
	if err != nil {
		return fmt.Errorf("checking bridge address: %w", err)
	}
	if !hasAddr {
		if err := netlink.AddAddr(br.Index, GatewayIP+"/16"); err != nil {
			return fmt.Errorf("addressing bridge: %w", err)
		}
	}
	if !br.Up() {
		if err := netlink.SetUp(br.Index); err != nil {
			return fmt.Errorf("bringing up bridge: %w", err)
		}
	}

	if err := enableForwarding(); err != nil {
		return err
	}
	return nil
}

// enableForwarding turns on the kernel switches container traffic depends on.
func enableForwarding() error {
	// Without ip_forward the host will not route between the bridge and the
	// outside world at all, so containers can reach the gateway and nothing
	// beyond it.
	if err := writeSysctl("/proc/sys/net/ipv4/ip_forward", "1"); err != nil {
		return err
	}

	// route_localnet allows 127.0.0.1 as a source address for traffic leaving
	// via the bridge. Published ports rely on it: OUTPUT-chain DNAT rewrites
	// the destination to a container address while the source stays 127.0.0.1,
	// and the kernel drops that as a martian by default. Without this,
	// `-p 8080:80` works from the LAN address but not from localhost, which is
	// exactly how everyone tests it. Docker solves the same problem with a
	// userspace proxy.
	path := fmt.Sprintf("/proc/sys/net/ipv4/conf/%s/route_localnet", BridgeName)
	if err := writeSysctl(path, "1"); err != nil {
		// Non-fatal: only published-port access via localhost suffers.
		fmt.Fprintf(os.Stderr, "warning: %v\n", err)
	}
	return nil
}

func writeSysctl(path, value string) error {
	if err := os.WriteFile(path, []byte(value), 0644); err != nil {
		return fmt.Errorf("setting %s=%s: %w", path, value, err)
	}
	return nil
}

// AttachContainer creates a veth pair, enslaves the host end to the bridge and
// moves the other end into the container's network namespace.
//
// Called from the parent while the container waits on the sync barrier: the
// namespace is identified by pid, so the process must exist, but the interface
// must be in place before it runs.
func AttachContainer(containerID string, pid int) error {
	hostName, peerName := vethNames(containerID)

	if err := netlink.AddVethPair(hostName, peerName); err != nil {
		return fmt.Errorf("creating veth pair: %w", err)
	}

	// From here on, failures must clean up the pair or the host accumulates
	// orphaned interfaces. Deleting either end removes both.
	cleanup := func() {
		if l, err := netlink.LinkByName(hostName); err == nil {
			netlink.DeleteLink(l.Index)
		}
	}

	br, err := netlink.LinkByName(BridgeName)
	if err != nil {
		cleanup()
		return fmt.Errorf("looking up bridge: %w", err)
	}
	host, err := netlink.LinkByName(hostName)
	if err != nil {
		cleanup()
		return fmt.Errorf("looking up %s: %w", hostName, err)
	}
	if err := netlink.SetMaster(host.Index, br.Index); err != nil {
		cleanup()
		return fmt.Errorf("enslaving %s to %s: %w", hostName, BridgeName, err)
	}
	if err := netlink.SetUp(host.Index); err != nil {
		cleanup()
		return fmt.Errorf("bringing up %s: %w", hostName, err)
	}

	peer, err := netlink.LinkByName(peerName)
	if err != nil {
		cleanup()
		return fmt.Errorf("looking up %s: %w", peerName, err)
	}
	// Once this succeeds the peer is no longer visible from the host namespace,
	// so any later failure is the container's to report.
	if err := netlink.SetNsPid(peer.Index, pid); err != nil {
		cleanup()
		return fmt.Errorf("moving %s into netns of pid %d: %w", peerName, pid, err)
	}
	return nil
}

// DetachContainer removes a container's host-side interface. Normally the
// kernel does this automatically when the network namespace goes away; this
// covers the case where setup failed partway.
func DetachContainer(containerID string) {
	hostName, _ := vethNames(containerID)
	if l, err := netlink.LinkByName(hostName); err == nil {
		netlink.DeleteLink(l.Index)
	}
}

// PeerName returns the interface name a container's veth end arrives with.
func PeerName(containerID string) string {
	_, peer := vethNames(containerID)
	return peer
}

// EnableLoopback brings up lo. Every container gets this, including ones with
// no external connectivity, because software routinely assumes 127.0.0.1 works.
func EnableLoopback() error {
	lo, err := netlink.LinkByName("lo")
	if err != nil {
		return fmt.Errorf("loopback not found: %w", err)
	}
	if lo.Up() {
		return nil
	}
	return netlink.SetUp(lo.Index)
}

// ConfigureInside runs in the container's network namespace and brings its
// interfaces up. The peer arrives under the name PeerName returns and is
// renamed to eth0, so the environment inside looks conventional.
func ConfigureInside(peerName, ip, gateway string, mtu int) error {
	if err := EnableLoopback(); err != nil {
		return err
	}

	peer, err := netlink.LinkByName(peerName)
	if err != nil {
		return fmt.Errorf("container interface %s not found: %w", peerName, err)
	}

	// Rename before bringing it up: the kernel rejects renaming a running
	// interface.
	if err := netlink.SetName(peer.Index, "eth0"); err != nil {
		return fmt.Errorf("renaming %s to eth0: %w", peerName, err)
	}
	if mtu > 0 {
		netlink.SetMTU(peer.Index, mtu)
	}
	if err := netlink.AddAddr(peer.Index, ip); err != nil {
		return fmt.Errorf("addressing eth0 with %s: %w", ip, err)
	}
	if err := netlink.SetUp(peer.Index); err != nil {
		return fmt.Errorf("bringing up eth0: %w", err)
	}
	if gateway != "" {
		if err := netlink.AddDefaultRoute(gateway, peer.Index); err != nil {
			return fmt.Errorf("adding default route via %s: %w", gateway, err)
		}
	}
	return nil
}
