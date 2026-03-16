//go:build linux

package main

// stripe_txtime_linux.go — Kernel-level TX pacing via SO_TXTIME + sch_fq.
//
// SO_TXTIME (Linux ≥ 4.19) lets the application stamp each outgoing datagram
// with an Earliest Departure Time (EDT, nanosecond CLOCK_MONOTONIC).  The
// sch_fq qdisc holds the packet in the kernel until that instant, providing
// nanosecond-precision inter-packet pacing that is impossible with userspace
// timers (Go's time.Sleep granularity is ~1 ms on Linux).
//
// Additionally, SO_MAX_PACING_RATE is set on each socket so that sch_fq can
// internaly pace any packets that do not carry an explicit SCM_TXTIME cmsg
// (e.g. single-segment writes that skip the GSO path).
//
// Prerequisites:
//   - Kernel ≥ 4.19 (Debian 12 = 6.1, Ubuntu 24.04 = 6.8 — both OK)
//   - sch_fq qdisc on egress interfaces:
//       tc qdisc replace dev <wan> root fq
//   - NIC with TX checksum offload (same requirement as GSO)

import (
	"fmt"
	"net"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// ── Kernel constants not (yet) in x/sys/unix ──────────────────────────────

const (
	soTXTIME  = 61   // SOL_SOCKET level option
	scmTXTIME = 61   // cmsg type for sendmsg ancillary data (SCM_TXTIME == SO_TXTIME)
)

// sockTxtime is the kernel struct sock_txtime (include/uapi/linux/net_tstamp.h).
//
//	struct sock_txtime {
//	    __kernel_clockid_t clockid;  /* reference clock */
//	    __u32              flags;    /* SOF_TXTIME_* */
//	};
type sockTxtime struct {
	ClockID int32
	Flags   uint32
}

// ── Probe ──────────────────────────────────────────────────────────────────

// stripeTxtimeProbe tests SO_TXTIME support on a UDP socket.
// Returns true if the setsockopt succeeds (kernel ≥ 4.19 + qdisc OK).
func stripeTxtimeProbe(conn *net.UDPConn) bool {
	rawConn, err := conn.SyscallConn()
	if err != nil {
		return false
	}
	var serr error
	if err := rawConn.Control(func(fd uintptr) {
		spec := sockTxtime{
			ClockID: unix.CLOCK_MONOTONIC,
			Flags:   0, // no error reporting (SOF_TXTIME_REPORT_ERRORS = 1)
		}
		serr = setsockoptTxtime(int(fd), spec)
	}); err != nil {
		return false
	}
	return serr == nil
}

// ── Socket setup ───────────────────────────────────────────────────────────

// stripeTxtimeSetup enables SO_TXTIME + SO_MAX_PACING_RATE on a UDP socket.
//
// SO_TXTIME allows per-packet EDT via SCM_TXTIME cmsg in sendmsg.
// SO_MAX_PACING_RATE sets the sch_fq flow rate so that any packet without
// an explicit SCM_TXTIME is still paced at the target rate.
//
// rateBytesPerSec is the target pacing rate for this individual socket.
// Pass 0 to skip SO_MAX_PACING_RATE (use only explicit EDT).
func stripeTxtimeSetup(conn *net.UDPConn, rateBytesPerSec uint64) error {
	rawConn, err := conn.SyscallConn()
	if err != nil {
		return fmt.Errorf("SyscallConn: %w", err)
	}
	var serr error
	if err := rawConn.Control(func(fd uintptr) {
		// 1. Enable SO_TXTIME with CLOCK_MONOTONIC
		spec := sockTxtime{
			ClockID: unix.CLOCK_MONOTONIC,
			Flags:   0,
		}
		if serr = setsockoptTxtime(int(fd), spec); serr != nil {
			return
		}
		// 2. Set SO_MAX_PACING_RATE (bytes/sec) so sch_fq paces the flow
		if rateBytesPerSec > 0 {
			serr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET,
				unix.SO_MAX_PACING_RATE, int(rateBytesPerSec))
		}
	}); err != nil {
		return fmt.Errorf("Control: %w", err)
	}
	return serr
}

// ── Per-packet OOB (cmsg) builders ─────────────────────────────────────────

// stripeTxtimeBuildOOB builds a standalone SCM_TXTIME cmsg carrying an EDT
// (nanoseconds since CLOCK_MONOTONIC epoch).
func stripeTxtimeBuildOOB(txtimeNs int64) []byte {
	const dataLen = 8 // uint64 nanoseconds
	b := make([]byte, unix.CmsgSpace(dataLen))
	h := (*unix.Cmsghdr)(unsafe.Pointer(&b[0]))
	h.Level = syscall.SOL_SOCKET
	h.Type = scmTXTIME
	h.SetLen(unix.CmsgLen(dataLen))
	*(*int64)(unsafe.Pointer(&b[unix.CmsgSpace(0)])) = txtimeNs
	return b
}

// stripeTxtimeAppendOOB appends SCM_TXTIME to existing OOB data (e.g. after
// a UDP_SEGMENT cmsg for GSO).  Returns the extended slice.
func stripeTxtimeAppendOOB(oob []byte, txtimeNs int64) []byte {
	const dataLen = 8
	startLen := len(oob)
	oob = append(oob, make([]byte, unix.CmsgSpace(dataLen))...)
	h := (*unix.Cmsghdr)(unsafe.Pointer(&oob[startLen]))
	h.Level = syscall.SOL_SOCKET
	h.Type = scmTXTIME
	h.SetLen(unix.CmsgLen(dataLen))
	*(*int64)(unsafe.Pointer(&oob[startLen+unix.CmsgSpace(0)])) = txtimeNs
	return oob
}

// ── Helpers ────────────────────────────────────────────────────────────────

func setsockoptTxtime(fd int, spec sockTxtime) error {
	_, _, errno := syscall.RawSyscall6(
		syscall.SYS_SETSOCKOPT,
		uintptr(fd),
		uintptr(syscall.SOL_SOCKET),
		uintptr(soTXTIME),
		uintptr(unsafe.Pointer(&spec)),
		uintptr(unsafe.Sizeof(spec)),
		0,
	)
	if errno != 0 {
		return errno
	}
	return nil
}

// monoNowNs returns the current CLOCK_MONOTONIC time in nanoseconds.
// This is the same clock SO_TXTIME references, so EDT values computed
// from this base are directly usable as SCM_TXTIME timestamps.
func monoNowNs() int64 {
	var ts unix.Timespec
	_ = unix.ClockGettime(unix.CLOCK_MONOTONIC, &ts)
	return ts.Sec*1e9 + ts.Nsec
}
