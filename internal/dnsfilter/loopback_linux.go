//go:build linux

package dnsfilter

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"

	"golang.org/x/sys/unix"
)

type loopbackAddress struct {
	address string
	family  uint8
	prefix  uint8
}

func ensureLoopbackDNSAddresses() error {
	loopback, err := net.InterfaceByName("lo")
	if err != nil {
		return fmt.Errorf("find Linux loopback interface: %w", err)
	}
	fd, err := unix.Socket(unix.AF_NETLINK, unix.SOCK_RAW|unix.SOCK_CLOEXEC, unix.NETLINK_ROUTE)
	if err != nil {
		return fmt.Errorf("open route netlink socket: %w", err)
	}
	defer unix.Close(fd)
	if err := unix.Bind(fd, &unix.SockaddrNetlink{Family: unix.AF_NETLINK}); err != nil {
		return fmt.Errorf("bind route netlink socket: %w", err)
	}
	for i, item := range []loopbackAddress{
		{address: IPv4Address, family: unix.AF_INET, prefix: 32},
		{address: IPv6Address, family: unix.AF_INET6, prefix: 128},
	} {
		ip := net.ParseIP(item.address)
		if item.family == unix.AF_INET {
			ip = ip.To4()
		} else {
			ip = ip.To16()
		}
		if ip == nil {
			return fmt.Errorf("invalid UCD DNS address %q", item.address)
		}
		if err := replaceAddress(fd, loopback.Index, item, ip, uint32(i+1)); err != nil {
			return fmt.Errorf("assign %s/%d to Linux loopback: %w", item.address, item.prefix, err)
		}
	}
	return nil
}

func replaceAddress(fd, ifindex int, address loopbackAddress, ip net.IP, sequence uint32) error {
	message := make([]byte, unix.SizeofNlMsghdr+unix.SizeofIfAddrmsg)
	message[unix.SizeofNlMsghdr] = address.family
	message[unix.SizeofNlMsghdr+1] = address.prefix
	message[unix.SizeofNlMsghdr+3] = unix.RT_SCOPE_HOST
	binary.NativeEndian.PutUint32(message[unix.SizeofNlMsghdr+4:unix.SizeofNlMsghdr+8], uint32(ifindex))
	if address.family == unix.AF_INET {
		message = appendAddressAttribute(message, unix.IFA_LOCAL, ip)
	}
	message = appendAddressAttribute(message, unix.IFA_ADDRESS, ip)
	binary.NativeEndian.PutUint32(message[0:4], uint32(len(message)))
	binary.NativeEndian.PutUint16(message[4:6], unix.RTM_NEWADDR)
	binary.NativeEndian.PutUint16(message[6:8], unix.NLM_F_REQUEST|unix.NLM_F_ACK|unix.NLM_F_CREATE|unix.NLM_F_REPLACE)
	binary.NativeEndian.PutUint32(message[8:12], sequence)
	if err := unix.Sendto(fd, message, 0, &unix.SockaddrNetlink{Family: unix.AF_NETLINK}); err != nil {
		return fmt.Errorf("send address assignment request: %w", err)
	}
	response := make([]byte, 4096)
	for {
		n, _, err := unix.Recvfrom(fd, response, 0)
		if err != nil {
			return fmt.Errorf("read address assignment response: %w", err)
		}
		for offset := 0; offset+unix.SizeofNlMsghdr <= n; {
			length := int(binary.NativeEndian.Uint32(response[offset : offset+4]))
			if length < unix.SizeofNlMsghdr || offset+length > n {
				return errors.New("invalid route netlink response length")
			}
			messageType := binary.NativeEndian.Uint16(response[offset+4 : offset+6])
			messageSequence := binary.NativeEndian.Uint32(response[offset+8 : offset+12])
			if messageSequence == sequence && messageType == unix.NLMSG_ERROR {
				if length < unix.SizeofNlMsghdr+4 {
					return errors.New("truncated route netlink error response")
				}
				code := int32(binary.NativeEndian.Uint32(response[offset+unix.SizeofNlMsghdr : offset+unix.SizeofNlMsghdr+4]))
				if code < 0 {
					return unix.Errno(-code)
				}
				if code != 0 {
					return fmt.Errorf("unexpected positive route netlink status %d", code)
				}
				return nil
			}
			offset += (length + 3) &^ 3
		}
	}
}

func appendAddressAttribute(message []byte, attributeType uint16, value []byte) []byte {
	length := unix.SizeofRtAttr + len(value)
	aligned := (length + 3) &^ 3
	start := len(message)
	message = append(message, make([]byte, aligned)...)
	binary.NativeEndian.PutUint16(message[start:start+2], uint16(length))
	binary.NativeEndian.PutUint16(message[start+2:start+4], attributeType)
	copy(message[start+unix.SizeofRtAttr:start+length], value)
	return message
}
