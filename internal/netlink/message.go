// Package netlink is a minimal hand-rolled rtnetlink client.
//
// It talks to the kernel over an AF_NETLINK socket directly rather than
// shelling out to iproute2, which keeps docksmith dependency-free and puts the
// interface configuration in the same place as everything else it does with
// namespaces.
//
// Only what container networking needs is implemented: creating bridges and
// veth pairs, moving an interface into another network namespace, renaming,
// bringing links up, assigning addresses, and adding routes.
//
// Constants and C struct layouts come from the standard library's syscall
// package; the handful it does not export are defined here.
package netlink

import (
	"bytes"
	"encoding/binary"
	"syscall"
	"unsafe"
)

// Attribute types absent from package syscall.
const (
	IFLA_INFO_UNSPEC = 0
	IFLA_INFO_KIND   = 1
	IFLA_INFO_DATA   = 2

	VETH_INFO_UNSPEC = 0
	VETH_INFO_PEER   = 1
)

// nativeEndian is the byte order netlink uses: the host's, not network order.
var nativeEndian binary.ByteOrder = func() binary.ByteOrder {
	var i uint16 = 1
	if (*[2]byte)(unsafe.Pointer(&i))[0] == 1 {
		return binary.LittleEndian
	}
	return binary.BigEndian
}()

// align rounds n up to a 4-byte boundary. Every netlink header and attribute
// is padded to this, and getting it wrong shifts everything that follows —
// the kernel then rejects the message with EINVAL and no explanation.
func align(n int) int {
	return (n + syscall.NLMSG_ALIGNTO - 1) & ^(syscall.NLMSG_ALIGNTO - 1)
}

// Attr is a netlink attribute: a type/length/value triple.
//
// Attributes nest, which is how link-type-specific configuration is expressed.
// Creating a veth pair means IFLA_LINKINFO containing IFLA_INFO_KIND ("veth")
// and IFLA_INFO_DATA, which in turn contains VETH_INFO_PEER describing the
// other end. Value and Children can both be set; Value is written first, which
// VETH_INFO_PEER needs because its payload is a raw ifinfomsg struct followed
// by ordinary attributes.
type Attr struct {
	Type     uint16
	Value    []byte
	Children []Attr
}

// Len is the attribute's encoded length including its header, before padding.
func (a Attr) Len() int {
	n := syscall.SizeofRtAttr + len(a.Value)
	for _, c := range a.Children {
		n += align(c.Len())
	}
	return n
}

// Encode appends the attribute, padded, to buf.
func (a Attr) Encode(buf *bytes.Buffer) {
	length := a.Len()
	binary.Write(buf, nativeEndian, uint16(length))
	binary.Write(buf, nativeEndian, a.Type)
	buf.Write(a.Value)
	for _, c := range a.Children {
		c.Encode(buf)
	}
	// Pad the whole attribute out to the alignment boundary.
	for i := length; i < align(length); i++ {
		buf.WriteByte(0)
	}
}

// Uint32Attr builds an attribute holding a single uint32, used for interface
// indexes and pids.
func Uint32Attr(typ uint16, v uint32) Attr {
	b := make([]byte, 4)
	nativeEndian.PutUint32(b, v)
	return Attr{Type: typ, Value: b}
}

// StringAttr builds a NUL-terminated string attribute. The kernel expects the
// terminator; omitting it yields names with trailing garbage.
func StringAttr(typ uint16, s string) Attr {
	return Attr{Type: typ, Value: append([]byte(s), 0)}
}

// BytesAttr builds an attribute from raw bytes, such as an IP address.
func BytesAttr(typ uint16, b []byte) Attr {
	return Attr{Type: typ, Value: b}
}

// NestedAttr builds an attribute whose payload is other attributes.
func NestedAttr(typ uint16, children ...Attr) Attr {
	return Attr{Type: typ, Children: children}
}

// Request is one netlink message: a header, a fixed-size family struct, and a
// list of attributes.
type Request struct {
	Type  uint16 // RTM_NEWLINK and friends
	Flags uint16
	Seq   uint32
	Data  []byte // ifinfomsg, ifaddrmsg or rtmsg, already encoded
	Attrs []Attr
}

// Serialize renders the request into the exact bytes sent to the kernel.
func (r *Request) Serialize() []byte {
	var body bytes.Buffer
	body.Write(r.Data)
	for _, a := range r.Attrs {
		a.Encode(&body)
	}

	total := syscall.NLMSG_HDRLEN + body.Len()
	var buf bytes.Buffer
	binary.Write(&buf, nativeEndian, uint32(total))
	binary.Write(&buf, nativeEndian, r.Type)
	binary.Write(&buf, nativeEndian, r.Flags)
	binary.Write(&buf, nativeEndian, r.Seq)
	binary.Write(&buf, nativeEndian, uint32(0)) // pid: 0 means the kernel
	buf.Write(body.Bytes())
	return buf.Bytes()
}

// encodeIfInfomsg renders an ifinfomsg, the fixed header for link messages.
func encodeIfInfomsg(family uint8, index int32, flags, change uint32) []byte {
	buf := make([]byte, syscall.SizeofIfInfomsg)
	buf[0] = family
	buf[1] = 0 // padding
	nativeEndian.PutUint16(buf[2:], 0)
	nativeEndian.PutUint32(buf[4:], uint32(index))
	nativeEndian.PutUint32(buf[8:], flags)
	nativeEndian.PutUint32(buf[12:], change)
	return buf
}

// encodeIfAddrmsg renders an ifaddrmsg, the fixed header for address messages.
func encodeIfAddrmsg(family, prefixLen, flags, scope uint8, index int32) []byte {
	buf := make([]byte, syscall.SizeofIfAddrmsg)
	buf[0] = family
	buf[1] = prefixLen
	buf[2] = flags
	buf[3] = scope
	nativeEndian.PutUint32(buf[4:], uint32(index))
	return buf
}

// encodeRtMsg renders an rtmsg, the fixed header for route messages.
func encodeRtMsg(family, dstLen, table, protocol, scope, rtType uint8, flags uint32) []byte {
	buf := make([]byte, syscall.SizeofRtMsg)
	buf[0] = family
	buf[1] = dstLen
	buf[2] = 0 // src_len
	buf[3] = 0 // tos
	buf[4] = table
	buf[5] = protocol
	buf[6] = scope
	buf[7] = rtType
	nativeEndian.PutUint32(buf[8:], flags)
	return buf
}

// parseAttrs walks the attribute list following a fixed-size header.
func parseAttrs(b []byte) map[uint16][]byte {
	out := make(map[uint16][]byte)
	for len(b) >= syscall.SizeofRtAttr {
		length := nativeEndian.Uint16(b[0:2])
		typ := nativeEndian.Uint16(b[2:4])
		if int(length) < syscall.SizeofRtAttr || int(length) > len(b) {
			break
		}
		out[typ] = b[syscall.SizeofRtAttr:length]
		step := align(int(length))
		if step > len(b) {
			break
		}
		b = b[step:]
	}
	return out
}
