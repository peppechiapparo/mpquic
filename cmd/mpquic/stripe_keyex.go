package main

import (
	"context"
	"crypto/tls"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"strings"
	"time"

	quic "github.com/quic-go/quic-go"
)

// ── Stripe Key Exchange (QUIC TLS Exporter) ───────────────────────────────

// applyKexPQPolicy imposta le CurvePreferences del client KX secondo
// stripe_kex_pq. Il client controlla l'offerta: con "require" propone SOLO
// il gruppo ibrido post-quantum e l'handshake fallisce se il server non lo
// supporta (fail-closed: nessun downgrade silenzioso a classico). Con Go
// ≥1.24 il default già preferisce X25519MLKEM768: "" non tocca nulla,
// "prefer" lo rende esplicito, "off" forza il classico (solo per A/B).
func applyKexPQPolicy(tlsCfg *tls.Config, mode string) error {
	switch mode {
	case "", "default":
		return nil
	case "require":
		tlsCfg.CurvePreferences = []tls.CurveID{tls.X25519MLKEM768}
	case "prefer":
		tlsCfg.CurvePreferences = []tls.CurveID{tls.X25519MLKEM768, tls.X25519}
	case "off":
		tlsCfg.CurvePreferences = []tls.CurveID{tls.X25519}
	default:
		return fmt.Errorf("stripe_kex_pq: valore %q non valido (require|prefer|off)", mode)
	}
	return nil
}

// kexGroupName rende leggibile il gruppo TLS negoziato.
func kexGroupName(id tls.CurveID) string {
	if id == tls.X25519MLKEM768 {
		return "X25519MLKEM768"
	}
	return id.String()
}

// stripeNegotiateKey establishes a temporary QUIC connection to the server's
// QUIC port using ALPN "mpquic-stripe-kx", exports keying material from the
// TLS 1.3 session, and derives AES-256-GCM keys for stripe encryption.
func stripeNegotiateKey(ctx context.Context, cfg *Config, pathCfg MultipathPathConfig, sessionID uint32, logger *Logger) (*stripeKeyMaterial, error) {
	// Resolve remote address (same logic as newStripeClientConn)
	remoteHost := pathCfg.RemoteAddr
	if remoteHost == "" {
		remoteHost = cfg.RemoteAddr
	}
	remotePort := pathCfg.RemotePort
	if remotePort == 0 {
		remotePort = cfg.RemotePort
	}

	bindIP, err := resolveBindIP(pathCfg.BindIP)
	if err != nil {
		return nil, fmt.Errorf("stripe KX: bind resolve: %w", err)
	}
	var ifName string
	if strings.HasPrefix(pathCfg.BindIP, "if:") {
		ifName = strings.TrimPrefix(pathCfg.BindIP, "if:")
	}

	// Create UDP socket bound to the same interface as stripe pipes
	laddr := &net.UDPAddr{IP: net.ParseIP(bindIP), Port: 0}
	udpConn, err := net.ListenUDP("udp4", laddr)
	if err != nil {
		return nil, fmt.Errorf("stripe KX: listen: %w", err)
	}
	if ifName != "" {
		if err := bindPipeToDevice(udpConn, ifName); err != nil {
			logger.Errorf("stripe KX: SO_BINDTODEVICE to %s: %v (continuing)", ifName, err)
		}
	}

	// Load client TLS config with stripe KX ALPN
	tlsCfg, err := loadClientTLSConfig(cfg)
	if err != nil {
		udpConn.Close()
		return nil, fmt.Errorf("stripe KX: TLS config: %w", err)
	}
	tlsCfg.NextProtos = []string{stripeKXALPN}
	if err := applyKexPQPolicy(tlsCfg, cfg.StripeKexPQ); err != nil {
		udpConn.Close()
		return nil, err
	}

	// Dial QUIC for key exchange
	tr := &quic.Transport{Conn: udpConn}
	raddr, err := net.ResolveUDPAddr("udp", net.JoinHostPort(remoteHost, fmt.Sprintf("%d", remotePort)))
	if err != nil {
		tr.Close()
		return nil, fmt.Errorf("stripe KX: resolve: %w", err)
	}

	kxCtx, kxCancel := context.WithTimeout(ctx, 10*time.Second)
	defer kxCancel()

	conn, err := tr.Dial(kxCtx, raddr, tlsCfg, &quic.Config{
		MaxIdleTimeout: 10 * time.Second,
	})
	if err != nil {
		tr.Close()
		return nil, fmt.Errorf("stripe KX: QUIC dial: %w", err)
	}

	// Send session ID over a stream so server can associate the key
	stream, err := conn.OpenStreamSync(kxCtx)
	if err != nil {
		conn.CloseWithError(1, "stream open failed")
		tr.Close()
		return nil, fmt.Errorf("stripe KX: open stream: %w", err)
	}
	var sessBytes [4]byte
	binary.BigEndian.PutUint32(sessBytes[:], sessionID)
	if _, err := stream.Write(sessBytes[:]); err != nil {
		conn.CloseWithError(1, "write failed")
		tr.Close()
		return nil, fmt.Errorf("stripe KX: write session ID: %w", err)
	}

	// Wait for server ACK (1 byte) before closing — ensures the server has
	// accepted the stream and stored the key before we send CONNECTION_CLOSE.
	var ack [1]byte
	if _, err := io.ReadFull(stream, ack[:]); err != nil {
		conn.CloseWithError(1, "ack read failed")
		tr.Close()
		return nil, fmt.Errorf("stripe KX: server ack: %w", err)
	}
	stream.Close()

	// Export keying material from the TLS 1.3 session
	state := conn.ConnectionState()
	material, err := state.TLS.ExportKeyingMaterial(stripeKXLabel, sessBytes[:], 64)
	if err != nil {
		conn.CloseWithError(1, "export failed")
		tr.Close()
		return nil, fmt.Errorf("stripe KX: export key material: %w", err)
	}

	conn.CloseWithError(0, "kx done")
	tr.Close()

	km, err := stripeDeriveKeys(material)
	if err != nil {
		return nil, err
	}

	group := state.TLS.CurveID
	km.kexGroup = kexGroupName(group)
	km.kexPQ = group == tls.X25519MLKEM768
	if cfg.StripeKexPQ == "require" && !km.kexPQ {
		return nil, fmt.Errorf("stripe KX: stripe_kex_pq=require ma il gruppo negoziato è %s", km.kexGroup)
	}

	logger.Infof("stripe KX: session=%08x key negotiated via TLS exporter group=%s post-quantum=%v",
		sessionID, km.kexGroup, km.kexPQ)
	return km, nil
}

// handleStripeKeyExchange handles a QUIC connection with ALPN "mpquic-stripe-kx".
// It reads the stripe session ID, exports matching keying material, and stores
// the derived keys in the pending store for the stripe UDP listener to consume.
func handleStripeKeyExchange(conn quic.Connection, pendingKeys *stripePendingKeys, logger *Logger) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	stream, err := conn.AcceptStream(ctx)
	if err != nil {
		logger.Errorf("stripe KX: accept stream: %v", err)
		conn.CloseWithError(0, "kx done")
		return
	}

	var sessBytes [4]byte
	if _, err := io.ReadFull(stream, sessBytes[:]); err != nil {
		logger.Errorf("stripe KX: read session ID: %v", err)
		conn.CloseWithError(0, "kx done")
		return
	}
	sessionID := binary.BigEndian.Uint32(sessBytes[:])

	// Export same keying material (TLS session is shared → same output)
	state := conn.ConnectionState()
	material, err := state.TLS.ExportKeyingMaterial(stripeKXLabel, sessBytes[:], 64)
	logger.Infof("stripe KX (server): session=%08x group=%s post-quantum=%v",
		sessionID, kexGroupName(state.TLS.CurveID), state.TLS.CurveID == tls.X25519MLKEM768)
	if err != nil {
		logger.Errorf("stripe KX: export key material session=%08x: %v", sessionID, err)
		conn.CloseWithError(0, "kx done")
		return
	}

	km, err := stripeDeriveKeys(material)
	if err != nil {
		logger.Errorf("stripe KX: derive keys session=%08x: %v", sessionID, err)
		conn.CloseWithError(0, "kx done")
		return
	}

	pendingKeys.Store(sessionID, km)

	// Send 1-byte ACK so the client knows we stored the key.
	stream.Write([]byte{0x01})

	// Wait for the client to close its stream side (FIN) — this ensures
	// the ACK byte is delivered before we send CONNECTION_CLOSE.
	io.ReadAll(stream)
	stream.Close()

	logger.Infof("stripe KX: session=%08x key stored for pending REGISTER", sessionID)
	conn.CloseWithError(0, "kx done")
}
