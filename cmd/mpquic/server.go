package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/songgao/water"
)

// configureTUN applies IP address, MTU and UP state to a TUN device.
// This is called AFTER water.New() to ensure the config is applied even
// if the library recreated the device (which wipes ensure_tun.sh settings).
func configureTUN(name, cidr string, mtu int, logger *Logger) error {
	if cidr == "" {
		return nil // nothing to configure (server-side may not have cidr)
	}
	if mtu <= 0 {
		mtu = 1300
	}
	cmds := [][]string{
		{"ip", "addr", "replace", cidr, "dev", name},
		{"ip", "link", "set", name, "mtu", fmt.Sprintf("%d", mtu)},
		{"ip", "link", "set", name, "up"},
	}
	for _, args := range cmds {
		out, err := exec.Command(args[0], args[1:]...).CombinedOutput()
		if err != nil {
			return fmt.Errorf("%s: %w (%s)", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
		}
	}
	logger.Infof("TUN %s configured: cidr=%s mtu=%d up=true", name, cidr, mtu)
	return nil
}

func runServer(ctx context.Context, cfg *Config, logger *Logger) error {
	if cfg.MultiConnEnabled {
		return runServerMultiConn(ctx, cfg, logger)
	}
	return runServerSingleConn(ctx, cfg, logger)
}

// openTUN opens a TUN device, trying IFF_MULTI_QUEUE first and falling back
// to single-queue. Returns the interface, whether multiqueue succeeded, and error.
func openTUN(name string, logger *Logger) (tun *water.Interface, multiQueue bool, err error) {
	tun, err = water.New(water.Config{
		DeviceType: water.TUN,
		PlatformSpecificParams: water.PlatformSpecificParams{
			Name:       name,
			MultiQueue: true,
		},
	})
	if err == nil {
		logger.Infof("TUN %s opened with IFF_MULTI_QUEUE", name)
		return tun, true, nil
	}
	logger.Infof("TUN %s: multiqueue not available (%v), falling back to single-queue", name, err)
	tun, err = water.New(water.Config{
		DeviceType: water.TUN,
		PlatformSpecificParams: water.PlatformSpecificParams{
			Name: name,
		},
	})
	if err != nil {
		return nil, false, err
	}
	return tun, false, nil
}

// runServerMultiConn accepts N concurrent QUIC connections on the same port,
// all sharing a single TUN device. Each client registers its TUN peer IP by
// sending it as the first datagram. The TUN reader dispatches return packets
// to the correct connection by inspecting the destination IP.
func runServerMultiConn(ctx context.Context, cfg *Config, logger *Logger) error {
	bindIP, err := resolveBindIP(cfg.BindIP)
	if err != nil {
		return err
	}

	tun, tunMultiQueue, err := openTUN(cfg.TunName, logger)
	if err != nil {
		return err
	}
	defer tun.Close()
	if err := configureTUN(cfg.TunName, cfg.TunCIDR, cfg.TunMTU, logger); err != nil {
		return fmt.Errorf("configure TUN: %w", err)
	}

	ct := newConnectionTable()
	defer ct.closeAll()

	// Periodic GC for stale flow entries in dispatch flowPaths maps.
	// Every 30s: current → prev, fresh map allocated. Active flows are
	// promoted on next packet. Idle flows expire after 2 ticks (60s max).
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				ct.flowGCTick()
			}
		}
	}()

	// Single TUN reader dispatches packets to the right connection via dst IP.
	go func() {
		buf := make([]byte, 65535)
		var lastDispatchFail time.Time
		for {
			n, readErr := tun.Read(buf)
			if readErr != nil {
				logger.Errorf("tun read error: %v", readErr)
				return
			}
			pkt := buf[:n]
			if len(pkt) < 20 {
				continue
			}

			// Extract dst IP from IPv4 header (bytes 16-19)
			version := pkt[0] >> 4
			var dstIP netip.Addr
			if version == 4 && len(pkt) >= 20 {
				dstIP = netip.AddrFrom4([4]byte{pkt[16], pkt[17], pkt[18], pkt[19]})
			} else if version == 6 && len(pkt) >= 40 {
				var b [16]byte
				copy(b[:], pkt[24:40])
				dstIP = netip.AddrFrom16(b)
			} else {
				continue
			}

			pktCopy := append([]byte(nil), pkt...)
			if !ct.dispatch(dstIP, pktCopy) {
				now := time.Now()
				if now.Sub(lastDispatchFail) > time.Second {
					lastDispatchFail = now
					logger.Infof("dispatch failed for dst=%s (no path or buffer full)", dstIP)
				}
			}
		}
	}()

	tlsConf, err := loadServerTLSConfig(cfg)
	if err != nil {
		return err
	}

	// Shared pending-keys store for stripe QUIC key exchange
	pendingKeys := newStripePendingKeys()

	// Start stripe listener if enabled (for Starlink session bypass clients)
	if cfg.StripeEnabled {
		ss, err := newStripeServer(cfg, tun, tunMultiQueue, ct, pendingKeys, logger)
		if err != nil {
			logger.Errorf("stripe server init failed: %v (continuing with QUIC only)", err)
		} else {
			registerMetricsServer(ss)
			defer ss.Close()
			go ss.Run(ctx)
		}
	}

	listenAddr := net.JoinHostPort(bindIP, fmt.Sprintf("%d", cfg.RemotePort))
	logger.Infof("server multi-conn listen=%s tun=%s", listenAddr, cfg.TunName)
	listener, err := quic.ListenAddr(listenAddr, tlsConf, &quic.Config{
		EnableDatagrams:     true,
		KeepAlivePeriod:     15 * time.Second,
		MaxIdleTimeout:      60 * time.Second,
		CongestionAlgorithm: cfg.CongestionAlgorithm,
	})
	if err != nil {
		return err
	}
	defer listener.Close()

	for {
		conn, err := listener.Accept(ctx)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return nil
			}
			return err
		}

		// Route by ALPN: stripe key exchange vs regular tunnel
		alpn := conn.ConnectionState().TLS.NegotiatedProtocol
		if alpn == stripeKXALPN {
			logger.Infof("stripe KX accepted remote=%s", conn.RemoteAddr())
			go handleStripeKeyExchange(conn, pendingKeys, logger)
			continue
		}

		logger.Infof("multi-conn accepted remote=%s", conn.RemoteAddr())

		go func(c quic.Connection) {
			if err := runServerMultiConnTunnel(ctx, c, tun, ct, cfg, logger); err != nil && !errors.Is(err, context.Canceled) {
				logger.Errorf("multi-conn tunnel closed: %v", err)
			}
		}(conn)
	}
}

// runServerMultiConnTunnel handles a single QUIC connection in multi-conn mode.
// First datagram received is expected to be a 4-byte registration containing the
// client's TUN IP. After registration, all received datagrams are written to TUN.
// Return path is handled by the shared TUN reader via connectionTable.
//
// Multi-path aware: multiple connections from the same peer IP are grouped in
// the connectionTable. When this goroutine exits, only this specific connection
// is removed from the group (not the entire peer entry).
func runServerMultiConnTunnel(parentCtx context.Context, conn quic.Connection, tun *water.Interface, ct *connectionTable, cfg *Config, logger *Logger) error {
	connCtx, cancel := context.WithCancel(parentCtx)
	defer cancel()

	remoteAddr := conn.RemoteAddr().String()

	// Wrap connection based on transport mode
	var dc datagramConn
	if cfg.TransportMode == "reliable" {
		sc, err := acceptStreamConn(connCtx, conn)
		if err != nil {
			return fmt.Errorf("accept stream: %w", err)
		}
		dc = sc
	} else {
		dc = conn
	}

	// Wait for registration: first message = 4-byte IPv4 peer IP
	var peerIP netip.Addr
	registered := false

	// On exit, remove this specific connection from the group
	defer func() {
		if registered {
			ct.unregisterConn(peerIP, remoteAddr)
			logger.Infof("multi-conn unregistered peer=%s remote=%s remaining_paths=%d",
				peerIP, remoteAddr, ct.pathCount(peerIP))
		}
	}()

	for {
		pkt, err := dc.ReceiveDatagram(connCtx)
		if err != nil {
			return err
		}

		if !registered {
			// Registration: first datagram is a 4-byte IPv4 address
			if len(pkt) == 4 {
				peerIP = netip.AddrFrom4([4]byte{pkt[0], pkt[1], pkt[2], pkt[3]})
				ct.register(peerIP, conn, dc, cancel)
				registered = true
				logger.Infof("multi-conn registered peer=%s remote=%s paths=%d",
					peerIP, remoteAddr, ct.pathCount(peerIP))
				continue
			}
			// Not a registration packet, try to auto-detect from IP header
			if len(pkt) >= 20 {
				version := pkt[0] >> 4
				if version == 4 {
					peerIP = netip.AddrFrom4([4]byte{pkt[12], pkt[13], pkt[14], pkt[15]})
					ct.register(peerIP, conn, dc, cancel)
					registered = true
					logger.Infof("multi-conn auto-registered peer=%s remote=%s paths=%d (from packet src)",
						peerIP, remoteAddr, ct.pathCount(peerIP))
					// Fall through to write this packet to TUN
				}
			}
		}

		if !registered {
			logger.Debugf("dropping pre-registration packet len=%d", len(pkt))
			continue
		}

		// Update last-received timestamp so lookup() prefers active paths.
		ct.touchPath(peerIP, remoteAddr)

		// De-duplicate: if a multi-path client sends the same packet via
		// multiple paths (duplication mode), skip writing it to TUN twice.
		if ct.pathCount(peerIP) > 1 && ct.dedup.isDuplicate(pkt) {
			logger.Debugf("dedup: skipping duplicate packet from peer=%s remote=%s len=%d", peerIP, remoteAddr, len(pkt))
			continue
		}

		// Learn source IP for return-path routing: if the client
		// forwards traffic from LAN hosts (src != peerIP), we record
		// src→peerIP so the TUN reader can dispatch replies.
		if len(pkt) >= 20 {
			version := pkt[0] >> 4
			if version == 4 {
				srcIP := netip.AddrFrom4([4]byte{pkt[12], pkt[13], pkt[14], pkt[15]})
				if srcIP != peerIP {
					ct.learnRoute(srcIP, peerIP)
				}
			}
		}

		if _, err := tun.Write(pkt); err != nil {
			return err
		}
		logger.Debugf("RX %d bytes from peer=%s via %s", len(pkt), peerIP, remoteAddr)
	}
}

// runServerSingleConn is the original single-connection server mode.
// Only one active client at a time; new connections supersede old ones.
func runServerSingleConn(ctx context.Context, cfg *Config, logger *Logger) error {
	bindIP, err := resolveBindIP(cfg.BindIP)
	if err != nil {
		return err
	}
	tun, _, err := openTUN(cfg.TunName, logger)
	if err != nil {
		return err
	}
	defer tun.Close()
	if err := configureTUN(cfg.TunName, cfg.TunCIDR, cfg.TunMTU, logger); err != nil {
		return fmt.Errorf("configure TUN: %w", err)
	}

	// Single TUN reader goroutine prevents goroutine leak on client reconnect.
	// Previously each runTunnelWithTUN call spawned its own tun.Read goroutine
	// that persisted after context cancellation (tun.Read is not ctx-aware),
	// causing stale goroutines to steal return-path packets from the active
	// connection. With N stale goroutines the active reader has 1/(N+1)
	// probability per packet, effectively killing VPS→client dataplane.
	tunReadCh := make(chan []byte, 64)
	go func() {
		buf := make([]byte, 65535)
		for {
			n, err := tun.Read(buf)
			if err != nil {
				logger.Errorf("tun read error: %v", err)
				return
			}
			pkt := append([]byte(nil), buf[:n]...)
			select {
			case tunReadCh <- pkt:
			default:
				// drop if channel full (no active conn or back-pressure)
			}
		}
	}()

	tlsConf, err := loadServerTLSConfig(cfg)
	if err != nil {
		return err
	}
	listenAddr := net.JoinHostPort(bindIP, fmt.Sprintf("%d", cfg.RemotePort))
	logger.Infof("server listen=%s tun=%s", listenAddr, cfg.TunName)
	listener, err := quic.ListenAddr(listenAddr, tlsConf, &quic.Config{
		EnableDatagrams:     true,
		KeepAlivePeriod:     15 * time.Second,
		MaxIdleTimeout:      60 * time.Second,
		CongestionAlgorithm: cfg.CongestionAlgorithm,
	})
	if err != nil {
		return err
	}
	defer listener.Close()

	var activeMu sync.Mutex
	var activeConn quic.Connection
	var activeCancel context.CancelFunc

	defer func() {
		activeMu.Lock()
		defer activeMu.Unlock()
		if activeCancel != nil {
			activeCancel()
		}
		if activeConn != nil {
			_ = activeConn.CloseWithError(0, "shutdown")
		}
	}()

	for {
		conn, err := listener.Accept(ctx)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return nil
			}
			return err
		}
		logger.Infof("accepted remote=%s", conn.RemoteAddr())

		activeMu.Lock()
		if activeCancel != nil {
			activeCancel()
		}
		if activeConn != nil {
			_ = activeConn.CloseWithError(0, "superseded")
		}
		connCtx, cancel := context.WithCancel(ctx)
		activeConn = conn
		activeCancel = cancel
		activeMu.Unlock()

		go func(c quic.Connection, cctx context.Context) {
			var dc datagramConn
			if cfg.TransportMode == "reliable" {
				sc, err := acceptStreamConn(cctx, c)
				if err != nil {
					logger.Errorf("accept stream: %v", err)
					return
				}
				dc = sc
			} else {
				dc = c
			}
			if err := runServerTunnel(cctx, dc, tun, tunReadCh, logger); err != nil && !errors.Is(err, context.Canceled) {
				logger.Errorf("tunnel closed: %v", err)
			}
		}(conn, connCtx)
	}
}

// runServerTunnel bridges a single QUIC connection to the shared TUN device.
// TX direction reads from the tunReadCh channel (fed by the single TUN reader)
// instead of calling tun.Read directly, so the goroutine exits cleanly when
// the context is cancelled — no leak, no packet theft.
func runServerTunnel(ctx context.Context, conn datagramConn, tun *water.Interface, tunReadCh <-chan []byte, logger *Logger) error {
	errCh := make(chan error, 1)

	// TX: tunReadCh → QUIC (context-aware, exits on cancel)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case pkt, ok := <-tunReadCh:
				if !ok {
					return
				}
				if err := conn.SendDatagram(pkt); err != nil {
					select {
					case errCh <- err:
					default:
					}
					return
				}
				logger.Debugf("TX %d bytes", len(pkt))
			}
		}
	}()

	// RX: QUIC → TUN
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
			return err
		}
		logger.Debugf("RX %d bytes", len(pkt))
	}
}
