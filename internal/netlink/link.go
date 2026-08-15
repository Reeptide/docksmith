package netlink

import (
	"bytes"
	"fmt"
	"syscall"
)

// Link is an existing network interface.
type Link struct {
	Index int32
	Name  string
	Flags uint32
}

// Up reports whether the interface is administratively up.
func (l Link) Up() bool { return l.Flags&syscall.IFF_UP != 0 }

// AddBridge creates a bridge interface.
//
// The type is carried in a nested IFLA_LINKINFO/IFLA_INFO_KIND attribute; the
// kernel has no separate "create a bridge" message.
func AddBridge(name string) error {
	return do(&Request{
		Type:  syscall.RTM_NEWLINK,
		Flags: syscall.NLM_F_CREATE | syscall.NLM_F_EXCL,
		Data:  encodeIfInfomsg(syscall.AF_UNSPEC, 0, 0, 0),
		Attrs: []Attr{
			StringAttr(syscall.IFLA_IFNAME, name),
			NestedAttr(syscall.IFLA_LINKINFO,
				StringAttr(IFLA_INFO_KIND, "bridge"),
			),
		},
	})
}

// AddVethPair creates a veth pair: two interfaces joined end to end, one of
// which is later moved into a container's network namespace.
//
// This is the most deeply nested message the package builds. The peer is
// described inside IFLA_LINKINFO → IFLA_INFO_DATA → VETH_INFO_PEER, and that
// innermost attribute's payload is a raw ifinfomsg struct followed by ordinary
// attributes rather than attributes alone — which is why Attr allows a raw
// Value and Children together.
func AddVethPair(name, peer string) error {
	peerSpec := Attr{
		Type:     VETH_INFO_PEER,
		Value:    encodeIfInfomsg(syscall.AF_UNSPEC, 0, 0, 0),
		Children: []Attr{StringAttr(syscall.IFLA_IFNAME, peer)},
	}

	return do(&Request{
		Type:  syscall.RTM_NEWLINK,
		Flags: syscall.NLM_F_CREATE | syscall.NLM_F_EXCL,
		Data:  encodeIfInfomsg(syscall.AF_UNSPEC, 0, 0, 0),
		Attrs: []Attr{
			StringAttr(syscall.IFLA_IFNAME, name),
			NestedAttr(syscall.IFLA_LINKINFO,
				StringAttr(IFLA_INFO_KIND, "veth"),
				NestedAttr(IFLA_INFO_DATA, peerSpec),
			),
		},
	})
}

// DeleteLink removes an interface. Deleting one end of a veth pair removes
// both.
func DeleteLink(index int32) error {
	return do(&Request{
		Type: syscall.RTM_DELLINK,
		Data: encodeIfInfomsg(syscall.AF_UNSPEC, index, 0, 0),
	})
}

// SetUp brings an interface up.
//
// Flags are a masked write: Change says which bits the kernel should look at,
// Flags supplies their values. Setting Flags without Change is a no-op, which
// is a silent and confusing failure.
func SetUp(index int32) error {
	return do(&Request{
		Type: syscall.RTM_NEWLINK,
		Data: encodeIfInfomsg(syscall.AF_UNSPEC, index, syscall.IFF_UP, syscall.IFF_UP),
	})
}

// SetMaster enslaves an interface to a bridge.
func SetMaster(index, master int32) error {
	return do(&Request{
		Type:  syscall.RTM_NEWLINK,
		Data:  encodeIfInfomsg(syscall.AF_UNSPEC, index, 0, 0),
		Attrs: []Attr{Uint32Attr(syscall.IFLA_MASTER, uint32(master))},
	})
}

// SetNsPid moves an interface into the network namespace of a process.
//
// This is why container setup needs a synchronisation barrier: the namespace
// is named by pid, so the container process must already exist, but the
// interface has to be in place before it starts running.
func SetNsPid(index int32, pid int) error {
	return do(&Request{
		Type:  syscall.RTM_NEWLINK,
		Data:  encodeIfInfomsg(syscall.AF_UNSPEC, index, 0, 0),
		Attrs: []Attr{Uint32Attr(syscall.IFLA_NET_NS_PID, uint32(pid))},
	})
}

// SetName renames an interface. The interface must be down.
func SetName(index int32, name string) error {
	return do(&Request{
		Type:  syscall.RTM_NEWLINK,
		Data:  encodeIfInfomsg(syscall.AF_UNSPEC, index, 0, 0),
		Attrs: []Attr{StringAttr(syscall.IFLA_IFNAME, name)},
	})
}

// SetMTU sets an interface's MTU.
func SetMTU(index int32, mtu int) error {
	return do(&Request{
		Type:  syscall.RTM_NEWLINK,
		Data:  encodeIfInfomsg(syscall.AF_UNSPEC, index, 0, 0),
		Attrs: []Attr{Uint32Attr(syscall.IFLA_MTU, uint32(mtu))},
	})
}

// LinkByName looks up an interface in the caller's network namespace.
//
// Implemented as a dump filtered in userspace rather than a targeted query:
// asking for a single link by name works on current kernels but has been
// inconsistent historically, and a dump of a handful of interfaces costs
// nothing.
func LinkByName(name string) (*Link, error) {
	links, err := ListLinks()
	if err != nil {
		return nil, err
	}
	for _, l := range links {
		if l.Name == name {
			return &l, nil
		}
	}
	return nil, fmt.Errorf("interface %q not found", name)
}

// LinkExists reports whether an interface is present.
func LinkExists(name string) bool {
	_, err := LinkByName(name)
	return err == nil
}

// ListLinks returns every interface in the caller's network namespace.
func ListLinks() ([]Link, error) {
	c, err := Dial()
	if err != nil {
		return nil, err
	}
	defer c.Close()

	payloads, err := c.Execute(&Request{
		Type:  syscall.RTM_GETLINK,
		Flags: syscall.NLM_F_DUMP,
		Data:  encodeIfInfomsg(syscall.AF_UNSPEC, 0, 0, 0),
	})
	if err != nil {
		return nil, err
	}

	var links []Link
	for _, p := range payloads {
		if len(p) < syscall.SizeofIfInfomsg {
			continue
		}
		l := Link{
			Index: int32(nativeEndian.Uint32(p[4:8])),
			Flags: nativeEndian.Uint32(p[8:12]),
		}
		attrs := parseAttrs(p[syscall.SizeofIfInfomsg:])
		if raw, ok := attrs[syscall.IFLA_IFNAME]; ok {
			l.Name = string(bytes.TrimRight(raw, "\x00"))
		}
		links = append(links, l)
	}
	return links, nil
}
