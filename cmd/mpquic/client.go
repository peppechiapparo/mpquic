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
	stripeConn       *stripeClientConn   // non-nil for stripe transport paths
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
}

type multipathConn struct {
	mu      sync.RWMutex
	paths   []*multipathPathState
	recvCh  chan []byte
	errCh   chan error
	errOnce sync.Once
	rr      int
	logger  *Logger
	cfg     *Config
	dataplane compiledDataplane
	classTx   map[string]*trafficClassCounters
	baseCtx context.Context
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

	var dc datagramConn
	if cfg.TransportMode == "reliable" {
		sc, err := openStreamConn(ctx, conn)
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

	return runTunnel(ctx, cfg, cc, logger)
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
		recvCh:  make(chan []byte, 512),
		errCh:   make(chan error, 1),
		logger:  logger,
		cfg:     cfg,
		dataplane: dpRuntime,
		classTx: make(map[string]*trafficClassCounters),
		baseCtx: ctx,
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
			sc, err := newStripeClientConn(ctx, cfg, p, keys, logger)
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

	go mp.telemetryLoop(ctx)

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
		return -1, nil
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
			sc, err := newStripeClientConn(ctx, m.cfg, pcfg, keys, m.logger)
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
		}
		m.mu.Unlock()

		m.logger.Infof("path recovered name=%s local=%s remote=%s", pcfg.Name, udpConn.LocalAddr(), remoteUDP.String())
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
