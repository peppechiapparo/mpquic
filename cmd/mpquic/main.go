package main

import (
	"context"
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	_ "net/http/pprof"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/quic-go/quic-go"
)

// ─── Interfaces ───────────────────────────────────────────────────────────

type datagramConn interface {
	SendDatagram([]byte) error
	ReceiveDatagram(context.Context) ([]byte, error)
}

// txBatcher is an optional interface for datagramConn implementations that
// support batch TX via sendmmsg. drainSendCh uses type assertion to call
// FlushTxBatch after draining the send channel.
type txBatcher interface {
	FlushTxBatch()
}

// streamConn wraps a single bidirectional QUIC stream to provide reliable,
// ordered delivery with 2-byte length-prefixed framing.
// This allows the congestion control algorithm (BBR vs Cubic) to drive
// retransmissions, unlike QUIC DATAGRAM frames which are unreliable.
type streamConn struct {
	stream  quic.Stream
	writeMu sync.Mutex
}

func (s *streamConn) SendDatagram(pkt []byte) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	var hdr [2]byte
	binary.BigEndian.PutUint16(hdr[:], uint16(len(pkt)))
	if _, err := s.stream.Write(hdr[:]); err != nil {
		return err
	}
	_, err := s.stream.Write(pkt)
	return err
}

func (s *streamConn) ReceiveDatagram(_ context.Context) ([]byte, error) {
	var hdr [2]byte
	if _, err := io.ReadFull(s.stream, hdr[:]); err != nil {
		return nil, err
	}
	length := binary.BigEndian.Uint16(hdr[:])
	pkt := make([]byte, length)
	if _, err := io.ReadFull(s.stream, pkt); err != nil {
		return nil, err
	}
	return pkt, nil
}

func openStreamConn(ctx context.Context, conn quic.Connection) (*streamConn, error) {
	stream, err := conn.OpenStreamSync(ctx)
	if err != nil {
		return nil, fmt.Errorf("open stream: %w", err)
	}
	return &streamConn{stream: stream}, nil
}

func acceptStreamConn(ctx context.Context, conn quic.Connection) (*streamConn, error) {
	stream, err := conn.AcceptStream(ctx)
	if err != nil {
		return nil, fmt.Errorf("accept stream: %w", err)
	}
	return &streamConn{stream: stream}, nil
}

// connectionTable maps TUN peer IPs to their QUIC connection for multi-conn server.
// When a packet is read from the shared TUN, the dst IP is looked up to find the
// right QUIC connection for the return path.
//
// In addition to the primary peerIP (the TUN address), the table also learns
// "routed" source IPs that clients forward through the tunnel (e.g. LAN hosts
// behind the client). This allows return traffic to be dispatched to the correct
// QUIC connection even when the dst IP in the reply packet is not the peer's
// TUN address but a LAN host behind it.
//
// Multi-path support: a single peerIP may have multiple QUIC connections
// (one per WAN path). The table aggregates them in a connGroup and the

// ─── Logger ───────────────────────────────────────────────────────────────

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
	pprofAddr := flag.String("pprof", "", "pprof HTTP listen address (e.g. :6060)")
	flag.Parse()
	if *cfgPath == "" {
		log.Fatal("--config is required")
	}

	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		log.Fatalf("config error: %v", err)
	}

	logger := newLogger(cfg.LogLevel)

	// Optional pprof HTTP server for CPU/memory profiling (debug only, localhost)
	if *pprofAddr != "" {
		go func() {
			logger.Infof("pprof listening on %s", *pprofAddr)
			if err := http.ListenAndServe(*pprofAddr, nil); err != nil {
				logger.Errorf("pprof server: %v", err)
			}
		}()
	}

	// Default congestion algorithm to "cubic" and normalize
	cfg.CongestionAlgorithm = strings.ToLower(strings.TrimSpace(cfg.CongestionAlgorithm))
	if cfg.CongestionAlgorithm == "" {
		cfg.CongestionAlgorithm = "cubic"
	}
	cfg.TransportMode = strings.ToLower(strings.TrimSpace(cfg.TransportMode))
	if cfg.TransportMode == "" {
		cfg.TransportMode = "datagram"
	}
	logger.Infof("congestion_algorithm=%s transport_mode=%s", cfg.CongestionAlgorithm, cfg.TransportMode)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	// Log shutdown initiation and enforce hard deadline
	go func() {
		<-ctx.Done()
		logger.Infof("shutdown signal received, stopping...")
		time.AfterFunc(10*time.Second, func() {
			logger.Errorf("shutdown deadline exceeded, forcing exit")
			os.Exit(1)
		})
	}()

	// Start metrics server (bound to tunnel IP for security)
	if cfg.MetricsListen != "" {
		registerMetricsRole(cfg.Role)
		stopMetrics := startMetricsServer(ctx, cfg.MetricsListen, logger)
		defer stopMetrics()
	}

	if cfg.Role == "server" {
		err = runServer(ctx, cfg, logger)
	} else {
		err = runClientLoop(ctx, cfg, logger)
	}
	logger.Infof("clean exit")
	if err != nil && !errors.Is(err, context.Canceled) {
		log.Fatalf("fatal: %v", err)
	}
}
