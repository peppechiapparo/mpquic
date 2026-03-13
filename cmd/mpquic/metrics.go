package main

// metrics.go — Layer 1 (Collection) + Layer 2 (Export) for the MPQUIC
// observability framework. Provides:
//
//   GET /metrics       — Prometheus text exposition format
//   GET /api/v1/stats  — JSON structured output for portals / AI-ML
//
// Design principles:
//   - Zero-alloc in hot path: all counters use sync/atomic
//   - Zero-lock on read: snapshot iterates sessions under RLock only
//   - Dedicated http.Server bound to tunnel IP (not exposed to Internet)
//   - pprof stays on its own optional --pprof listener (localhost only)
//   - Gracefully returns empty output if no stripe server/client registered

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// ─── Global metrics registry ──────────────────────────────────────────────

var globalMetrics metricsRegistry

type metricsRegistry struct {
	mu           sync.RWMutex
	startTime    time.Time
	server       *stripeServer
	client       *multipathConn
	clientPaths  func() []*multipathPathState // snapshot under lock
}

func init() {
	globalMetrics.startTime = time.Now()
}

func registerMetricsServer(ss *stripeServer) {
	globalMetrics.mu.Lock()
	globalMetrics.server = ss
	globalMetrics.mu.Unlock()
}

func registerMetricsClient(mc *multipathConn) {
	globalMetrics.mu.Lock()
	globalMetrics.client = mc
	globalMetrics.clientPaths = func() []*multipathPathState {
		mc.mu.RLock()
		defer mc.mu.RUnlock()
		out := make([]*multipathPathState, len(mc.paths))
		copy(out, mc.paths)
		return out
	}
	globalMetrics.mu.Unlock()
}

// startMetricsServer launches a dedicated HTTP server for /metrics and
// /api/v1/stats on the given address (typically a tunnel IP like
// 10.200.17.254:9090). Returns a stop function.
func startMetricsServer(ctx context.Context, addr string, logger *Logger) func() {
	mux := http.NewServeMux()
	mux.HandleFunc("/metrics", handlePrometheus)
	mux.HandleFunc("/api/v1/stats", handleStatsJSON)

	server := &http.Server{
		Addr:    addr,
		Handler: mux,
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()

	go func() {
		logger.Infof("metrics server listening on %s", addr)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Errorf("metrics server: %v", err)
		}
	}()

	return func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}
}

// ─── Snapshot types (Layer 1 → Layer 2 boundary) ──────────────────────────

// SessionStats holds a point-in-time snapshot of one stripe session's counters.
type SessionStats struct {
	SessionID string `json:"session_id"`
	PeerIP    string `json:"peer_ip"`
	Pipes     int    `json:"pipes"`

	TxBytes  uint64 `json:"tx_bytes"`
	TxPkts   uint64 `json:"tx_pkts"`
	RxBytes  uint64 `json:"rx_bytes"`
	RxPkts   uint64 `json:"rx_pkts"`

	FECMode     string `json:"fec_mode"`
	AdaptiveM   int    `json:"adaptive_m"`
	FECEncoded  uint64 `json:"fec_encoded"`  // FEC groups encoded (TX)
	FECRecov    uint64 `json:"fec_recovered"`

	ARQNackSent    uint64 `json:"arq_nack_sent"`
	ARQRetxRecv    uint64 `json:"arq_retx_recv"`
	ARQDupFiltered uint64 `json:"arq_dup_filtered"`

	LossRate  uint32 `json:"loss_rate_pct"` // peer-reported 0-100
	UptimeSec float64 `json:"uptime_sec"`

	DecryptFail uint64 `json:"decrypt_fail"`
}

// PathStats holds a point-in-time snapshot of one multipath path (client).
type PathStats struct {
	Name      string `json:"name"`
	BindIP    string `json:"bind_ip"`
	Alive     bool   `json:"alive"`

	TxBytes uint64 `json:"tx_bytes"`
	TxPkts  uint64 `json:"tx_pkts"`
	RxBytes uint64 `json:"rx_bytes"`
	RxPkts  uint64 `json:"rx_pkts"`

	StripeTxBytes uint64 `json:"stripe_tx_bytes,omitempty"`
	StripeTxPkts  uint64 `json:"stripe_tx_pkts,omitempty"`
	StripeRxBytes uint64 `json:"stripe_rx_bytes,omitempty"`
	StripeRxPkts  uint64 `json:"stripe_rx_pkts,omitempty"`
	StripeFECRecov uint64 `json:"stripe_fec_recovered,omitempty"`
}

// GlobalStats is the top-level JSON response.
type GlobalStats struct {
	Role       string         `json:"role"` // "server" or "client"
	Version    string         `json:"version"`
	UptimeSec  float64        `json:"uptime_sec"`
	Sessions   []SessionStats `json:"sessions,omitempty"`
	Paths      []PathStats    `json:"paths,omitempty"`
	TotalTxBytes uint64       `json:"total_tx_bytes"`
	TotalRxBytes uint64       `json:"total_rx_bytes"`
	TotalTxPkts  uint64       `json:"total_tx_pkts"`
	TotalRxPkts  uint64       `json:"total_rx_pkts"`
}

// ─── Snapshot functions ───────────────────────────────────────────────────

func snapshotServerSessions(ss *stripeServer) []SessionStats {
	ss.mu.RLock()
	defer ss.mu.RUnlock()

	stats := make([]SessionStats, 0, len(ss.sessions))
	now := time.Now()
	for _, sess := range ss.sessions {
		s := SessionStats{
			SessionID: fmt.Sprintf("%08x", sess.sessionID),
			PeerIP:    sess.peerIP.String(),
			Pipes:     sess.registered,
			TxBytes:   atomic.LoadUint64(&sess.txBytes),
			TxPkts:    atomic.LoadUint64(&sess.txPkts),
			RxBytes:   atomic.LoadUint64(&sess.rxBytes),
			RxPkts:    atomic.LoadUint64(&sess.rxPkts),
			FECMode:   sess.fecMode,
			AdaptiveM: int(atomic.LoadInt32(&sess.adaptiveM)),
			FECEncoded: atomic.LoadUint64(&sess.fecEncoded),
			FECRecov:   atomic.LoadUint64(&sess.rxFECRecov),
			LossRate:   atomic.LoadUint32(&sess.peerLossRate),
			UptimeSec:  now.Sub(sess.createdAt).Seconds(),
			DecryptFail: atomic.LoadUint64(&sess.securityDecryptFail),
		}
		if sess.arqRx != nil {
			s.ARQNackSent, s.ARQRetxRecv, s.ARQDupFiltered = sess.arqRx.stats()
		}
		stats = append(stats, s)
	}
	return stats
}

func snapshotClientPaths(mc *multipathConn) []PathStats {
	mc.mu.RLock()
	defer mc.mu.RUnlock()

	stats := make([]PathStats, 0, len(mc.paths))
	for _, p := range mc.paths {
		ps := PathStats{
			Name:      p.cfg.Name,
			BindIP:    p.cfg.BindIP,
			Alive:     p.alive,
			TxPkts:    atomic.LoadUint64(&p.txPackets),
			RxPkts:    atomic.LoadUint64(&p.rxPackets),
		}
		if p.stripeConn != nil {
			ps.StripeTxBytes = atomic.LoadUint64(&p.stripeConn.txBytes)
			ps.StripeTxPkts = atomic.LoadUint64(&p.stripeConn.txPkts)
			ps.StripeRxBytes = atomic.LoadUint64(&p.stripeConn.rxBytes)
			ps.StripeRxPkts = atomic.LoadUint64(&p.stripeConn.rxPkts)
			ps.StripeFECRecov = atomic.LoadUint64(&p.stripeConn.fecRecov)
		}
		stats = append(stats, ps)
	}
	return stats
}

func buildGlobalStats() GlobalStats {
	globalMetrics.mu.RLock()
	ss := globalMetrics.server
	mc := globalMetrics.client
	start := globalMetrics.startTime
	globalMetrics.mu.RUnlock()

	gs := GlobalStats{
		Version:   "4.2",
		UptimeSec: time.Since(start).Seconds(),
	}

	if ss != nil {
		gs.Role = "server"
		gs.Sessions = snapshotServerSessions(ss)
		for _, s := range gs.Sessions {
			gs.TotalTxBytes += s.TxBytes
			gs.TotalRxBytes += s.RxBytes
			gs.TotalTxPkts += s.TxPkts
			gs.TotalRxPkts += s.RxPkts
		}
	}

	if mc != nil {
		gs.Role = "client"
		gs.Paths = snapshotClientPaths(mc)
		for _, p := range gs.Paths {
			gs.TotalTxPkts += p.TxPkts
			gs.TotalRxPkts += p.RxPkts
			gs.TotalTxBytes += p.StripeTxBytes
			gs.TotalRxBytes += p.StripeRxBytes
		}
	}

	return gs
}

// ─── Layer 2: HTTP handlers ──────────────────────────────────────────────

func handleStatsJSON(w http.ResponseWriter, r *http.Request) {
	gs := buildGlobalStats()
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(gs)
}

func handlePrometheus(w http.ResponseWriter, r *http.Request) {
	gs := buildGlobalStats()
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")

	// Global
	fmt.Fprintf(w, "# HELP mpquic_uptime_seconds Process uptime in seconds.\n")
	fmt.Fprintf(w, "# TYPE mpquic_uptime_seconds gauge\n")
	fmt.Fprintf(w, "mpquic_uptime_seconds %f\n\n", gs.UptimeSec)

	fmt.Fprintf(w, "# HELP mpquic_tx_bytes_total Total bytes transmitted.\n")
	fmt.Fprintf(w, "# TYPE mpquic_tx_bytes_total counter\n")
	fmt.Fprintf(w, "mpquic_tx_bytes_total %d\n\n", gs.TotalTxBytes)

	fmt.Fprintf(w, "# HELP mpquic_rx_bytes_total Total bytes received.\n")
	fmt.Fprintf(w, "# TYPE mpquic_rx_bytes_total counter\n")
	fmt.Fprintf(w, "mpquic_rx_bytes_total %d\n\n", gs.TotalRxBytes)

	fmt.Fprintf(w, "# HELP mpquic_tx_packets_total Total packets transmitted.\n")
	fmt.Fprintf(w, "# TYPE mpquic_tx_packets_total counter\n")
	fmt.Fprintf(w, "mpquic_tx_packets_total %d\n\n", gs.TotalTxPkts)

	fmt.Fprintf(w, "# HELP mpquic_rx_packets_total Total packets received.\n")
	fmt.Fprintf(w, "# TYPE mpquic_rx_packets_total counter\n")
	fmt.Fprintf(w, "mpquic_rx_packets_total %d\n\n", gs.TotalRxPkts)

	// Per-session (server)
	if len(gs.Sessions) > 0 {
		fmt.Fprintf(w, "# HELP mpquic_session_tx_bytes Bytes transmitted per session.\n")
		fmt.Fprintf(w, "# TYPE mpquic_session_tx_bytes counter\n")
		for _, s := range gs.Sessions {
			fmt.Fprintf(w, "mpquic_session_tx_bytes{session=\"%s\",peer=\"%s\"} %d\n", s.SessionID, s.PeerIP, s.TxBytes)
		}

		fmt.Fprintf(w, "\n# HELP mpquic_session_rx_bytes Bytes received per session.\n")
		fmt.Fprintf(w, "# TYPE mpquic_session_rx_bytes counter\n")
		for _, s := range gs.Sessions {
			fmt.Fprintf(w, "mpquic_session_rx_bytes{session=\"%s\",peer=\"%s\"} %d\n", s.SessionID, s.PeerIP, s.RxBytes)
		}

		fmt.Fprintf(w, "\n# HELP mpquic_session_tx_packets Packets transmitted per session.\n")
		fmt.Fprintf(w, "# TYPE mpquic_session_tx_packets counter\n")
		for _, s := range gs.Sessions {
			fmt.Fprintf(w, "mpquic_session_tx_packets{session=\"%s\",peer=\"%s\"} %d\n", s.SessionID, s.PeerIP, s.TxPkts)
		}

		fmt.Fprintf(w, "\n# HELP mpquic_session_rx_packets Packets received per session.\n")
		fmt.Fprintf(w, "# TYPE mpquic_session_rx_packets counter\n")
		for _, s := range gs.Sessions {
			fmt.Fprintf(w, "mpquic_session_rx_packets{session=\"%s\",peer=\"%s\"} %d\n", s.SessionID, s.PeerIP, s.RxPkts)
		}

		fmt.Fprintf(w, "\n# HELP mpquic_session_pipes Number of active pipes per session.\n")
		fmt.Fprintf(w, "# TYPE mpquic_session_pipes gauge\n")
		for _, s := range gs.Sessions {
			fmt.Fprintf(w, "mpquic_session_pipes{session=\"%s\",peer=\"%s\"} %d\n", s.SessionID, s.PeerIP, s.Pipes)
		}

		fmt.Fprintf(w, "\n# HELP mpquic_session_adaptive_m Current FEC parity M per session.\n")
		fmt.Fprintf(w, "# TYPE mpquic_session_adaptive_m gauge\n")
		for _, s := range gs.Sessions {
			fmt.Fprintf(w, "mpquic_session_adaptive_m{session=\"%s\",peer=\"%s\"} %d\n", s.SessionID, s.PeerIP, s.AdaptiveM)
		}

		fmt.Fprintf(w, "\n# HELP mpquic_session_fec_encoded FEC groups encoded (TX) per session.\n")
		fmt.Fprintf(w, "# TYPE mpquic_session_fec_encoded counter\n")
		for _, s := range gs.Sessions {
			fmt.Fprintf(w, "mpquic_session_fec_encoded{session=\"%s\",peer=\"%s\"} %d\n", s.SessionID, s.PeerIP, s.FECEncoded)
		}

		fmt.Fprintf(w, "\n# HELP mpquic_session_fec_recovered FEC groups recovered (RX) per session.\n")
		fmt.Fprintf(w, "# TYPE mpquic_session_fec_recovered counter\n")
		for _, s := range gs.Sessions {
			fmt.Fprintf(w, "mpquic_session_fec_recovered{session=\"%s\",peer=\"%s\"} %d\n", s.SessionID, s.PeerIP, s.FECRecov)
		}

		fmt.Fprintf(w, "\n# HELP mpquic_session_arq_nack_sent ARQ NACKs sent per session.\n")
		fmt.Fprintf(w, "# TYPE mpquic_session_arq_nack_sent counter\n")
		for _, s := range gs.Sessions {
			fmt.Fprintf(w, "mpquic_session_arq_nack_sent{session=\"%s\",peer=\"%s\"} %d\n", s.SessionID, s.PeerIP, s.ARQNackSent)
		}

		fmt.Fprintf(w, "\n# HELP mpquic_session_arq_retx_recv ARQ retransmissions received per session.\n")
		fmt.Fprintf(w, "# TYPE mpquic_session_arq_retx_recv counter\n")
		for _, s := range gs.Sessions {
			fmt.Fprintf(w, "mpquic_session_arq_retx_recv{session=\"%s\",peer=\"%s\"} %d\n", s.SessionID, s.PeerIP, s.ARQRetxRecv)
		}

		fmt.Fprintf(w, "\n# HELP mpquic_session_arq_dup_filtered Duplicate packets filtered per session.\n")
		fmt.Fprintf(w, "# TYPE mpquic_session_arq_dup_filtered counter\n")
		for _, s := range gs.Sessions {
			fmt.Fprintf(w, "mpquic_session_arq_dup_filtered{session=\"%s\",peer=\"%s\"} %d\n", s.SessionID, s.PeerIP, s.ARQDupFiltered)
		}

		fmt.Fprintf(w, "\n# HELP mpquic_session_loss_rate_pct Peer-reported loss rate percentage.\n")
		fmt.Fprintf(w, "# TYPE mpquic_session_loss_rate_pct gauge\n")
		for _, s := range gs.Sessions {
			fmt.Fprintf(w, "mpquic_session_loss_rate_pct{session=\"%s\",peer=\"%s\"} %d\n", s.SessionID, s.PeerIP, s.LossRate)
		}

		fmt.Fprintf(w, "\n# HELP mpquic_session_uptime_seconds Session uptime in seconds.\n")
		fmt.Fprintf(w, "# TYPE mpquic_session_uptime_seconds gauge\n")
		for _, s := range gs.Sessions {
			fmt.Fprintf(w, "mpquic_session_uptime_seconds{session=\"%s\",peer=\"%s\"} %f\n", s.SessionID, s.PeerIP, s.UptimeSec)
		}

		fmt.Fprintf(w, "\n# HELP mpquic_session_decrypt_fail Decryption failures per session.\n")
		fmt.Fprintf(w, "# TYPE mpquic_session_decrypt_fail counter\n")
		for _, s := range gs.Sessions {
			fmt.Fprintf(w, "mpquic_session_decrypt_fail{session=\"%s\",peer=\"%s\"} %d\n", s.SessionID, s.PeerIP, s.DecryptFail)
		}
		fmt.Fprintln(w)
	}

	// Per-path (client)
	if len(gs.Paths) > 0 {
		fmt.Fprintf(w, "# HELP mpquic_path_alive Whether the path is alive (1) or down (0).\n")
		fmt.Fprintf(w, "# TYPE mpquic_path_alive gauge\n")
		for _, p := range gs.Paths {
			v := 0
			if p.Alive {
				v = 1
			}
			fmt.Fprintf(w, "mpquic_path_alive{path=\"%s\",bind=\"%s\"} %d\n", p.Name, p.BindIP, v)
		}

		fmt.Fprintf(w, "\n# HELP mpquic_path_tx_packets Packets transmitted per path.\n")
		fmt.Fprintf(w, "# TYPE mpquic_path_tx_packets counter\n")
		for _, p := range gs.Paths {
			fmt.Fprintf(w, "mpquic_path_tx_packets{path=\"%s\",bind=\"%s\"} %d\n", p.Name, p.BindIP, p.TxPkts)
		}

		fmt.Fprintf(w, "\n# HELP mpquic_path_rx_packets Packets received per path.\n")
		fmt.Fprintf(w, "# TYPE mpquic_path_rx_packets counter\n")
		for _, p := range gs.Paths {
			fmt.Fprintf(w, "mpquic_path_rx_packets{path=\"%s\",bind=\"%s\"} %d\n", p.Name, p.BindIP, p.RxPkts)
		}

		fmt.Fprintf(w, "\n# HELP mpquic_path_stripe_tx_bytes Stripe bytes transmitted per path.\n")
		fmt.Fprintf(w, "# TYPE mpquic_path_stripe_tx_bytes counter\n")
		for _, p := range gs.Paths {
			fmt.Fprintf(w, "mpquic_path_stripe_tx_bytes{path=\"%s\",bind=\"%s\"} %d\n", p.Name, p.BindIP, p.StripeTxBytes)
		}

		fmt.Fprintf(w, "\n# HELP mpquic_path_stripe_rx_bytes Stripe bytes received per path.\n")
		fmt.Fprintf(w, "# TYPE mpquic_path_stripe_rx_bytes counter\n")
		for _, p := range gs.Paths {
			fmt.Fprintf(w, "mpquic_path_stripe_rx_bytes{path=\"%s\",bind=\"%s\"} %d\n", p.Name, p.BindIP, p.StripeRxBytes)
		}

		fmt.Fprintf(w, "\n# HELP mpquic_path_stripe_fec_recovered Stripe FEC groups recovered per path.\n")
		fmt.Fprintf(w, "# TYPE mpquic_path_stripe_fec_recovered counter\n")
		for _, p := range gs.Paths {
			fmt.Fprintf(w, "mpquic_path_stripe_fec_recovered{path=\"%s\",bind=\"%s\"} %d\n", p.Name, p.BindIP, p.StripeFECRecov)
		}
		fmt.Fprintln(w)
	}
}
