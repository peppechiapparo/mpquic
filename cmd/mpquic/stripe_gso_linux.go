//go:build linux

package main

// stripe_gso_linux.go — UDP Generic Segmentation Offload (GSO) helpers.
//
// GSO (UDP_SEGMENT) allows the kernel to accept one large buffer and split it
// into MTU-sized UDP datagrams in the network stack, turning N sendmsg calls
// into 1. This dramatically reduces syscall overhead on the TX hot path.
//
// Requires Linux ≥ 5.0 and NIC with TX checksum offload.

import (
	"errors"
	"net"
	"os"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// stripeGSOProbe tests whether a UDP socket supports GSO (UDP_SEGMENT).
// Returns false on kernels < 5.0 or if the socket option is not available.
func stripeGSOProbe(conn *net.UDPConn) bool {
	rawConn, err := conn.SyscallConn()
	if err != nil {
		return false
	}

	// Parse kernel major version
	var uname syscall.Utsname
	if err := syscall.Uname(&uname); err != nil {
		return false
	}
	var major int
	for _, c := range uname.Release {
		if c >= '0' && c <= '9' {
			major = major*10 + int(c-'0')
		} else {
			break
		}
	}
	if major < 5 {
		return false
	}

	// Test UDP_SEGMENT support on socket
	var serr error
	if err := rawConn.Control(func(fd uintptr) {
		_, serr = unix.GetsockoptInt(int(fd), unix.IPPROTO_UDP, unix.UDP_SEGMENT)
	}); err != nil {
		return false
	}
	return serr == nil
}

// stripeGSOBuildOOB constructs the OOB ancillary data (cmsg) that tells the
// kernel to split the payload into segments of segSize bytes each.
func stripeGSOBuildOOB(segSize uint16) []byte {
	const dataLen = 2 // payload is a uint16
	b := make([]byte, unix.CmsgSpace(dataLen))
	h := (*unix.Cmsghdr)(unsafe.Pointer(&b[0]))
	h.Level = syscall.IPPROTO_UDP
	h.Type = unix.UDP_SEGMENT
	h.SetLen(unix.CmsgLen(dataLen))
	*(*uint16)(unsafe.Pointer(&b[unix.CmsgSpace(0)])) = segSize
	return b
}

// stripeGSOIsError returns true if the error indicates the NIC does not
// support TX checksum offload, which is a hard requirement for UDP_SEGMENT.
// When this happens, GSO should be permanently disabled for this socket.
func stripeGSOIsError(err error) bool {
	var serr *os.SyscallError
	if errors.As(err, &serr) {
		// EIO is returned by udp_send_skb() when the device driver lacks
		// tx checksum offload. See man 7 udp and kernel source.
		return serr.Err == unix.EIO
	}
	return false
}
