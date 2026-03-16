//go:build !linux

package main

import "net"

// stripeGSOProbe returns false on non-Linux platforms (GSO not supported).
func stripeGSOProbe(_ *net.UDPConn) bool { return false }

// stripeGSOBuildOOB is a no-op on non-Linux platforms.
func stripeGSOBuildOOB(_ uint16) []byte { return nil }

// stripeGSOIsError is a no-op on non-Linux platforms.
func stripeGSOIsError(_ error) bool { return false }
