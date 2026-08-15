package netlink

import (
	"fmt"
	"net"
	"syscall"
)

// AddDefaultRoute points all unmatched traffic at a gateway.
//
// A default route is expressed by omitting RTA_DST entirely and setting
// dst_len to 0 — there is no explicit 0.0.0.0/0 attribute.
func AddDefaultRoute(gateway string, oif int32) error {
	gw := net.ParseIP(gateway)
	if gw == nil {
		return fmt.Errorf("parsing gateway %q", gateway)
	}
	v4 := gw.To4()
	if v4 == nil {
		return fmt.Errorf("gateway %q is not IPv4", gateway)
	}

	return do(&Request{
		Type:  syscall.RTM_NEWROUTE,
		Flags: syscall.NLM_F_CREATE | syscall.NLM_F_EXCL,
		Data: encodeRtMsg(syscall.AF_INET, 0, syscall.RT_TABLE_MAIN,
			syscall.RTPROT_BOOT, syscall.RT_SCOPE_UNIVERSE, syscall.RTN_UNICAST, 0),
		Attrs: []Attr{
			BytesAttr(syscall.RTA_GATEWAY, v4),
			Uint32Attr(syscall.RTA_OIF, uint32(oif)),
		},
	})
}

// AddRoute adds a route to a subnet reachable directly on an interface, with
// no gateway. Scope is RT_SCOPE_LINK: the destination is on the same segment,
// which is what makes the route valid without a next hop.
func AddRoute(cidr string, oif int32) error {
	_, ipnet, err := net.ParseCIDR(cidr)
	if err != nil {
		return fmt.Errorf("parsing route %q: %w", cidr, err)
	}
	v4 := ipnet.IP.To4()
	if v4 == nil {
		return fmt.Errorf("route %q is not IPv4", cidr)
	}
	prefixLen, _ := ipnet.Mask.Size()

	return do(&Request{
		Type:  syscall.RTM_NEWROUTE,
		Flags: syscall.NLM_F_CREATE | syscall.NLM_F_EXCL,
		Data: encodeRtMsg(syscall.AF_INET, uint8(prefixLen), syscall.RT_TABLE_MAIN,
			syscall.RTPROT_BOOT, syscall.RT_SCOPE_LINK, syscall.RTN_UNICAST, 0),
		Attrs: []Attr{
			BytesAttr(syscall.RTA_DST, v4),
			Uint32Attr(syscall.RTA_OIF, uint32(oif)),
		},
	})
}
