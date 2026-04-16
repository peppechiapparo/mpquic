package main

import (
	"context"
	"net"
	"strings"
	"time"
)

const ipWatchInterval = 10 * time.Second

// watchInterfaceIP monitors the IPv4 address of a network interface.
// When the IP changes (including becoming unavailable), it calls onChange
// with the new IP (or "" if no IP). The goroutine exits when ctx is cancelled.
func watchInterfaceIP(ctx context.Context, ifaceBind string, currentIP string, onChange func(newIP string)) {
	if !strings.HasPrefix(ifaceBind, "if:") {
		return // only watch interface-based bindings
	}
	ifName := strings.TrimPrefix(ifaceBind, "if:")

	ticker := time.NewTicker(ipWatchInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			newIP := getInterfaceIPv4(ifName)
			if newIP != currentIP && newIP != "" {
				// IP changed to a different valid address (e.g. CGNAT reassignment)
				onChange(newIP)
				return
			}
			// If newIP=="" the interface temporarily lost its IP (DHCP renewal);
			// wait for it to come back rather than triggering a needless reconnect.
		}
	}
}

// getInterfaceIPv4 returns the first non-loopback IPv4 address on the named
// interface, or "" if the interface doesn't exist or has no IPv4.
func getInterfaceIPv4(ifName string) string {
	iface, err := net.InterfaceByName(ifName)
	if err != nil {
		return ""
	}
	addrs, err := iface.Addrs()
	if err != nil {
		return ""
	}
	for _, addr := range addrs {
		ipNet, ok := addr.(*net.IPNet)
		if !ok {
			continue
		}
		ip := ipNet.IP.To4()
		if ip == nil || ip.IsLoopback() {
			continue
		}
		return ip.String()
	}
	return ""
}
