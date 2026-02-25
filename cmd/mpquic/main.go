package main

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"log"
	"math/big"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/songgao/water"
	"gopkg.in/yaml.v3"
)

type Config struct {
	Role       string `yaml:"role"`
	BindIP     string `yaml:"bind_ip"`
	RemoteAddr string `yaml:"remote_addr"`
	RemotePort int    `yaml:"remote_port"`
	TunName    string `yaml:"tun_name"`
	TunCIDR    string `yaml:"tun_cidr"`
	LogLevel   string `yaml:"log_level"`
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
	if cfg.BindIP == "" {
		return nil, fmt.Errorf("bind_ip required")
	}
	if cfg.RemotePort <= 0 || cfg.RemotePort > 65535 {
		return nil, fmt.Errorf("remote_port invalid")
	}
	if cfg.TunName == "" {
		return nil, fmt.Errorf("tun_name required")
	}
	if cfg.TunCIDR == "" {
		return nil, fmt.Errorf("tun_cidr required")
	}
	if cfg.Role == "client" && cfg.RemoteAddr == "" {
		return nil, fmt.Errorf("remote_addr required for client")
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
	listenAddr := net.JoinHostPort(bindIP, fmt.Sprintf("%d", cfg.RemotePort))
	logger.Infof("server listen=%s tun=%s", listenAddr, cfg.TunName)
	listener, err := quic.ListenAddr(listenAddr, generateTLSConfig(), &quic.Config{
		EnableDatagrams: true,
		KeepAlivePeriod: 15 * time.Second,
		MaxIdleTimeout:  60 * time.Second,
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
		logger.Infof("accepted remote=%s", conn.RemoteAddr())
		if err := runTunnel(ctx, cfg, conn, logger); err != nil {
			logger.Errorf("tunnel closed: %v", err)
		}
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

	transport := quic.Transport{Conn: udpConn}
	conn, err := transport.Dial(ctx, remoteUDP, &tls.Config{
		InsecureSkipVerify: true,
		NextProtos:         []string{"mpquic-ip"},
	}, &quic.Config{
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

func runTunnel(ctx context.Context, cfg *Config, conn datagramConn, logger *Logger) error {
	tun, err := water.New(water.Config{DeviceType: water.TUN, PlatformSpecificParams: water.PlatformSpecificParams{Name: cfg.TunName}})
	if err != nil {
		return err
	}
	defer tun.Close()

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

func generateTLSConfig() *tls.Config {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	tmpl := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject: pkix.Name{CommonName: "mpquic"},
		NotBefore: time.Now().Add(-1 * time.Hour),
		NotAfter:  time.Now().Add(3650 * 24 * time.Hour),
		KeyUsage:  x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage: []x509.ExtKeyUsage{
			x509.ExtKeyUsageServerAuth,
		},
		BasicConstraintsValid: true,
	}
	certDER, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		panic(err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		panic(err)
	}
	return &tls.Config{Certificates: []tls.Certificate{cert}, NextProtos: []string{"mpquic-ip"}}
}
