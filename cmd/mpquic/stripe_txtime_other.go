//go:build !linux

package main

import (
	"net"
	"time"
)

// stripe_txtime_other.go — stubs for non-Linux platforms.
// SO_TXTIME / SCM_TXTIME / sch_fq are Linux-specific.

func stripeTxtimeProbe(_ *net.UDPConn) bool             { return false }
func stripeTxtimeSetup(_ *net.UDPConn, _ uint64) error   { return nil }
func stripeTxtimeBuildOOB(_ int64) []byte                { return nil }
func stripeTxtimeAppendOOB(oob []byte, _ int64) []byte   { return oob }
func monoNowNs() int64                                   { return time.Now().UnixNano() }
