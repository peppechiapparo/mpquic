package main

import (
	"context"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/songgao/water"
)

func runTunnel(ctx context.Context, cfg *Config, conn datagramConn, logger *Logger) error {
	tun, _, err := openTUN(cfg.TunName, logger)
	if err != nil {
		return err
	}
	var tunCloseOnce sync.Once
	closeTun := func() { tunCloseOnce.Do(func() { tun.Close() }) }
	defer closeTun()

	return runTunnelWithTUN(ctx, conn, tun, closeTun, logger)
}

func runTunnelWithTUN(ctx context.Context, conn datagramConn, tun *water.Interface, closeTun func(), logger *Logger) error {

	errCh := make(chan error, 2)

	// Close TUN device when context is cancelled to unblock tun.Read goroutine.
	// This runs BEFORE the deferred tun.Close() in the caller, ensuring the
	// read goroutine doesn't stay blocked during shutdown.
	go func() {
		<-ctx.Done()
		closeTun()
	}()

	go func() {
		buf := make([]byte, 65535)
		for {
			n, err := tun.Read(buf)
			if err != nil {
				// Check context first — if shutting down, exit silently
				if ctx.Err() != nil {
					return
				}
				errCh <- err
				return
			}
			// Check context before sending — don't attempt write on closed conn
			if ctx.Err() != nil {
				return
			}
			pkt := append([]byte(nil), buf[:n]...)
			if err := conn.SendDatagram(pkt); err != nil {
				if ctx.Err() != nil {
					return
				}
				errCh <- err
				return
			}
			logger.Debugf("TX %d bytes", n)
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-errCh:
			return err
		default:
		}
		pkt, err := conn.ReceiveDatagram(ctx)
		if err != nil {
			return err
		}
		if _, err := tun.Write(pkt); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}
		logger.Debugf("RX %d bytes", len(pkt))
	}
}

// expandMultipathPipes expands path configs with pipes > 1 into N individual
// path entries. Each pipe gets a unique name (e.g. "wan5.0", "wan5.1") but
// shares the same bind IP, remote address, priority, and weight.
// Each pipe will open its own UDP socket (different source port), creating
// separate UDP sessions that bypass per-session traffic shaping (e.g. Starlink).
func expandMultipathPipes(paths []MultipathPathConfig, cfg *Config, logger *Logger) []MultipathPathConfig {
	var expanded []MultipathPathConfig
	for _, p := range paths {
		effectiveTransport := resolvePathTransport(p, cfg, logger)

		// Stripe transport: do NOT expand pipes — stripeClientConn manages its
		// own N UDP sockets internally. The path stays as a single entry.
		if effectiveTransport == "stripe" {
			p.BasePath = p.Name
			if p.Pipes <= 0 {
				p.Pipes = cfg.StarlinkDefaultPipes
				if p.Pipes <= 0 {
					p.Pipes = 4
				}
			}
			logger.Infof("stripe path=%s pipes=%d (managed internally)", p.Name, p.Pipes)
			expanded = append(expanded, p)
			continue
		}

		// QUIC transport: expand pipes as before
		pipes := p.Pipes
		// Auto-detect Starlink if configured and pipes not explicitly set
		if pipes <= 0 && cfg.DetectStarlink {
			if detectStarlink(p.BindIP, logger) {
				pipes = cfg.StarlinkDefaultPipes
				if pipes <= 0 {
					pipes = 4
				}
				logger.Infof("starlink detected path=%s auto_pipes=%d", p.Name, pipes)
			}
		}
		if pipes <= 1 {
			p.BasePath = p.Name
			expanded = append(expanded, p)
			continue
		}
		logger.Infof("expanding path=%s into %d pipes", p.Name, pipes)
		for i := 0; i < pipes; i++ {
			ep := p
			ep.Name = fmt.Sprintf("%s.%d", p.Name, i)
			ep.BasePath = p.Name
			ep.Pipes = 1
			expanded = append(expanded, ep)
		}
	}
	return expanded
}

// resolvePathTransport determines the effective transport mode for a path.
// Returns "stripe" for Starlink paths or "quic" (default).
// Priority: explicit per-path → global starlink_transport → auto-detect.
func resolvePathTransport(p MultipathPathConfig, cfg *Config, logger *Logger) string {
	// Explicit per-path transport
	if p.Transport != "" && p.Transport != "auto" {
		return p.Transport
	}
	// Global starlink_transport setting
	if cfg.StarlinkTransport == "stripe" {
		return "stripe"
	}
	// Auto-detect: if detect_starlink is enabled, check rDNS/CGNAT
	if p.Transport == "auto" || cfg.DetectStarlink {
		if detectStarlink(p.BindIP, logger) {
			logger.Infof("starlink auto-detected for path=%s → stripe transport", p.Name)
			return "stripe"
		}
	}
	return "quic"
}

// detectStarlink checks if a network interface is connected via Starlink by
// performing a reverse DNS lookup on the interface's WAN IP.
// Starlink IPs resolve to *.starlinkisp.net PTR records.
// Uses the interface name extracted from bind_ip (e.g. "if:enp7s7") to
// obtain the external IP via a DNS resolver bound to that interface.
func detectStarlink(bindIP string, logger *Logger) bool {
	ip, err := resolveBindIP(bindIP)
	if err != nil {
		logger.Debugf("starlink detect: cannot resolve %s: %v", bindIP, err)
		return false
	}

	// First check: CGNAT range 100.64.0.0/10 is commonly used by Starlink
	parsed := net.ParseIP(ip)
	if parsed == nil {
		return false
	}
	cgnat := net.IPNet{IP: net.ParseIP("100.64.0.0"), Mask: net.CIDRMask(10, 32)}
	isCGNAT := cgnat.Contains(parsed)

	// Try reverse DNS on the local IP (Starlink assigns IPs with PTR records)
	names, err := net.LookupAddr(ip)
	if err == nil {
		for _, name := range names {
			lower := strings.ToLower(name)
			if strings.Contains(lower, "starlinkisp.net") || strings.Contains(lower, "starlink") {
				logger.Debugf("starlink detect: positive via rDNS %s → %s", ip, name)
				return true
			}
		}
	}

	// Fallback: try to obtain WAN IP via DNS (dig-style) and check rDNS on that
	wanIP := getWANIPViaDNS(ip, logger)
	if wanIP != "" {
		wanNames, err := net.LookupAddr(wanIP)
		if err == nil {
			for _, name := range wanNames {
				lower := strings.ToLower(name)
				if strings.Contains(lower, "starlinkisp.net") || strings.Contains(lower, "starlink") {
					logger.Debugf("starlink detect: positive via WAN rDNS %s → %s", wanIP, name)
					return true
				}
			}
		}
	}

	// Heuristic: CGNAT range is a strong indicator (most Starlink installations)
	if isCGNAT {
		logger.Debugf("starlink detect: CGNAT range match %s (heuristic positive)", ip)
		return true
	}

	logger.Debugf("starlink detect: negative for %s", ip)
	return false
}

// getWANIPViaDNS obtains the external (WAN) IP for traffic exiting via a
// specific local IP by using OpenDNS myip resolution.
// This is equivalent to: dig +short myip.opendns.com @resolver1.opendns.com -b <localIP>
func getWANIPViaDNS(localIP string, logger *Logger) string {
	laddr := net.ParseIP(localIP)
	if laddr == nil {
		return ""
	}
	dialer := &net.Dialer{
		LocalAddr: &net.UDPAddr{IP: laddr},
		Timeout:   3 * time.Second,
	}
	resolver := &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
			return dialer.DialContext(ctx, "udp", "208.67.222.222:53") // resolver1.opendns.com
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	ips, err := resolver.LookupHost(ctx, "myip.opendns.com")
	if err != nil || len(ips) == 0 {
		logger.Debugf("starlink detect: WAN IP lookup failed via %s: %v", localIP, err)
		return ""
	}
	logger.Debugf("starlink detect: WAN IP via %s = %s", localIP, ips[0])
	return ips[0]
}

func resolveBindIP(value string) (string, error) {
	if strings.HasPrefix(value, "if:") {
		ifName := strings.TrimPrefix(value, "if:")
		iface, err := net.InterfaceByName(ifName)
		if err != nil {
			return "", err
		}
		addrs, err := iface.Addrs()
		if err != nil {
			return "", err
		}
		for _, addr := range addrs {
			ipNet, ok := addr.(*net.IPNet)
			if !ok {
				continue
			}
			ip := ipNet.IP.To4()
			if ip == nil {
				continue
			}
			if ip.IsLoopback() {
				continue
			}
			return ip.String(), nil
		}
		return "", fmt.Errorf("no ipv4 found on %s", ifName)
	}
	ip := net.ParseIP(value)
	if ip == nil {
		return "", fmt.Errorf("invalid bind_ip: %s", value)
	}
	return value, nil
}
