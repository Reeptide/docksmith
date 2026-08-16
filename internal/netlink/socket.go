package netlink

import (
	"fmt"
	"os"
	"sync/atomic"
	"syscall"
)

// Conn is a netlink socket.
//
// Deliberately not safe for concurrent use: sequence numbers and the read
// buffer are per-connection, so callers take a fresh Conn rather than sharing
// one. They are cheap — one socket, opened and closed around a few messages.
type Conn struct {
	fd  int
	seq uint32
}

// Dial opens a NETLINK_ROUTE socket bound to this process.
func Dial() (*Conn, error) {
	fd, err := syscall.Socket(syscall.AF_NETLINK, syscall.SOCK_RAW|syscall.SOCK_CLOEXEC, syscall.NETLINK_ROUTE)
	if err != nil {
		return nil, fmt.Errorf("netlink socket: %w", err)
	}
	// Pid 0 lets the kernel assign the port id, which avoids clashing with any
	// other netlink socket this process already holds.
	addr := &syscall.SockaddrNetlink{Family: syscall.AF_NETLINK}
	if err := syscall.Bind(fd, addr); err != nil {
		syscall.Close(fd)
		return nil, fmt.Errorf("netlink bind: %w", err)
	}
	return &Conn{fd: fd}, nil
}

func (c *Conn) Close() error { return syscall.Close(c.fd) }

// nextSeq returns a fresh sequence number for matching replies to requests.
func (c *Conn) nextSeq() uint32 { return atomic.AddUint32(&c.seq, 1) }

// Execute sends a request and waits for the kernel's acknowledgement,
// returning any payload messages. NLM_F_REQUEST and NLM_F_ACK are added
// automatically — without ACK the kernel stays silent on success and there is
// no way to tell a working call from a silently dropped one.
func (c *Conn) Execute(req *Request) ([][]byte, error) {
	req.Seq = c.nextSeq()
	req.Flags |= syscall.NLM_F_REQUEST | syscall.NLM_F_ACK

	if err := syscall.Sendto(c.fd, req.Serialize(), 0, &syscall.SockaddrNetlink{Family: syscall.AF_NETLINK}); err != nil {
		return nil, fmt.Errorf("netlink send: %w", err)
	}
	return c.receive(req.Seq)
}

// receive reads until the acknowledgement or end of a dump.
func (c *Conn) receive(seq uint32) ([][]byte, error) {
	var payloads [][]byte
	buf := make([]byte, 64*1024) // comfortably above the 32KB netlink limit

	for {
		n, _, err := syscall.Recvfrom(c.fd, buf, 0)
		if err != nil {
			if err == syscall.EINTR {
				continue
			}
			return nil, fmt.Errorf("netlink receive: %w", err)
		}
		if n < syscall.NLMSG_HDRLEN {
			return nil, fmt.Errorf("netlink: short message (%d bytes)", n)
		}

		msgs, err := syscall.ParseNetlinkMessage(buf[:n])
		if err != nil {
			return nil, fmt.Errorf("netlink parse: %w", err)
		}

		for _, m := range msgs {
			if m.Header.Seq != seq {
				continue // a reply to something else on this socket
			}
			switch m.Header.Type {
			case syscall.NLMSG_DONE:
				return payloads, nil
			case syscall.NLMSG_ERROR:
				// Payload is a negative errno followed by the offending header.
				// Zero means this is a plain acknowledgement, not a failure.
				if len(m.Data) < 4 {
					return nil, fmt.Errorf("netlink: malformed error message")
				}
				errno := int32(nativeEndian.Uint32(m.Data[0:4]))
				if errno == 0 {
					return payloads, nil
				}
				return nil, os.NewSyscallError("netlink", syscall.Errno(-errno))
			default:
				// Copy: m.Data aliases the receive buffer, which the next
				// Recvfrom of a multi-part dump overwrites in place. Retaining
				// the slice corrupts payloads already collected.
				payloads = append(payloads, append([]byte(nil), m.Data...))
				// A dump ends with NLMSG_DONE; a single reply does not, so stop
				// once the multi-part flag is absent.
				if m.Header.Flags&syscall.NLM_F_MULTI == 0 {
					return payloads, nil
				}
			}
		}
	}
}

// do opens a connection, runs one request, and closes it.
func do(req *Request) error {
	c, err := Dial()
	if err != nil {
		return err
	}
	defer c.Close()
	_, err = c.Execute(req)
	return err
}
