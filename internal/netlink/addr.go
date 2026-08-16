package netlink

import (
	"fmt"
	"net"
	"syscall"
)

// AddAddr assigns an IPv4 address to an interface.
//
// IFA_LOCAL and IFA_ADDRESS are both sent and both equal here. They differ only
// on point-to-point links, where IFA_ADDRESS is the peer; omitting IFA_LOCAL on
// an ordinary interface leaves the address unusable as a source.
//
// The broadcast address is also set, because a bridge with no broadcast
// address will not answer ARP for the subnet, and the symptom — containers
// that can reach nothing at all — gives no hint as to why.
func AddAddr(index int32, cidr string) error {
	ip, ipnet, err := net.ParseCIDR(cidr)
	if err != nil {
		return fmt.Errorf("parsing address %q: %w", cidr, err)
	}
	v4 := ip.To4()
	if v4 == nil {
		return fmt.Errorf("address %q is not IPv4", cidr)
	}
	prefixLen, _ := ipnet.Mask.Size()

	broadcast := make(net.IP, len(ipnet.IP.To4()))
	copy(broadcast, ipnet.IP.To4())
	for i := range broadcast {
		broadcast[i] |= ^ipnet.Mask[i]
	}

	return do(&Request{
		Type:  syscall.RTM_NEWADDR,
		Flags: syscall.NLM_F_CREATE | syscall.NLM_F_EXCL,
		Data: encodeIfAddrmsg(syscall.AF_INET, uint8(prefixLen), 0,
			syscall.RT_SCOPE_UNIVERSE, index),
		Attrs: []Attr{
			BytesAttr(syscall.IFA_LOCAL, v4),
			BytesAttr(syscall.IFA_ADDRESS, v4),
			BytesAttr(syscall.IFA_BROADCAST, broadcast),
		},
	})
}

// AddrExists reports whether an interface already carries an IPv4 address.
// Used to make bridge setup idempotent.
func AddrExists(index int32) (bool, error) {
	c, err := Dial()
	if err != nil {
		return false, err
	}
	defer c.Close()

	payloads, err := c.Execute(&Request{
		Type:  syscall.RTM_GETADDR,
		Flags: syscall.NLM_F_DUMP,
		Data:  encodeIfAddrmsg(syscall.AF_INET, 0, 0, 0, 0),
	})
	if err != nil {
		return false, err
	}
	for _, p := range payloads {
		if len(p) < syscall.SizeofIfAddrmsg {
			continue
		}
		if int32(nativeEndian.Uint32(p[4:8])) == index {
			return true, nil
		}
	}
	return false, nil
}
