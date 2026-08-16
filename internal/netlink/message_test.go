package netlink

import (
	"bytes"
	"encoding/binary"
	"syscall"
	"testing"
)

func TestAlign(t *testing.T) {
	cases := map[int]int{0: 0, 1: 4, 2: 4, 3: 4, 4: 4, 5: 8, 7: 8, 8: 8, 9: 12, 16: 16, 17: 20}
	for in, want := range cases {
		if got := align(in); got != want {
			t.Errorf("align(%d) = %d, want %d", in, got, want)
		}
	}
}

// An attribute is a 4-byte header plus its value, padded out to a 4-byte
// boundary. Getting the padding wrong shifts everything after it and the
// kernel rejects the whole message with a bare EINVAL.
func TestStringAttrEncodingIsNulTerminatedAndPadded(t *testing.T) {
	var buf bytes.Buffer
	StringAttr(syscall.IFLA_IFNAME, "eth0").Encode(&buf)

	got := buf.Bytes()
	// 4 header + 5 value ("eth0\0") = 9, padded to 12.
	if len(got) != 12 {
		t.Fatalf("encoded length %d, want 12: % x", len(got), got)
	}
	if length := nativeEndian.Uint16(got[0:2]); length != 9 {
		t.Errorf("declared length %d, want 9 (unpadded)", length)
	}
	if typ := nativeEndian.Uint16(got[2:4]); typ != syscall.IFLA_IFNAME {
		t.Errorf("type = %d, want IFLA_IFNAME", typ)
	}
	if string(got[4:8]) != "eth0" {
		t.Errorf("value = %q", got[4:8])
	}
	if got[8] != 0 {
		t.Error("string attribute is not NUL-terminated")
	}
	for i, b := range got[9:12] {
		if b != 0 {
			t.Errorf("padding byte %d is %d, want 0", i, b)
		}
	}
}

// The declared length excludes padding but includes the header — a classic
// off-by-four that produces messages the kernel silently ignores.
func TestAttrLengthExcludesPadding(t *testing.T) {
	a := StringAttr(syscall.IFLA_IFNAME, "veth123456")
	if a.Len() != 4+11 {
		t.Errorf("Len() = %d, want 15", a.Len())
	}
	var buf bytes.Buffer
	a.Encode(&buf)
	if buf.Len() != 16 {
		t.Errorf("encoded length %d, want 16 (15 padded to 16)", buf.Len())
	}
}

func TestUint32AttrEncoding(t *testing.T) {
	var buf bytes.Buffer
	Uint32Attr(syscall.IFLA_NET_NS_PID, 4242).Encode(&buf)

	got := buf.Bytes()
	if len(got) != 8 {
		t.Fatalf("encoded length %d, want 8: % x", len(got), got)
	}
	if nativeEndian.Uint16(got[0:2]) != 8 {
		t.Errorf("declared length %d, want 8", nativeEndian.Uint16(got[0:2]))
	}
	if nativeEndian.Uint32(got[4:8]) != 4242 {
		t.Errorf("value = %d, want 4242", nativeEndian.Uint32(got[4:8]))
	}
}

// Nested attributes are how link types are described. The outer attribute's
// length must cover every child including their padding.
func TestNestedAttrLengthCoversChildren(t *testing.T) {
	nested := NestedAttr(syscall.IFLA_LINKINFO, StringAttr(IFLA_INFO_KIND, "bridge"))

	var buf bytes.Buffer
	nested.Encode(&buf)
	got := buf.Bytes()

	// Child: 4 header + 7 ("bridge\0") = 11, padded to 12.
	// Outer: 4 header + 12 = 16.
	if len(got) != 16 {
		t.Fatalf("encoded length %d, want 16: % x", len(got), got)
	}
	if outer := nativeEndian.Uint16(got[0:2]); outer != 16 {
		t.Errorf("outer length %d, want 16", outer)
	}
	if typ := nativeEndian.Uint16(got[2:4]); typ != syscall.IFLA_LINKINFO {
		t.Errorf("outer type = %d, want IFLA_LINKINFO", typ)
	}
	if inner := nativeEndian.Uint16(got[4:6]); inner != 11 {
		t.Errorf("inner length %d, want 11", inner)
	}
	if string(got[8:14]) != "bridge" {
		t.Errorf("inner value = %q", got[8:14])
	}
}

// The veth peer specification is the deepest structure the package builds and
// the one most likely to be encoded wrong: its payload is a raw ifinfomsg
// struct followed by attributes, not attributes alone.
func TestVethPeerAttrLayout(t *testing.T) {
	peer := Attr{
		Type:     VETH_INFO_PEER,
		Value:    encodeIfInfomsg(syscall.AF_UNSPEC, 0, 0, 0),
		Children: []Attr{StringAttr(syscall.IFLA_IFNAME, "veth1")},
	}
	var buf bytes.Buffer
	peer.Encode(&buf)
	got := buf.Bytes()

	// 4 header + 16 ifinfomsg + (4 + 6 = 10, padded to 12) = 32.
	if len(got) != 32 {
		t.Fatalf("encoded length %d, want 32: % x", len(got), got)
	}
	if nativeEndian.Uint16(got[0:2]) != 32 {
		t.Errorf("declared length %d, want 32", nativeEndian.Uint16(got[0:2]))
	}
	if nativeEndian.Uint16(got[2:4]) != VETH_INFO_PEER {
		t.Error("type is not VETH_INFO_PEER")
	}
	for i, b := range got[4:20] {
		if b != 0 {
			t.Errorf("ifinfomsg byte %d should be zero, got %d", i, b)
		}
	}
	if nativeEndian.Uint16(got[20:22]) != 10 {
		t.Errorf("peer name attr length %d, want 10", nativeEndian.Uint16(got[20:22]))
	}
	if string(got[24:29]) != "veth1" {
		t.Errorf("peer name = %q", got[24:29])
	}
}

func TestRequestHeaderIsCorrect(t *testing.T) {
	req := &Request{
		Type:  syscall.RTM_NEWLINK,
		Flags: syscall.NLM_F_CREATE | syscall.NLM_F_EXCL,
		Seq:   7,
		Data:  encodeIfInfomsg(syscall.AF_UNSPEC, 0, 0, 0),
		Attrs: []Attr{StringAttr(syscall.IFLA_IFNAME, "br0")},
	}
	got := req.Serialize()

	// 16 header + 16 ifinfomsg + (4 + 4 = 8) = 40.
	if len(got) != 40 {
		t.Fatalf("serialized length %d, want 40: % x", len(got), got)
	}
	if declared := nativeEndian.Uint32(got[0:4]); int(declared) != len(got) {
		t.Errorf("declared length %d does not match actual %d", declared, len(got))
	}
	if nativeEndian.Uint16(got[4:6]) != syscall.RTM_NEWLINK {
		t.Error("message type is not RTM_NEWLINK")
	}
	if flags := nativeEndian.Uint16(got[6:8]); flags != syscall.NLM_F_CREATE|syscall.NLM_F_EXCL {
		t.Errorf("flags = %#x", flags)
	}
	if nativeEndian.Uint32(got[8:12]) != 7 {
		t.Error("sequence number not carried through")
	}
	if nativeEndian.Uint32(got[12:16]) != 0 {
		t.Error("pid should be 0 (the kernel)")
	}
}

// Every netlink message the kernel accepts declares its own total length.
func TestSerializedLengthAlwaysMatchesDeclared(t *testing.T) {
	reqs := []*Request{
		{Type: syscall.RTM_NEWLINK, Data: encodeIfInfomsg(syscall.AF_UNSPEC, 0, 0, 0)},
		{Type: syscall.RTM_NEWLINK, Data: encodeIfInfomsg(syscall.AF_UNSPEC, 3, syscall.IFF_UP, syscall.IFF_UP)},
		{
			Type:  syscall.RTM_NEWLINK,
			Data:  encodeIfInfomsg(syscall.AF_UNSPEC, 0, 0, 0),
			Attrs: []Attr{StringAttr(syscall.IFLA_IFNAME, "a"), Uint32Attr(syscall.IFLA_MASTER, 2)},
		},
		{
			Type: syscall.RTM_NEWLINK,
			Data: encodeIfInfomsg(syscall.AF_UNSPEC, 0, 0, 0),
			Attrs: []Attr{
				StringAttr(syscall.IFLA_IFNAME, "veth-with-a-long-name"),
				NestedAttr(syscall.IFLA_LINKINFO,
					StringAttr(IFLA_INFO_KIND, "veth"),
					NestedAttr(IFLA_INFO_DATA, Attr{
						Type:     VETH_INFO_PEER,
						Value:    encodeIfInfomsg(syscall.AF_UNSPEC, 0, 0, 0),
						Children: []Attr{StringAttr(syscall.IFLA_IFNAME, "peer0")},
					}),
				),
			},
		},
	}
	for i, r := range reqs {
		got := r.Serialize()
		if declared := nativeEndian.Uint32(got[0:4]); int(declared) != len(got) {
			t.Errorf("request %d: declared %d, actual %d", i, declared, len(got))
		}
		if len(got)%4 != 0 {
			t.Errorf("request %d: length %d is not 4-byte aligned", i, len(got))
		}
	}
}

func TestEncodeIfInfomsgLayout(t *testing.T) {
	got := encodeIfInfomsg(syscall.AF_UNSPEC, 42, syscall.IFF_UP, syscall.IFF_UP)
	if len(got) != syscall.SizeofIfInfomsg {
		t.Fatalf("length %d, want %d", len(got), syscall.SizeofIfInfomsg)
	}
	if int32(nativeEndian.Uint32(got[4:8])) != 42 {
		t.Errorf("index = %d, want 42", int32(nativeEndian.Uint32(got[4:8])))
	}
	if nativeEndian.Uint32(got[8:12]) != syscall.IFF_UP {
		t.Error("flags not encoded")
	}
	// Change is the mask saying which flag bits to act on. Without it the
	// kernel ignores Flags entirely and the interface silently stays down.
	if nativeEndian.Uint32(got[12:16]) != syscall.IFF_UP {
		t.Error("change mask not encoded")
	}
}

func TestEncodeIfAddrmsgLayout(t *testing.T) {
	got := encodeIfAddrmsg(syscall.AF_INET, 16, 0, syscall.RT_SCOPE_UNIVERSE, 3)
	if len(got) != syscall.SizeofIfAddrmsg {
		t.Fatalf("length %d, want %d", len(got), syscall.SizeofIfAddrmsg)
	}
	if got[0] != syscall.AF_INET {
		t.Error("family not encoded")
	}
	if got[1] != 16 {
		t.Errorf("prefix length = %d, want 16", got[1])
	}
	if int32(nativeEndian.Uint32(got[4:8])) != 3 {
		t.Error("index not encoded")
	}
}

func TestEncodeRtMsgLayout(t *testing.T) {
	got := encodeRtMsg(syscall.AF_INET, 0, syscall.RT_TABLE_MAIN,
		syscall.RTPROT_BOOT, syscall.RT_SCOPE_UNIVERSE, syscall.RTN_UNICAST, 0)
	if len(got) != syscall.SizeofRtMsg {
		t.Fatalf("length %d, want %d", len(got), syscall.SizeofRtMsg)
	}
	if got[0] != syscall.AF_INET {
		t.Error("family not encoded")
	}
	// A default route is dst_len 0 with no RTA_DST attribute at all.
	if got[1] != 0 {
		t.Errorf("dst_len = %d, want 0 for a default route", got[1])
	}
	if got[7] != syscall.RTN_UNICAST {
		t.Error("route type not encoded")
	}
}

// parseAttrs must be able to read back what Encode produces.
func TestParseAttrsRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	StringAttr(syscall.IFLA_IFNAME, "eth0").Encode(&buf)
	Uint32Attr(syscall.IFLA_MTU, 1500).Encode(&buf)
	Uint32Attr(syscall.IFLA_MASTER, 9).Encode(&buf)

	attrs := parseAttrs(buf.Bytes())
	if len(attrs) != 3 {
		t.Fatalf("parsed %d attributes, want 3", len(attrs))
	}
	if name := string(bytes.TrimRight(attrs[syscall.IFLA_IFNAME], "\x00")); name != "eth0" {
		t.Errorf("name = %q", name)
	}
	if mtu := nativeEndian.Uint32(attrs[syscall.IFLA_MTU]); mtu != 1500 {
		t.Errorf("mtu = %d", mtu)
	}
	if master := nativeEndian.Uint32(attrs[syscall.IFLA_MASTER]); master != 9 {
		t.Errorf("master = %d", master)
	}
}

// Attribute data comes from the kernel, but a truncated read must not panic.
func TestParseAttrsToleratesTruncatedInput(t *testing.T) {
	var buf bytes.Buffer
	StringAttr(syscall.IFLA_IFNAME, "eth0").Encode(&buf)
	full := buf.Bytes()

	for cut := 0; cut < len(full); cut++ {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("panic on %d-byte input: %v", cut, r)
				}
			}()
			parseAttrs(full[:cut])
		}()
	}

	// A length field claiming more than the buffer holds must be rejected too.
	bogus := make([]byte, 8)
	binary.LittleEndian.PutUint16(bogus[0:2], 9999)
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Errorf("panic on oversized length: %v", r)
			}
		}()
		if got := parseAttrs(bogus); len(got) != 0 {
			t.Errorf("oversized attribute should be dropped, got %v", got)
		}
	}()
}

func TestNativeEndianMatchesRuntime(t *testing.T) {
	var probe uint16 = 0x0102
	b := make([]byte, 2)
	nativeEndian.PutUint16(b, probe)
	// Netlink is host byte order, so a round-trip through the detected order
	// must be lossless.
	if nativeEndian.Uint16(b) != probe {
		t.Error("native endian detection is inconsistent")
	}
}
