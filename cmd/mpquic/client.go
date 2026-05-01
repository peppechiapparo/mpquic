package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/quic-go/quic-go"
	"gopkg.in/yaml.v3"
)

type multipathPathState struct {
	cfg              MultipathPathConfig
	udpConn          *net.UDPConn
	transport        *quic.Transport
	conn             quic.Connection
	dc               datagramConn
	stripeConn       *stripeClientConn // non-nil for stripe transport paths
	alive            bool
	reconnecting     bool
	consecutiveFails int
	cooldownUntil    time.Time
	txPackets        uint64
	rxPackets        uint64
	txErrors         uint64
	rxErrors         uint64
	lastUp           time.Time
	lastDown         time.Time

	// atomic-only fields (no lock) — used by healthCheckLoop (single writer
	// for `degraded`/`degradedSinceNs`) and by RX hooks that bump lastRxNs.
	lastRxNs        int64  // UnixNano of last RX seen on this path
	degraded        uint32 // single-writer (healthCheckLoop): 1 = silent > threshold
	degradedSinceNs int64  // UnixNano of alive→degraded transition (0 when sane)
	degradedTotal   uint64 // counter alive→degraded transitions
	failoverTotal   uint64 // counter (mirrors degradedTotal for now)
	failbackTotal   uint64 // counter degraded→healthy transitions
	blackholeNs     int64  // cumulative ns spent in degraded state
}

type multipathConn struct {
	mu        sync.RWMutex
	paths     []*multipathPathState
	recvCh    chan []byte
	errCh     chan error
	errOnce   sync.Once
	rr        int
	logger    *Logger
	cfg       *Config
	dataplane compiledDataplane
	classTx   map[string]*trafficClassCounters
	baseCtx   context.Context
}

func runClientLoop(ctx context.Context, cfg *Config, logger *Logger) error {
	for {
		err := runClientOnce(ctx, cfg, logger)
		if err == nil || errors.Is(err, context.Canceled) {
			return err
		}
		logger.Errorf("reconnect in 3s: %v", err)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
}

func runClientOnce(ctx context.Context, cfg *Config, logger *Logger) error {
	if cfg.MultipathEnabled {
		return runClientOnceMultipath(ctx, cfg, logger)
	}

	bindIP, err := resolveBindIP(cfg.BindIP)
	if err != nil {
		return err
	}
	localUDP := &net.UDPAddr{IP: net.ParseIP(bindIP), Port: 0}
	udpConn, err := net.ListenUDP("udp", localUDP)
	if err != nil {
		return err
	}
	defer udpConn.Close()

	remoteUDP, err := net.ResolveUDPAddr("udp", net.JoinHostPort(cfg.RemoteAddr, fmt.Sprintf("%d", cfg.RemotePort)))
	if err != nil {
		return err
	}
	tlsConf, err := loadClientTLSConfig(cfg)
	if err != nil {
		return err
	}

	transport := quic.Transport{Conn: udpConn}
	conn, err := transport.Dial(ctx, remoteUDP, tlsConf, &quic.Config{
		EnableDatagrams:     true,
		KeepAlivePeriod:     15 * time.Second,
		MaxIdleTimeout:      60 * time.Second,
		CongestionAlgorithm: cfg.CongestionAlgorithm,
	})
	if err != nil {
		return err
	}
	defer conn.CloseWithError(0, "shutdown")

	logger.Infof("connected local=%s remote=%s tun=%s", udpConn.LocalAddr(), remoteUDP.String(), cfg.TunName)

	// Start IP change watcher: if the bind interface's IP changes (e.g. Starlink
	// CGNAT reassignment), cancel the tunnel context to force a fast reconnect
	// instead of waiting for the 60s QUIC idle timeout.
	tunnelCtx, tunnelCancel := context.WithCancelCause(ctx)
	defer tunnelCancel(nil)
	go watchInterfaceIP(tunnelCtx, cfg.BindIP, bindIP, func(newIP string) {
		logger.Infof("WARN bind IP changed on %s: %s → %s, triggering reconnect", cfg.BindIP, bindIP, newIP)
		tunnelCancel(fmt.Errorf("bind IP changed: %s → %s", bindIP, newIP))
	})

	var dc datagramConn
	if cfg.TransportMode == "reliable" {
		sc, err := openStreamConn(tunnelCtx, conn)
		if err != nil {
			return fmt.Errorf("open stream: %w", err)
		}
		dc = sc
	} else {
		dc = conn
	}

	// Wrap in countingConn to track TX/RX for single-path metrics
	cc := newCountingConn(dc)
	registerMetricsSinglePath(cc)

	err = runTunnel(tunnelCtx, cfg, cc, logger)
	// Distinguish IP-watcher cancellation from real shutdown: if the parent
	// context is still alive, the cancel came from the IP watcher — wrap the
	// cause so runClientLoop sees a non-Canceled error and reconnects.
	if err != nil && errors.Is(err, context.Canceled) && ctx.Err() == nil {
		cause := context.Cause(tunnelCtx)
		if cause != nil {
			return fmt.Errorf("ip watcher: %w", cause)
		}
	}
	return err
}

func runClientOnceMultipath(ctx context.Context, cfg *Config, logger *Logger) error {
	mpConn, err := newMultipathConn(ctx, cfg, logger)
	if err != nil {
		return err
	}
	defer mpConn.closeAll(0, "shutdown")

	if cfg.ControlAPIListen != "" {
		stopAPI, err := startControlAPI(ctx, cfg, mpConn, logger)
		if err != nil {
			return err
		}
		defer stopAPI()
	}

	logger.Infof("connected multipath paths=%d policy=%s tun=%s", len(cfg.MultipathPaths), cfg.MultipathPolicy, cfg.TunName)
	return runTunnel(ctx, cfg, mpConn, logger)
}

func newMultipathConn(ctx context.Context, cfg *Config, logger *Logger) (*multipathConn, error) {
	dpRuntime, err := compileDataplaneConfig(cfg.Dataplane)
	if err != nil {
		return nil, err
	}

	// Expand pipes: paths with pipes > 1 become N internal path entries
	expandedPaths := expandMultipathPipes(cfg.MultipathPaths, cfg, logger)

	mp := &multipathConn{
		recvCh:    make(chan []byte, 512),
		errCh:     make(chan error, 1),
		logger:    logger,
		cfg:       cfg,
		dataplane: dpRuntime,
		classTx:   make(map[string]*trafficClassCounters),
		baseCtx:   ctx,
	}
	registerMetricsClient(mp)

	for className := range dpRuntime.classes {
		mp.classTx[className] = &trafficClassCounters{}
	}

	aliveCount := 0

	for _, p := range expandedPaths {
		state := &multipathPathState{cfg: p}
		mp.paths = append(mp.paths, state)

		effectiveTransport := resolvePathTransport(p, cfg, logger)

		// ── Stripe transport (Starlink-optimized) ─────────────────
		if effectiveTransport == "stripe" {
			sessionID, err := stripeComputeSessionID(cfg, p.Name)
			if err != nil {
				logger.Errorf("stripe session ID failed name=%s err=%v", p.Name, err)
				state.reconnecting = true
				continue
			}
			keys, err := stripeNegotiateKey(ctx, cfg, p, sessionID, logger)
			if err != nil {
				logger.Errorf("stripe key exchange failed name=%s err=%v", p.Name, err)
				state.reconnecting = true
				continue
			}
			sc, err := newStripeClientConn(ctx, cfg, p, keys, &state.lastRxNs, logger)
			if err != nil {
				logger.Errorf("stripe init failed name=%s err=%v", p.Name, err)
				state.reconnecting = true
				continue
			}
			state.dc = sc
			state.stripeConn = sc
			state.alive = true
			state.reconnecting = false
			state.lastUp = time.Now()
			// Initialize liveness baseline so healthCheckLoop never sees
			// a zero `lastRxNs` for a freshly-alive path. The stripe RX
			// hook (lastRxNsPtr) was wired into the constructor before the
			// recv goroutines were spawned (no race).
			atomic.StoreInt64(&state.lastRxNs, time.Now().UnixNano())
			aliveCount++
			logger.Infof("stripe path up name=%s pipes=%d", p.Name, p.Pipes)
			continue
		}

		// ── QUIC transport (default) ──────────────────────────────
		bindIP, err := resolveBindIP(p.BindIP)
		if err != nil {
			logger.Errorf("path init failed name=%s step=bind-resolve err=%v", p.Name, err)
			state.reconnecting = true
			continue
		}

		localUDP := &net.UDPAddr{IP: net.ParseIP(bindIP), Port: 0}
		udpConn, err := net.ListenUDP("udp", localUDP)
		if err != nil {
			logger.Errorf("path init failed name=%s step=listen err=%v", p.Name, err)
			state.reconnecting = true
			continue
		}

		remoteUDP, err := net.ResolveUDPAddr("udp", net.JoinHostPort(p.RemoteAddr, fmt.Sprintf("%d", p.RemotePort)))
		if err != nil {
			_ = udpConn.Close()
			logger.Errorf("path init failed name=%s step=remote-resolve err=%v", p.Name, err)
			state.reconnecting = true
			continue
		}

		tlsConf, err := loadClientTLSConfig(cfg)
		if err != nil {
			_ = udpConn.Close()
			logger.Errorf("path init failed name=%s step=tls err=%v", p.Name, err)
			state.reconnecting = true
			continue
		}

		transport := quic.Transport{Conn: udpConn}
		conn, err := transport.Dial(ctx, remoteUDP, tlsConf, &quic.Config{
			EnableDatagrams:     true,
			KeepAlivePeriod:     15 * time.Second,
			MaxIdleTimeout:      60 * time.Second,
			CongestionAlgorithm: cfg.CongestionAlgorithm,
		})
		if err != nil {
			_ = udpConn.Close()
			logger.Errorf("path init failed name=%s step=dial err=%v", p.Name, err)
			state.reconnecting = true
			continue
		}

		state.udpConn = udpConn
		state.transport = &transport
		state.conn = conn
		if cfg.TransportMode == "reliable" {
			sc, err := openStreamConn(ctx, conn)
			if err != nil {
				_ = conn.CloseWithError(0, "stream-open-failed")
				_ = udpConn.Close()
				logger.Errorf("path init failed name=%s step=stream err=%v", p.Name, err)
				state.reconnecting = true
				continue
			}
			state.dc = sc
		} else {
			state.dc = conn
		}
		state.alive = true
		state.reconnecting = false
		state.lastUp = time.Now()
		// Initialize liveness baseline for QUIC paths too: recvLoop bumps
		// state.lastRxNs on every successful datagram.
		atomic.StoreInt64(&state.lastRxNs, time.Now().UnixNano())
		aliveCount++
		logger.Infof("path up name=%s local=%s remote=%s", p.Name, udpConn.LocalAddr(), remoteUDP.String())
	}

	if aliveCount == 0 {
		mp.closeAll(0, "dial-error")
		return nil, fmt.Errorf("multipath: no initial path available")
	}

	for idx := range mp.paths {
		go mp.recvLoop(ctx, idx)
		if !mp.paths[idx].alive && mp.paths[idx].reconnecting {
			go mp.reconnectLoop(ctx, idx)
		}
	}

	// Start IP watchers for QUIC paths with interface-based bindings.
	// If a Starlink modem changes IP (CGNAT reassignment), detect it
	// and trigger a fast reconnect instead of waiting for idle timeout.
	for idx := range mp.paths {
		p := mp.paths[idx]
		pathIdx := idx
		pcfg := p.cfg
		effectiveTransport := resolvePathTransport(pcfg, cfg, logger)
		if effectiveTransport == "stripe" {
			continue // stripe has its own keepalive/re-register mechanism
		}
		if !strings.HasPrefix(pcfg.BindIP, "if:") {
			continue
		}
		currentIP, _ := resolveBindIP(pcfg.BindIP)
		if currentIP == "" {
			continue
		}
		go watchInterfaceIP(ctx, pcfg.BindIP, currentIP, func(newIP string) {
			mp.logger.Infof("WARN bind IP changed on %s (path %s): %s → %s, triggering reconnect",
				pcfg.BindIP, pcfg.Name, currentIP, newIP)
			mp.onPathError(ctx, pathIdx, fmt.Errorf("bind IP changed: %s → %s", currentIP, newIP))
		})
	}

	go mp.telemetryLoop(ctx)
	go mp.healthCheckLoop(ctx)

	return mp, nil
}

func (m *multipathConn) SendDatagram(pkt []byte) error {
	className, classPolicy := m.resolvePacketClass(pkt)

	if classPolicy.Duplicate {
		return m.sendDuplicate(pkt, className, classPolicy)
	}

	deadline := time.Now().Add(1200 * time.Millisecond)
	for {
		idx, conn := m.selectBestPath(classPolicy, nil)
		if idx < 0 || conn == nil {
			if time.Now().After(deadline) {
				m.markClassError(className)
				return fmt.Errorf("multipath: no active path available")
			}
			time.Sleep(100 * time.Millisecond)
			continue
		}

		if err := conn.SendDatagram(pkt); err != nil {
			m.markTxError(idx, err)
			continue
		}

		m.markTxSuccess(idx)
		m.markClassTx(className)
		return nil
	}
}

func (m *multipathConn) sendDuplicate(pkt []byte, className string, classPolicy DataplaneClassPolicy) error {
	copies := classPolicy.DuplicateCopies
	if copies < 2 {
		copies = 2
	}

	skip := make(map[int]struct{}, copies)
	sent := 0
	deadline := time.Now().Add(1200 * time.Millisecond)

	for sent < copies {
		idx, conn := m.selectBestPath(classPolicy, skip)
		if idx < 0 || conn == nil {
			if sent > 0 || time.Now().After(deadline) {
				break
			}
			time.Sleep(80 * time.Millisecond)
			continue
		}

		if err := conn.SendDatagram(pkt); err != nil {
			m.markTxError(idx, err)
			skip[idx] = struct{}{}
			continue
		}

		m.markTxSuccess(idx)
		skip[idx] = struct{}{}
		sent++
	}

	if sent == 0 {
		m.markClassError(className)
		return fmt.Errorf("multipath: no active path available for duplicated send")
	}

	m.markClassTx(className)
	if sent > 1 {
		m.markClassDuplicate(className, uint64(sent-1))
	}
	return nil
}

func (m *multipathConn) selectBestPath(classPolicy DataplaneClassPolicy, skip map[int]struct{}) (int, datagramConn) {
	m.mu.Lock()
	defer m.mu.Unlock()

	now := time.Now()
	if len(m.paths) == 0 {
		return -1, nil
	}

	bestIdx := -1
	bestScore := int(^uint(0) >> 1)
	start := m.rr % len(m.paths)
	policy := classPolicy.SchedulerPolicy
	if policy == "" {
		policy = m.cfg.MultipathPolicy
	}
	if policy == "" {
		policy = "priority"
	}

	excluded := make(map[string]struct{}, len(classPolicy.ExcludedPaths))
	for _, name := range classPolicy.ExcludedPaths {
		excluded[name] = struct{}{}
	}

	preferred := make(map[string]struct{}, len(classPolicy.PreferredPaths))
	for _, name := range classPolicy.PreferredPaths {
		preferred[name] = struct{}{}
	}
	preferredOnly := len(preferred) > 0

	for pass := 0; pass < 2; pass++ {
		bestIdx = -1
		bestScore = int(^uint(0) >> 1)

		for i := 0; i < len(m.paths); i++ {
			idx := (start + i) % len(m.paths)
			if skip != nil {
				if _, blocked := skip[idx]; blocked {
					continue
				}
			}

			p := m.paths[idx]
			// Check excluded: match on both pipe name and base path name
			if _, blocked := excluded[p.cfg.Name]; blocked {
				continue
			}
			if p.cfg.BasePath != "" {
				if _, blocked := excluded[p.cfg.BasePath]; blocked {
					continue
				}
			}
			if preferredOnly && pass == 0 {
				_, nameOk := preferred[p.cfg.Name]
				_, baseOk := preferred[p.cfg.BasePath]
				if !nameOk && !baseOk {
					continue
				}
			}
			if !p.alive || p.dc == nil {
				continue
			}
			// Skip degraded paths in the regular pass; healthCheckLoop
			// is the single writer of `degraded` and never mutates `alive`,
			// so this is a pure scheduling hint. Best-of-bad fallback below
			// picks the freshest degraded path if no healthy one is found.
			if atomic.LoadUint32(&p.degraded) == 1 {
				continue
			}
			if now.Before(p.cooldownUntil) {
				continue
			}
			score := pathPolicyScore(policy, p)
			if score < bestScore {
				bestScore = score
				bestIdx = idx
			}
		}

		if bestIdx >= 0 {
			break
		}
		if !preferredOnly {
			break
		}
	}

	if bestIdx < 0 {
		// Best-of-bad fallback: every healthy path was excluded by the
		// degraded filter (combo A+E). Pick the alive path with the most
		// recent RX to minimize blackhole during recovery; tiebreak on
		// fewer consecutiveFails. We do not loosen excluded/cooldown to
		// preserve operator policy semantics.
		var bestRx int64
		for i := 0; i < len(m.paths); i++ {
			idx := (start + i) % len(m.paths)
			if skip != nil {
				if _, blocked := skip[idx]; blocked {
					continue
				}
			}
			p := m.paths[idx]
			if _, blocked := excluded[p.cfg.Name]; blocked {
				continue
			}
			if p.cfg.BasePath != "" {
				if _, blocked := excluded[p.cfg.BasePath]; blocked {
					continue
				}
			}
			if !p.alive || p.dc == nil {
				continue
			}
			rx := atomic.LoadInt64(&p.lastRxNs)
			if bestIdx < 0 || rx > bestRx ||
				(rx == bestRx && p.consecutiveFails < m.paths[bestIdx].consecutiveFails) {
				bestIdx = idx
				bestRx = rx
			}
		}
		if bestIdx < 0 {
			return -1, nil
		}
	}

	m.rr = (bestIdx + 1) % len(m.paths)
	return bestIdx, m.paths[bestIdx].dc
}

func pathPolicyScore(policy string, p *multipathPathState) int {
	base := p.cfg.Priority * 1000
	failPenalty := p.consecutiveFails * 100

	switch policy {
	case "failover":
		return base + failPenalty
	case "balanced":
		weightBonus := 0
		if p.cfg.Weight > 1 {
			weightBonus = (p.cfg.Weight - 1) * 120
		}
		return base + failPenalty - weightBonus
	default:
		weightBonus := 0
		if p.cfg.Weight > 1 {
			weightBonus = (p.cfg.Weight - 1) * 10
		}
		return base + failPenalty - weightBonus
	}
}

func (m *multipathConn) resolvePacketClass(pkt []byte) (string, DataplaneClassPolicy) {
	meta, ok := parsePacketMeta(pkt)
	if ok {
		for _, rule := range m.dataplane.classifiers {
			if rule.matches(meta) {
				if classPolicy, found := m.dataplane.classes[rule.className]; found {
					return rule.className, classPolicy
				}
			}
		}
	}

	className := m.dataplane.defaultClass
	classPolicy, found := m.dataplane.classes[className]
	if !found {
		className = "default"
		classPolicy = DataplaneClassPolicy{SchedulerPolicy: "priority"}
	}
	return className, classPolicy
}

func (m *multipathConn) markClassTx(className string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c := m.classTx[className]
	if c == nil {
		c = &trafficClassCounters{}
		m.classTx[className] = c
	}
	c.txPackets++
}

func (m *multipathConn) markClassError(className string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c := m.classTx[className]
	if c == nil {
		c = &trafficClassCounters{}
		m.classTx[className] = c
	}
	c.txErrors++
}

func (m *multipathConn) markClassDuplicate(className string, duplicates uint64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c := m.classTx[className]
	if c == nil {
		c = &trafficClassCounters{}
		m.classTx[className] = c
	}
	c.txDuplicates += duplicates
}

func (m *multipathConn) markTxSuccess(idx int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p := m.paths[idx]
	p.txPackets++
	if p.consecutiveFails > 0 {
		p.consecutiveFails--
	}
}

func (m *multipathConn) markTxError(idx int, err error) {
	m.mu.Lock()
	p := m.paths[idx]
	p.txErrors++
	p.consecutiveFails++
	if p.consecutiveFails > 6 {
		p.consecutiveFails = 6
	}
	p.cooldownUntil = time.Now().Add(time.Duration(p.consecutiveFails) * time.Second)
	p.alive = false
	p.dc = nil
	if p.conn != nil {
		_ = p.conn.CloseWithError(0, "tx-error")
		p.conn = nil
	}
	if p.udpConn != nil {
		_ = p.udpConn.Close()
		p.udpConn = nil
	}
	name := p.cfg.Name
	needReconnect := !p.reconnecting
	if needReconnect {
		p.reconnecting = true
	}
	m.mu.Unlock()

	m.logger.Errorf("path tx failed name=%s err=%v", name, err)
	if needReconnect {
		go m.reconnectLoop(m.baseCtx, idx)
	}
}

func (m *multipathConn) ReceiveDatagram(ctx context.Context) ([]byte, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case err := <-m.errCh:
		return nil, err
	case pkt := <-m.recvCh:
		return pkt, nil
	}
}

func (m *multipathConn) recvLoop(ctx context.Context, idx int) {
	for {
		// Fast exit check — essential for clean shutdown
		select {
		case <-ctx.Done():
			return
		default:
		}

		conn := m.currentPathConn(idx)
		if conn == nil {
			select {
			case <-ctx.Done():
				return
			case <-time.After(150 * time.Millisecond):
			}
			continue
		}

		pkt, err := conn.ReceiveDatagram(ctx)
		if err != nil {
			// During shutdown, don't trigger reconnect — just exit
			if ctx.Err() != nil {
				return
			}
			m.onPathError(ctx, idx, err)
			continue
		}
		m.onPathSuccess(idx)
		atomic.StoreInt64(&m.paths[idx].lastRxNs, time.Now().UnixNano())
		copyPkt := append([]byte(nil), pkt...)
		select {
		case <-ctx.Done():
			return
		case m.recvCh <- copyPkt:
		}
	}
}

func (m *multipathConn) currentPathConn(idx int) datagramConn {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if idx < 0 || idx >= len(m.paths) {
		return nil
	}
	return m.paths[idx].dc
}

func (m *multipathConn) onPathSuccess(idx int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p := m.paths[idx]
	p.rxPackets++
	if p.consecutiveFails > 0 {
		p.consecutiveFails--
	}
}

func (m *multipathConn) onPathError(ctx context.Context, idx int, err error) {
	// Skip reconnect during shutdown
	if ctx.Err() != nil {
		return
	}

	m.mu.Lock()
	p := m.paths[idx]
	p.alive = false
	p.rxErrors++
	p.lastDown = time.Now()
	p.consecutiveFails++
	if p.consecutiveFails > 6 {
		p.consecutiveFails = 6
	}
	p.cooldownUntil = time.Now().Add(time.Duration(p.consecutiveFails) * time.Second)
	oldStripe := p.stripeConn
	oldConn := p.conn
	oldUDP := p.udpConn
	p.dc = nil
	p.stripeConn = nil
	p.conn = nil
	p.udpConn = nil
	name := p.cfg.Name
	needReconnect := !p.reconnecting
	if needReconnect {
		p.reconnecting = true
	}
	m.mu.Unlock()

	// Close outside lock to prevent deadlock with closeAll
	if oldStripe != nil {
		_ = oldStripe.Close()
	}
	if oldConn != nil {
		_ = oldConn.CloseWithError(0, "rx-error")
	}
	if oldUDP != nil {
		_ = oldUDP.Close()
	}

	m.logger.Errorf("path down name=%s err=%v", name, err)
	if needReconnect {
		go m.reconnectLoop(ctx, idx)
	}
}

func (m *multipathConn) reconnectLoop(ctx context.Context, idx int) {
	for {
		select {
		case <-ctx.Done():
			m.mu.Lock()
			if idx >= 0 && idx < len(m.paths) {
				m.paths[idx].reconnecting = false
			}
			m.mu.Unlock()
			return
		default:
		}

		m.mu.RLock()
		if idx < 0 || idx >= len(m.paths) {
			m.mu.RUnlock()
			return
		}
		pcfg := m.paths[idx].cfg
		m.mu.RUnlock()

		effectiveTransport := resolvePathTransport(pcfg, m.cfg, m.logger)

		// ── Stripe reconnect ──────────────────────────────────────
		if effectiveTransport == "stripe" {
			sessionID, err := stripeComputeSessionID(m.cfg, pcfg.Name)
			if err != nil {
				m.logger.Errorf("stripe session ID failed name=%s err=%v", pcfg.Name, err)
				select {
				case <-ctx.Done():
					return
				case <-time.After(2 * time.Second):
				}
				continue
			}
			keys, err := stripeNegotiateKey(ctx, m.cfg, pcfg, sessionID, m.logger)
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				m.logger.Errorf("stripe key exchange failed name=%s err=%v", pcfg.Name, err)
				select {
				case <-ctx.Done():
					return
				case <-time.After(2 * time.Second):
				}
				continue
			}
			sc, err := newStripeClientConn(ctx, m.cfg, pcfg, keys, &m.paths[idx].lastRxNs, m.logger)
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				m.logger.Errorf("stripe redial failed name=%s err=%v", pcfg.Name, err)
				select {
				case <-ctx.Done():
					return
				case <-time.After(2 * time.Second):
				}
				continue
			}
			m.mu.Lock()
			if idx >= 0 && idx < len(m.paths) {
				p := m.paths[idx]
				p.dc = sc
				p.stripeConn = sc
				p.alive = true
				p.reconnecting = false
				p.lastUp = time.Now()
				if p.consecutiveFails > 0 {
					p.consecutiveFails--
				}
				// Re-baseline liveness; the stripe RX hook (lastRxNsPtr) was
				// wired into the new constructor before recv goroutines spawned
				// (no race). Accumulate any unflushed blackhole time before
				// clearing the degraded marker so metrics stay accurate when
				// recovery is driven by reconnect rather than healthCheckLoop.
				nowNs := time.Now().UnixNano()
				flushDegradedOnReset(p, nowNs)
				atomic.StoreInt64(&p.lastRxNs, nowNs)
			}
			m.mu.Unlock()
			m.logger.Infof("stripe path recovered name=%s pipes=%d", pcfg.Name, pcfg.Pipes)
			return
		}

		// ── QUIC reconnect (existing logic) ───────────────────────
		bindIP, err := resolveBindIP(pcfg.BindIP)
		if err != nil {
			m.logger.Errorf("path redial resolve failed name=%s err=%v", pcfg.Name, err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(2 * time.Second):
			}
			continue
		}

		localUDP := &net.UDPAddr{IP: net.ParseIP(bindIP), Port: 0}
		udpConn, err := net.ListenUDP("udp", localUDP)
		if err != nil {
			m.logger.Errorf("path redial listen failed name=%s err=%v", pcfg.Name, err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(2 * time.Second):
			}
			continue
		}

		remoteUDP, err := net.ResolveUDPAddr("udp", net.JoinHostPort(pcfg.RemoteAddr, fmt.Sprintf("%d", pcfg.RemotePort)))
		if err != nil {
			_ = udpConn.Close()
			m.logger.Errorf("path redial remote resolve failed name=%s err=%v", pcfg.Name, err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(2 * time.Second):
			}
			continue
		}

		tlsConf, err := loadClientTLSConfig(m.cfg)
		if err != nil {
			_ = udpConn.Close()
			m.logger.Errorf("path redial tls failed name=%s err=%v", pcfg.Name, err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(2 * time.Second):
			}
			continue
		}

		transport := quic.Transport{Conn: udpConn}
		dialCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
		conn, err := transport.Dial(dialCtx, remoteUDP, tlsConf, &quic.Config{
			EnableDatagrams:     true,
			KeepAlivePeriod:     15 * time.Second,
			MaxIdleTimeout:      60 * time.Second,
			CongestionAlgorithm: m.cfg.CongestionAlgorithm,
		})
		cancel()
		if err != nil {
			_ = udpConn.Close()
			m.logger.Errorf("path redial failed name=%s err=%v", pcfg.Name, err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(2 * time.Second):
			}
			continue
		}

		var dc datagramConn
		if m.cfg.TransportMode == "reliable" {
			sc, err := openStreamConn(ctx, conn)
			if err != nil {
				_ = conn.CloseWithError(0, "stream-open-failed")
				_ = udpConn.Close()
				m.logger.Errorf("path redial stream failed name=%s err=%v", pcfg.Name, err)
				select {
				case <-ctx.Done():
					return
				case <-time.After(2 * time.Second):
				}
				continue
			}
			dc = sc
		} else {
			dc = conn
		}

		m.mu.Lock()
		if idx >= 0 && idx < len(m.paths) {
			p := m.paths[idx]
			p.conn = conn
			p.dc = dc
			p.udpConn = udpConn
			p.transport = &transport
			p.alive = true
			p.reconnecting = false
			p.lastUp = time.Now()
			if p.consecutiveFails > 0 {
				p.consecutiveFails--
			}
			// Re-baseline liveness for QUIC reconnect. Accumulate any
			// unflushed blackhole time before clearing the degraded marker
			// so metrics stay accurate when recovery is driven by reconnect
			// rather than healthCheckLoop.
			nowNs := time.Now().UnixNano()
			flushDegradedOnReset(p, nowNs)
			atomic.StoreInt64(&p.lastRxNs, nowNs)
		}
		m.mu.Unlock()

		m.logger.Infof("path recovered name=%s local=%s remote=%s", pcfg.Name, udpConn.LocalAddr(), remoteUDP.String())

		// Restart IP watcher for QUIC paths with interface-based bindings
		if strings.HasPrefix(pcfg.BindIP, "if:") {
			newIP, _ := resolveBindIP(pcfg.BindIP)
			if newIP != "" {
				go watchInterfaceIP(ctx, pcfg.BindIP, newIP, func(changedIP string) {
					m.logger.Infof("WARN bind IP changed on %s (path %s): %s → %s, triggering reconnect",
						pcfg.BindIP, pcfg.Name, newIP, changedIP)
					m.onPathError(ctx, idx, fmt.Errorf("bind IP changed: %s → %s", newIP, changedIP))
				})
			}
		}
		return
	}
}

func (m *multipathConn) telemetryLoop(ctx context.Context) {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.logTelemetrySnapshot()
		}
	}
}

// healthCheckTickPath applies one health-check tick on a single path.
// It is the per-path body of healthCheckLoop, factored out for testability.
// Uses atomic ops only; safe to call without holding m.mu as long as the
// caller is the single writer of degraded/degradedSinceNs (healthCheckLoop
// at runtime, the test goroutine in unit tests).
func healthCheckTickPath(p *multipathPathState, nowNs, thresholdNs, recoveryNs int64, logger *Logger) {
	if p == nil {
		return
	}
	if !p.alive {
		return
	}
	lastRx := atomic.LoadInt64(&p.lastRxNs)
	if lastRx == 0 {
		return
	}
	silentNs := nowNs - lastRx
	if atomic.LoadUint32(&p.degraded) == 0 {
		if silentNs > thresholdNs {
			atomic.StoreUint32(&p.degraded, 1)
			atomic.StoreInt64(&p.degradedSinceNs, nowNs)
			atomic.AddUint64(&p.degradedTotal, 1)
			atomic.AddUint64(&p.failoverTotal, 1)
			if logger != nil {
				logger.Errorf("path degraded name=%s silent=%v",
					p.cfg.Name, time.Duration(silentNs))
			}
		}
		return
	}
	if silentNs < recoveryNs {
		since := atomic.LoadInt64(&p.degradedSinceNs)
		var bh int64
		if since > 0 {
			bh = nowNs - since
			atomic.AddInt64(&p.blackholeNs, bh)
		}
		atomic.AddUint64(&p.failbackTotal, 1)
		atomic.StoreUint32(&p.degraded, 0)
		atomic.StoreInt64(&p.degradedSinceNs, 0)
		if logger != nil {
			logger.Infof("path recovered name=%s blackhole=%v",
				p.cfg.Name, time.Duration(bh))
		}
	}
}

// flushDegradedOnReset accumulates any pending blackhole time and clears the
// degraded marker on a path that is being brought back up by reconnectLoop.
// Returns the blackhole delta added (0 if path was not degraded). Does NOT
// touch lastRxNs; the caller re-baselines it with its own time source.
func flushDegradedOnReset(p *multipathPathState, nowNs int64) int64 {
	var bh int64
	if since := atomic.LoadInt64(&p.degradedSinceNs); since > 0 {
		bh = nowNs - since
		atomic.AddInt64(&p.blackholeNs, bh)
		atomic.AddUint64(&p.failbackTotal, 1)
	}
	atomic.StoreUint32(&p.degraded, 0)
	atomic.StoreInt64(&p.degradedSinceNs, 0)
	return bh
}

// healthCheckLoop is the only writer of degraded/degradedSinceNs while the path is alive.
// reconnectLoop performs a handoff reset under m.mu before flipping alive back to true.
// It samples lastRxNs (mirrored by stripe RX hook and by the QUIC recvLoop)
// and flips paths between healthy and degraded based on silent-time thresholds.
// It does NOT mutate p.alive, does NOT trigger reconnectLoop, and does NOT take
// m.mu — the loop only reads len(m.paths) (paths slice is sized at init and
// never resized) and uses atomic ops on the per-path counters.
func (m *multipathConn) healthCheckLoop(ctx context.Context) {
	interval := m.cfg.StripeHealthCheckInterval
	if interval <= 0 {
		interval = stripeHealthCheckInterval
	}
	threshold := m.cfg.StripePathDegradedThreshold
	if threshold <= 0 {
		threshold = stripePathDegradedThreshold
	}
	recovery := m.cfg.StripePathDegradedRecovery
	if recovery <= 0 {
		recovery = stripePathDegradedRecovery
	}
	thresholdNs := threshold.Nanoseconds()
	recoveryNs := recovery.Nanoseconds()

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		nowNs := time.Now().UnixNano()
		// Snapshot path slice header under RLock to be safe vs future
		// growth, but do not hold the lock during atomic ops below.
		m.mu.RLock()
		paths := m.paths
		m.mu.RUnlock()

		for _, p := range paths {
			// alive is guarded by m.mu in writers, but a stale read
			// here is acceptable: at worst we skip a tick on a path
			// that just came up or down.
			healthCheckTickPath(p, nowNs, thresholdNs, recoveryNs, m.logger)
		}
	}
}

func (m *multipathConn) logTelemetrySnapshot() {
	m.mu.RLock()
	defer m.mu.RUnlock()

	// Per-pipe telemetry
	type baseAgg struct {
		txPkts, rxPkts, txErr, rxErr uint64
		alive, total                 int
	}
	agg := make(map[string]*baseAgg)

	for _, p := range m.paths {
		state := "down"
		if p.alive && (p.conn != nil || p.stripeConn != nil) {
			state = "up"
		}
		m.logger.Infof(
			"path telemetry name=%s state=%s tx_pkts=%d rx_pkts=%d tx_err=%d rx_err=%d fails=%d cooldown_until=%s last_up=%s last_down=%s",
			p.cfg.Name,
			state,
			p.txPackets,
			p.rxPackets,
			p.txErrors,
			p.rxErrors,
			p.consecutiveFails,
			formatTime(p.cooldownUntil),
			formatTime(p.lastUp),
			formatTime(p.lastDown),
		)

		// Log stripe security metrics if available
		if p.stripeConn != nil {
			if df := p.stripeConn.SecurityStats(); df > 0 {
				m.logger.Infof("stripe security name=%s decrypt_fail=%d", p.cfg.Name, df)
			}
		}

		// Aggregate by base path (for multi-pipe summary)
		base := p.cfg.BasePath
		if base == "" {
			base = p.cfg.Name
		}
		a := agg[base]
		if a == nil {
			a = &baseAgg{}
			agg[base] = a
		}
		a.txPkts += p.txPackets
		a.rxPkts += p.rxPackets
		a.txErr += p.txErrors
		a.rxErr += p.rxErrors
		a.total++
		if p.alive && (p.conn != nil || p.stripeConn != nil) {
			a.alive++
		}
	}

	// Log aggregate per base path (only when pipes > 1)
	for base, a := range agg {
		if a.total > 1 {
			m.logger.Infof(
				"path aggregate base=%s pipes=%d/%d tx_pkts=%d rx_pkts=%d tx_err=%d rx_err=%d",
				base, a.alive, a.total, a.txPkts, a.rxPkts, a.txErr, a.rxErr,
			)
		}
	}

	classes := make([]string, 0, len(m.classTx))
	for className := range m.classTx {
		classes = append(classes, className)
	}
	sort.Strings(classes)
	for _, className := range classes {
		c := m.classTx[className]
		if c == nil {
			continue
		}
		m.logger.Infof(
			"class telemetry class=%s tx_pkts=%d tx_err=%d tx_dups=%d",
			className,
			c.txPackets,
			c.txErrors,
			c.txDuplicates,
		)
	}
}

func formatTime(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.Format(time.RFC3339)
}

func (m *multipathConn) closeAll(code quic.ApplicationErrorCode, reason string) {
	// Collect references under lock, then close outside the lock.
	// This prevents deadlock with recvLoop → onPathError which also
	// needs m.mu. Closing stripe/QUIC connections unblocks those goroutines.
	m.mu.Lock()
	type toClose struct {
		stripe *stripeClientConn
		conn   quic.Connection
		udp    *net.UDPConn
	}
	var items []toClose
	for _, p := range m.paths {
		items = append(items, toClose{
			stripe: p.stripeConn,
			conn:   p.conn,
			udp:    p.udpConn,
		})
		// Nil out refs so onPathError won't double-close
		p.stripeConn = nil
		p.conn = nil
		p.dc = nil
		p.udpConn = nil
		p.alive = false
	}
	m.mu.Unlock()

	// Close outside lock — these calls unblock recvLoop goroutines
	for _, item := range items {
		if item.stripe != nil {
			_ = item.stripe.Close()
		}
		if item.conn != nil {
			_ = item.conn.CloseWithError(code, reason)
		}
		if item.udp != nil {
			_ = item.udp.Close()
		}
	}
}

func (m *multipathConn) snapshotDataplaneConfig() DataplaneConfig {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return cloneDataplaneConfig(m.cfg.Dataplane)
}

func (m *multipathConn) applyDataplaneConfig(dp DataplaneConfig) error {
	normalizeDataplaneConfig(&dp, m.cfg.MultipathPolicy)
	if err := validateDataplaneConfig(dp, m.cfg.MultipathPaths); err != nil {
		return err
	}
	compiled, err := compileDataplaneConfig(dp)
	if err != nil {
		return err
	}

	m.mu.Lock()
	m.cfg.Dataplane = cloneDataplaneConfig(dp)
	m.dataplane = compiled
	for className := range compiled.classes {
		if _, ok := m.classTx[className]; !ok {
			m.classTx[className] = &trafficClassCounters{}
		}
	}
	m.mu.Unlock()

	m.logger.Infof("dataplane policy applied classes=%d classifiers=%d", len(dp.Classes), len(dp.Classifiers))
	return nil
}

func (m *multipathConn) reloadDataplaneFromFile() error {
	path := strings.TrimSpace(m.cfg.DataplaneConfigFile)
	if path == "" {
		return fmt.Errorf("dataplane_config_file not configured")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	dp := DataplaneConfig{}
	if err := yaml.Unmarshal(b, &dp); err != nil {
		return err
	}
	return m.applyDataplaneConfig(dp)
}
