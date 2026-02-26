package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/songgao/water"
	"gopkg.in/yaml.v3"
)

type Config struct {
	Role                  string                `yaml:"role"`
	BindIP                string                `yaml:"bind_ip"`
	RemoteAddr            string                `yaml:"remote_addr"`
	RemotePort            int                   `yaml:"remote_port"`
	MultipathEnabled      bool                  `yaml:"multipath_enabled"`
	MultipathPaths        []MultipathPathConfig `yaml:"multipath_paths"`
	TunName               string                `yaml:"tun_name"`
	TunCIDR               string                `yaml:"tun_cidr"`
	LogLevel              string                `yaml:"log_level"`
	TLSCertFile           string                `yaml:"tls_cert_file"`
	TLSKeyFile            string                `yaml:"tls_key_file"`
	TLSCAFile             string                `yaml:"tls_ca_file"`
	TLSServerName         string                `yaml:"tls_server_name"`
	TLSInsecureSkipVerify bool                  `yaml:"tls_insecure_skip_verify"`
}

type MultipathPathConfig struct {
	Name       string `yaml:"name"`
	BindIP     string `yaml:"bind_ip"`
	RemoteAddr string `yaml:"remote_addr"`
	RemotePort int    `yaml:"remote_port"`
	Priority   int    `yaml:"priority"`
	Weight     int    `yaml:"weight"`
}

type multipathPathState struct {
	cfg              MultipathPathConfig
	udpConn          *net.UDPConn
	transport        *quic.Transport
	conn             quic.Connection
	alive            bool
	reconnecting     bool
	consecutiveFails int
	cooldownUntil    time.Time
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
	baseCtx context.Context
}

type datagramConn interface {
	SendDatagram([]byte) error
	ReceiveDatagram(context.Context) ([]byte, error)
}

type Logger struct {
	level int
}

const (
	levelDebug = 10
	levelInfo  = 20
	levelError = 30
)

func newLogger(level string) *Logger {
	switch strings.ToLower(level) {
	case "debug":
		return &Logger{level: levelDebug}
	case "error":
		return &Logger{level: levelError}
	default:
		return &Logger{level: levelInfo}
	}
}

func (l *Logger) Debugf(format string, args ...any) {
	if l.level <= levelDebug {
		log.Printf("DEBUG "+format, args...)
	}
}

func (l *Logger) Infof(format string, args ...any) {
	if l.level <= levelInfo {
		log.Printf("INFO "+format, args...)
	}
}

func (l *Logger) Errorf(format string, args ...any) {
	if l.level <= levelError {
		log.Printf("ERROR "+format, args...)
	}
}

func main() {
	cfgPath := flag.String("config", "", "path to YAML config")
	flag.Parse()
	if *cfgPath == "" {
		log.Fatal("--config is required")
	}

	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		log.Fatalf("config error: %v", err)
	}

	logger := newLogger(cfg.LogLevel)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	if cfg.Role == "server" {
		err = runServer(ctx, cfg, logger)
	} else {
		err = runClientLoop(ctx, cfg, logger)
	}
	if err != nil && !errors.Is(err, context.Canceled) {
		log.Fatalf("fatal: %v", err)
	}
}

func loadConfig(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	cfg := &Config{}
	if err := yaml.Unmarshal(b, cfg); err != nil {
		return nil, err
	}
	cfg.Role = strings.ToLower(strings.TrimSpace(cfg.Role))
	cfg.LogLevel = strings.ToLower(strings.TrimSpace(cfg.LogLevel))
	if cfg.Role != "client" && cfg.Role != "server" {
		return nil, fmt.Errorf("role must be client or server")
	}
	if cfg.BindIP == "" && !(cfg.Role == "client" && cfg.MultipathEnabled) {
		return nil, fmt.Errorf("bind_ip required")
	}
	if !(cfg.Role == "client" && cfg.MultipathEnabled) {
		if cfg.RemotePort <= 0 || cfg.RemotePort > 65535 {
			return nil, fmt.Errorf("remote_port invalid")
		}
	}
	if cfg.Role == "client" && cfg.MultipathEnabled {
		if len(cfg.MultipathPaths) == 0 {
			return nil, fmt.Errorf("multipath_paths required when multipath_enabled=true")
		}
		for i := range cfg.MultipathPaths {
			p := &cfg.MultipathPaths[i]
			if p.Name == "" {
				p.Name = fmt.Sprintf("path%d", i+1)
			}
			if p.BindIP == "" {
				return nil, fmt.Errorf("multipath_paths[%d].bind_ip required", i)
			}
			if p.RemoteAddr == "" {
				return nil, fmt.Errorf("multipath_paths[%d].remote_addr required", i)
			}
			if p.RemotePort <= 0 || p.RemotePort > 65535 {
				return nil, fmt.Errorf("multipath_paths[%d].remote_port invalid", i)
			}
			if p.Weight <= 0 {
				p.Weight = 1
			}
		}
	}
	if cfg.TunName == "" {
		return nil, fmt.Errorf("tun_name required")
	}
	if cfg.TunCIDR == "" {
		return nil, fmt.Errorf("tun_cidr required")
	}
	if cfg.Role == "client" && !cfg.MultipathEnabled && cfg.RemoteAddr == "" {
		return nil, fmt.Errorf("remote_addr required for client")
	}
	if cfg.Role == "server" {
		if cfg.TLSCertFile == "" || cfg.TLSKeyFile == "" {
			return nil, fmt.Errorf("tls_cert_file and tls_key_file required for server")
		}
	}
	if cfg.Role == "client" {
		if !cfg.TLSInsecureSkipVerify && cfg.TLSCAFile == "" {
			return nil, fmt.Errorf("tls_ca_file required for client when tls_insecure_skip_verify=false")
		}
		if cfg.TLSServerName == "" {
			cfg.TLSServerName = "mpquic-server"
		}
	}
	if cfg.LogLevel == "" {
		cfg.LogLevel = "info"
	}
	return cfg, nil
}

func runServer(ctx context.Context, cfg *Config, logger *Logger) error {
	bindIP, err := resolveBindIP(cfg.BindIP)
	if err != nil {
		return err
	}
	tun, err := water.New(water.Config{DeviceType: water.TUN, PlatformSpecificParams: water.PlatformSpecificParams{Name: cfg.TunName}})
	if err != nil {
		return err
	}
	defer tun.Close()

	tlsConf, err := loadServerTLSConfig(cfg)
	if err != nil {
		return err
	}
	listenAddr := net.JoinHostPort(bindIP, fmt.Sprintf("%d", cfg.RemotePort))
	logger.Infof("server listen=%s tun=%s", listenAddr, cfg.TunName)
	listener, err := quic.ListenAddr(listenAddr, tlsConf, &quic.Config{
		EnableDatagrams: true,
		KeepAlivePeriod: 15 * time.Second,
		MaxIdleTimeout:  60 * time.Second,
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
			if err := runTunnelWithTUN(cctx, c, tun, logger); err != nil && !errors.Is(err, context.Canceled) {
				logger.Errorf("tunnel closed: %v", err)
			}
		}(conn, connCtx)
	}
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
		EnableDatagrams: true,
		KeepAlivePeriod: 15 * time.Second,
		MaxIdleTimeout:  60 * time.Second,
	})
	if err != nil {
		return err
	}
	defer conn.CloseWithError(0, "shutdown")

	logger.Infof("connected local=%s remote=%s tun=%s", udpConn.LocalAddr(), remoteUDP.String(), cfg.TunName)
	return runTunnel(ctx, cfg, conn, logger)
}

func runClientOnceMultipath(ctx context.Context, cfg *Config, logger *Logger) error {
	mpConn, err := newMultipathConn(ctx, cfg, logger)
	if err != nil {
		return err
	}
	defer mpConn.closeAll(0, "shutdown")

	logger.Infof("connected multipath paths=%d tun=%s", len(cfg.MultipathPaths), cfg.TunName)
	return runTunnel(ctx, cfg, mpConn, logger)
}

func newMultipathConn(ctx context.Context, cfg *Config, logger *Logger) (*multipathConn, error) {
	mp := &multipathConn{
		recvCh:  make(chan []byte, 512),
		errCh:   make(chan error, 1),
		logger:  logger,
		cfg:     cfg,
		baseCtx: ctx,
	}

	aliveCount := 0

	for _, p := range cfg.MultipathPaths {
		state := &multipathPathState{cfg: p}
		mp.paths = append(mp.paths, state)

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
			EnableDatagrams: true,
			KeepAlivePeriod: 15 * time.Second,
			MaxIdleTimeout:  60 * time.Second,
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
		state.alive = true
		state.reconnecting = false
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

	return mp, nil
}

func (m *multipathConn) SendDatagram(pkt []byte) error {
	deadline := time.Now().Add(1200 * time.Millisecond)
	for {
		idx, conn := m.selectBestPath()
		if idx < 0 || conn == nil {
			if time.Now().After(deadline) {
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
		return nil
	}
}

func (m *multipathConn) selectBestPath() (int, quic.Connection) {
	m.mu.Lock()
	defer m.mu.Unlock()

	now := time.Now()
	if len(m.paths) == 0 {
		return -1, nil
	}

	bestIdx := -1
	bestScore := int(^uint(0) >> 1)
	start := m.rr % len(m.paths)

	for i := 0; i < len(m.paths); i++ {
		idx := (start + i) % len(m.paths)
		p := m.paths[idx]
		if !p.alive || p.conn == nil {
			continue
		}
		if now.Before(p.cooldownUntil) {
			continue
		}
		score := p.cfg.Priority*1000 + p.consecutiveFails*100
		if p.cfg.Weight > 1 {
			score -= (p.cfg.Weight - 1) * 10
		}
		if score < bestScore {
			bestScore = score
			bestIdx = idx
		}
	}

	if bestIdx < 0 {
		return -1, nil
	}

	m.rr = (bestIdx + 1) % len(m.paths)
	return bestIdx, m.paths[bestIdx].conn
}

func (m *multipathConn) markTxSuccess(idx int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p := m.paths[idx]
	if p.consecutiveFails > 0 {
		p.consecutiveFails--
	}
}

func (m *multipathConn) markTxError(idx int, err error) {
	m.mu.Lock()
	p := m.paths[idx]
	p.consecutiveFails++
	if p.consecutiveFails > 6 {
		p.consecutiveFails = 6
	}
	p.cooldownUntil = time.Now().Add(time.Duration(p.consecutiveFails) * time.Second)
	p.alive = false
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

func (m *multipathConn) currentPathConn(idx int) quic.Connection {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if idx < 0 || idx >= len(m.paths) {
		return nil
	}
	return m.paths[idx].conn
}

func (m *multipathConn) onPathSuccess(idx int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p := m.paths[idx]
	if p.consecutiveFails > 0 {
		p.consecutiveFails--
	}
}

func (m *multipathConn) onPathError(ctx context.Context, idx int, err error) {
	m.mu.Lock()
	p := m.paths[idx]
	p.alive = false
	p.consecutiveFails++
	if p.consecutiveFails > 6 {
		p.consecutiveFails = 6
	}
	p.cooldownUntil = time.Now().Add(time.Duration(p.consecutiveFails) * time.Second)
	if p.conn != nil {
		_ = p.conn.CloseWithError(0, "rx-error")
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

		bindIP, err := resolveBindIP(pcfg.BindIP)
		if err != nil {
			m.logger.Errorf("path redial resolve failed name=%s err=%v", pcfg.Name, err)
			time.Sleep(2 * time.Second)
			continue
		}

		localUDP := &net.UDPAddr{IP: net.ParseIP(bindIP), Port: 0}
		udpConn, err := net.ListenUDP("udp", localUDP)
		if err != nil {
			m.logger.Errorf("path redial listen failed name=%s err=%v", pcfg.Name, err)
			time.Sleep(2 * time.Second)
			continue
		}

		remoteUDP, err := net.ResolveUDPAddr("udp", net.JoinHostPort(pcfg.RemoteAddr, fmt.Sprintf("%d", pcfg.RemotePort)))
		if err != nil {
			_ = udpConn.Close()
			m.logger.Errorf("path redial remote resolve failed name=%s err=%v", pcfg.Name, err)
			time.Sleep(2 * time.Second)
			continue
		}

		tlsConf, err := loadClientTLSConfig(m.cfg)
		if err != nil {
			_ = udpConn.Close()
			m.logger.Errorf("path redial tls failed name=%s err=%v", pcfg.Name, err)
			time.Sleep(2 * time.Second)
			continue
		}

		transport := quic.Transport{Conn: udpConn}
		dialCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
		conn, err := transport.Dial(dialCtx, remoteUDP, tlsConf, &quic.Config{
			EnableDatagrams: true,
			KeepAlivePeriod: 15 * time.Second,
			MaxIdleTimeout:  60 * time.Second,
		})
		cancel()
		if err != nil {
			_ = udpConn.Close()
			m.logger.Errorf("path redial failed name=%s err=%v", pcfg.Name, err)
			time.Sleep(2 * time.Second)
			continue
		}

		m.mu.Lock()
		if idx >= 0 && idx < len(m.paths) {
			p := m.paths[idx]
			p.conn = conn
			p.udpConn = udpConn
			p.transport = &transport
			p.alive = true
			p.reconnecting = false
			if p.consecutiveFails > 0 {
				p.consecutiveFails--
			}
		}
		m.mu.Unlock()

		m.logger.Infof("path recovered name=%s local=%s remote=%s", pcfg.Name, udpConn.LocalAddr(), remoteUDP.String())
		return
	}
}

func (m *multipathConn) closeAll(code quic.ApplicationErrorCode, reason string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, p := range m.paths {
		if p.conn != nil {
			_ = p.conn.CloseWithError(code, reason)
		}
		if p.udpConn != nil {
			_ = p.udpConn.Close()
		}
	}
}

func runTunnel(ctx context.Context, cfg *Config, conn datagramConn, logger *Logger) error {
	tun, err := water.New(water.Config{DeviceType: water.TUN, PlatformSpecificParams: water.PlatformSpecificParams{Name: cfg.TunName}})
	if err != nil {
		return err
	}
	defer tun.Close()

	return runTunnelWithTUN(ctx, conn, tun, logger)
}

func runTunnelWithTUN(ctx context.Context, conn datagramConn, tun *water.Interface, logger *Logger) error {

	errCh := make(chan error, 1)
	go func() {
		buf := make([]byte, 65535)
		for {
			n, err := tun.Read(buf)
			if err != nil {
				errCh <- err
				return
			}
			pkt := append([]byte(nil), buf[:n]...)
			if err := conn.SendDatagram(pkt); err != nil {
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
			return err
		}
		logger.Debugf("RX %d bytes", len(pkt))
	}
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

func loadServerTLSConfig(cfg *Config) (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(cfg.TLSCertFile, cfg.TLSKeyFile)
	if err != nil {
		return nil, err
	}
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		NextProtos:   []string{"mpquic-ip"},
		MinVersion:   tls.VersionTLS13,
	}, nil
}

func loadClientTLSConfig(cfg *Config) (*tls.Config, error) {
	tlsConf := &tls.Config{
		InsecureSkipVerify: cfg.TLSInsecureSkipVerify,
		ServerName:         cfg.TLSServerName,
		NextProtos:         []string{"mpquic-ip"},
		MinVersion:         tls.VersionTLS13,
	}
	if cfg.TLSCAFile != "" {
		caPEM, err := os.ReadFile(cfg.TLSCAFile)
		if err != nil {
			return nil, err
		}
		roots := x509.NewCertPool()
		if !roots.AppendCertsFromPEM(caPEM) {
			return nil, fmt.Errorf("failed loading tls_ca_file: %s", cfg.TLSCAFile)
		}
		tlsConf.RootCAs = roots
	}
	return tlsConf, nil
}
