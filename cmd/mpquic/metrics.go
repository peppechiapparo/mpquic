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
	role         string // "server" or "client"
	server       *stripeServer
	client       *multipathConn
	clientPaths  func() []*multipathPathState // snapshot under lock
}

func init() {
	globalMetrics.startTime = time.Now()
}

func registerMetricsRole(role string) {
	globalMetrics.mu.Lock()
	globalMetrics.role = role
	globalMetrics.mu.Unlock()
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
	FECType     string `json:"fec_type,omitempty"`
	AdaptiveM   int    `json:"adaptive_m"`
	XorActive   int    `json:"xor_active"`                   // 1=XOR TX on, 0=off (adaptive gate)
	FECEncoded  uint64 `json:"fec_encoded"`  // FEC groups encoded (TX)
	FECRecov    uint64 `json:"fec_recovered"`
	TxtimeGapNs int64  `json:"txtime_gap_ns,omitempty"`

	XorEmitted      uint64 `json:"xor_emitted,omitempty"`       // XOR repair packets sent
	XorRecovered    uint64 `json:"xor_recovered,omitempty"`     // packets recovered via XOR
	XorUnrecoverable uint64 `json:"xor_unrecoverable,omitempty"` // multi-loss windows (fell back to ARQ)
	XorEffectivenessPct float64 `json:"xor_effectiveness_pct,omitempty"`
	XorWindow       int    `json:"xor_window,omitempty"`
	XorStride       int    `json:"xor_stride,omitempty"`
	XorRxCapacity   int    `json:"xor_rx_capacity,omitempty"`
	RLCActive       int    `json:"rlc_active,omitempty"`
	RLCEmitted      uint64 `json:"rlc_emitted,omitempty"`
	RLCRecovered    uint64 `json:"rlc_recovered,omitempty"`
	RLCDecodeFailures uint64 `json:"rlc_decode_failures,omitempty"`
	RLCEffectivenessPct float64 `json:"rlc_effectiveness_pct,omitempty"`
	RLCWindow       int    `json:"rlc_window,omitempty"`
	RLCStride       int    `json:"rlc_stride,omitempty"`
	RLCRxCapacity   int    `json:"rlc_rx_capacity,omitempty"`

	ARQNackSent    uint64 `json:"arq_nack_sent"`
	ARQRetxRecv    uint64 `json:"arq_retx_recv"`
	ARQDupFiltered uint64 `json:"arq_dup_filtered"`
	ARQNackThresh  uint32 `json:"arq_nack_thresh,omitempty"`
	ARQMaxOOO      uint32 `json:"arq_max_ooo,omitempty"`
	ARQPendingSpan uint32 `json:"arq_pending_span,omitempty"`

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
	StripeAdaptiveM      int    `json:"stripe_adaptive_m,omitempty"`
	StripePeerLossRate   uint32 `json:"stripe_peer_loss_rate_pct,omitempty"`
	StripeTxtimeGapNs    int64  `json:"stripe_txtime_gap_ns,omitempty"`
	StripeXorActive      int    `json:"stripe_xor_active,omitempty"`
	StripeXorEmitted     uint64 `json:"stripe_xor_emitted,omitempty"`
	StripeXorRecovered   uint64 `json:"stripe_xor_recovered,omitempty"`
	StripeXorUnrecoverable uint64 `json:"stripe_xor_unrecoverable,omitempty"`
	StripeXorEffectivenessPct float64 `json:"stripe_xor_effectiveness_pct,omitempty"`
	StripeXorWindow      int    `json:"stripe_xor_window,omitempty"`
	StripeXorStride      int    `json:"stripe_xor_stride,omitempty"`
	StripeXorRxCapacity  int    `json:"stripe_xor_rx_capacity,omitempty"`
	StripeRLCActive      int    `json:"stripe_rlc_active,omitempty"`
	StripeRLCEmitted     uint64 `json:"stripe_rlc_emitted,omitempty"`
	StripeRLCRecovered   uint64 `json:"stripe_rlc_recovered,omitempty"`
	StripeRLCDecodeFailures uint64 `json:"stripe_rlc_decode_failures,omitempty"`
	StripeRLCEffectivenessPct float64 `json:"stripe_rlc_effectiveness_pct,omitempty"`
	StripeRLCWindow      int    `json:"stripe_rlc_window,omitempty"`
	StripeRLCStride      int    `json:"stripe_rlc_stride,omitempty"`
	StripeRLCRxCapacity  int    `json:"stripe_rlc_rx_capacity,omitempty"`
	StripeARQNackSent    uint64 `json:"stripe_arq_nack_sent,omitempty"`
	StripeARQRetxRecv    uint64 `json:"stripe_arq_retx_recv,omitempty"`
	StripeARQDupFiltered uint64 `json:"stripe_arq_dup_filtered,omitempty"`
	StripeARQNackThresh  uint32 `json:"stripe_arq_nack_thresh,omitempty"`
	StripeARQMaxOOO      uint32 `json:"stripe_arq_max_ooo,omitempty"`
	StripeARQPendingSpan uint32 `json:"stripe_arq_pending_span,omitempty"`
}

// GlobalStats is the top-level JSON response.
type GlobalStats struct {
	Role       string         `json:"role"` // "server" or "client"
	Version    string         `json:"version"`
	UptimeSec  float64        `json:"uptime_sec"`
	Sessions   []SessionStats `json:"sessions,omitempty"`
	Paths      []PathStats    `json:"paths,omitempty"`
	Dispatch   []DispatchPathStats `json:"dispatch,omitempty"`
	TotalTxBytes uint64       `json:"total_tx_bytes"`
	TotalRxBytes uint64       `json:"total_rx_bytes"`
	TotalTxPkts  uint64       `json:"total_tx_pkts"`
	TotalRxPkts  uint64       `json:"total_rx_pkts"`
}

// DispatchPathStats holds aggregated dispatch metrics for a path index.
// Aggregated across all connGroups in the connectionTable.
type DispatchPathStats struct {
	PathIdx      int    `json:"path_idx"`
	RemoteAddr   string `json:"remote_addr"`
	DispatchHit  uint64 `json:"dispatch_hit"`
	DispatchDrop uint64 `json:"dispatch_drop"`
	SendQueueLen int    `json:"send_queue_len"`
	FlowCount    int    `json:"flow_count"` // number of flows assigned to this path
}

// ─── Snapshot functions ───────────────────────────────────────────────────

func snapshotServerSessions(ss *stripeServer) []SessionStats {
	ss.mu.RLock()
	defer ss.mu.RUnlock()

	stats := make([]SessionStats, 0, len(ss.sessions))
	now := time.Now()
	for _, sess := range ss.sessions {
		activePipes := 0
		for _, p := range sess.pipes {
			if p != nil {
				activePipes++
			}
		}
		s := SessionStats{
			SessionID: fmt.Sprintf("%08x", sess.sessionID),
			PeerIP:    sess.peerIP.String(),
			Pipes:     activePipes,
			TxBytes:   atomic.LoadUint64(&sess.txBytes),
			TxPkts:    atomic.LoadUint64(&sess.txPkts),
			RxBytes:   atomic.LoadUint64(&sess.rxBytes),
			RxPkts:    atomic.LoadUint64(&sess.rxPkts),
			FECMode:   sess.fecMode,
			FECType:   sess.fecType,
			AdaptiveM: int(atomic.LoadInt32(&sess.adaptiveM)),
			XorActive: int(atomic.LoadInt32(&sess.xorActive)),
			RLCActive: int(atomic.LoadInt32(&sess.rlcActive)),
			FECEncoded: atomic.LoadUint64(&sess.fecEncoded),
			FECRecov:   atomic.LoadUint64(&sess.rxFECRecov),
			TxtimeGapNs: atomic.LoadInt64(&sess.txtimeGapNs),
			LossRate:   atomic.LoadUint32(&sess.peerLossRate),
			UptimeSec:  now.Sub(sess.createdAt).Seconds(),
			DecryptFail: atomic.LoadUint64(&sess.securityDecryptFail),
		}
		if sess.xorTx != nil {
			s.XorEmitted = atomic.LoadUint64(&sess.xorTx.emitted)
			s.XorWindow, s.XorStride, _ = sess.xorTx.stats()
		}
		if sess.xorRx != nil {
			s.XorRecovered = atomic.LoadUint64(&sess.xorRx.recovered)
			s.XorUnrecoverable = atomic.LoadUint64(&sess.xorRx.unrecoverable)
			_, s.XorRxCapacity = sess.xorRx.stats()
		}
		if s.XorEmitted > 0 {
			s.XorEffectivenessPct = (float64(s.XorRecovered) * 100.0) / float64(s.XorEmitted)
		}
		if sess.rlcTx != nil {
			s.RLCEmitted = atomic.LoadUint64(&sess.rlcTx.emitted)
			s.RLCWindow, s.RLCStride, _ = sess.rlcTx.stats()
		}
		if sess.rlcRx != nil {
			s.RLCRecovered = atomic.LoadUint64(&sess.rlcRx.recovered)
			s.RLCDecodeFailures = atomic.LoadUint64(&sess.rlcRx.decodeFailures)
			_, s.RLCRxCapacity = sess.rlcRx.stats()
		}
		if s.RLCEmitted > 0 {
			s.RLCEffectivenessPct = (float64(s.RLCRecovered) * 100.0) / float64(s.RLCEmitted)
		}
		if sess.arqRx != nil {
			s.ARQNackSent, s.ARQRetxRecv, s.ARQDupFiltered = sess.arqRx.stats()
			s.ARQNackThresh, s.ARQMaxOOO, s.ARQPendingSpan = sess.arqRx.dynamicStats()
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
			ps.StripeAdaptiveM = int(atomic.LoadInt32(&p.stripeConn.adaptiveM))
			ps.StripePeerLossRate = atomic.LoadUint32(&p.stripeConn.peerLossRate)
			ps.StripeTxtimeGapNs = atomic.LoadInt64(&p.stripeConn.txtimeGapNs)
			ps.StripeXorActive = int(atomic.LoadInt32(&p.stripeConn.xorActive))
			ps.StripeRLCActive = int(atomic.LoadInt32(&p.stripeConn.rlcActive))
			if p.stripeConn.xorTx != nil {
				ps.StripeXorEmitted = atomic.LoadUint64(&p.stripeConn.xorTx.emitted)
				ps.StripeXorWindow, ps.StripeXorStride, _ = p.stripeConn.xorTx.stats()
			}
			if p.stripeConn.xorRx != nil {
				ps.StripeXorRecovered = atomic.LoadUint64(&p.stripeConn.xorRx.recovered)
				ps.StripeXorUnrecoverable = atomic.LoadUint64(&p.stripeConn.xorRx.unrecoverable)
				_, ps.StripeXorRxCapacity = p.stripeConn.xorRx.stats()
			}
			if ps.StripeXorEmitted > 0 {
				ps.StripeXorEffectivenessPct = (float64(ps.StripeXorRecovered) * 100.0) / float64(ps.StripeXorEmitted)
			}
			if p.stripeConn.rlcTx != nil {
				ps.StripeRLCEmitted = atomic.LoadUint64(&p.stripeConn.rlcTx.emitted)
				ps.StripeRLCWindow, ps.StripeRLCStride, _ = p.stripeConn.rlcTx.stats()
			}
			if p.stripeConn.rlcRx != nil {
				ps.StripeRLCRecovered = atomic.LoadUint64(&p.stripeConn.rlcRx.recovered)
				ps.StripeRLCDecodeFailures = atomic.LoadUint64(&p.stripeConn.rlcRx.decodeFailures)
				_, ps.StripeRLCRxCapacity = p.stripeConn.rlcRx.stats()
			}
			if ps.StripeRLCEmitted > 0 {
				ps.StripeRLCEffectivenessPct = (float64(ps.StripeRLCRecovered) * 100.0) / float64(ps.StripeRLCEmitted)
			}
			if p.stripeConn.arqRx != nil {
				ps.StripeARQNackSent, ps.StripeARQRetxRecv, ps.StripeARQDupFiltered = p.stripeConn.arqRx.stats()
				ps.StripeARQNackThresh, ps.StripeARQMaxOOO, ps.StripeARQPendingSpan = p.stripeConn.arqRx.dynamicStats()
			}
		}
		stats = append(stats, ps)
	}
	return stats
}

// snapshotDispatchStats aggregates per-path dispatch metrics from the
// connectionTable. Walks all connGroups under ct.mu.RLock and builds
// per-pathIdx stats including dispatched packets, drops, bytes, queue
// depth and flow count.
func snapshotDispatchStats(ss *stripeServer) []DispatchPathStats {
	if ss == nil || ss.ct == nil {
		return nil
	}
	ct := ss.ct
	ct.mu.RLock()
	defer ct.mu.RUnlock()

	// Aggregate per remoteAddr across all groups.
	type agg struct {
		idx      int
		remote   string
		hit      uint64
		drop     uint64
		qLen     int
		flows    int
	}
	byRemote := make(map[string]*agg)

	for _, grp := range ct.byIP {
		// Count flows per pathIdx
		flowCounts := make(map[int]int, len(grp.paths))
		for _, fpIdx := range grp.flowPaths {
			flowCounts[fpIdx]++
		}
		for i, pc := range grp.paths {
			if pc == nil {
				continue
			}
			key := pc.remoteAddr
			a, ok := byRemote[key]
			if !ok {
				a = &agg{idx: i, remote: key}
				byRemote[key] = a
			}
			a.hit += atomic.LoadUint64(&pc.dispatchHit)
			a.drop += atomic.LoadUint64(&pc.dispatchDrop)
			a.qLen += len(pc.sendCh)
			a.flows += flowCounts[i]
		}
	}

	stats := make([]DispatchPathStats, 0, len(byRemote))
	for _, a := range byRemote {
		stats = append(stats, DispatchPathStats{
			PathIdx:      a.idx,
			RemoteAddr:   a.remote,
			DispatchHit:  a.hit,
			DispatchDrop: a.drop,
			SendQueueLen: a.qLen,
			FlowCount:    a.flows,
		})
	}
	return stats
}

func buildGlobalStats() GlobalStats {
	globalMetrics.mu.RLock()
	role := globalMetrics.role
	ss := globalMetrics.server
	mc := globalMetrics.client
	start := globalMetrics.startTime
	globalMetrics.mu.RUnlock()

	gs := GlobalStats{
		Role:      role,
		Version:   "4.2",
		UptimeSec: time.Since(start).Seconds(),
	}

	if ss != nil {
		gs.Sessions = snapshotServerSessions(ss)
		gs.Dispatch = snapshotDispatchStats(ss)
		for _, s := range gs.Sessions {
			gs.TotalTxBytes += s.TxBytes
			gs.TotalRxBytes += s.RxBytes
			gs.TotalTxPkts += s.TxPkts
			gs.TotalRxPkts += s.RxPkts
		}
	}

	if mc != nil {
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

		fmt.Fprintf(w, "\n# HELP mpquic_session_adaptive_m Current FEC parity M per session (RS mode).\n")
		fmt.Fprintf(w, "# TYPE mpquic_session_adaptive_m gauge\n")
		for _, s := range gs.Sessions {
			fmt.Fprintf(w, "mpquic_session_adaptive_m{session=\"%s\",peer=\"%s\"} %d\n", s.SessionID, s.PeerIP, s.AdaptiveM)
		}

		fmt.Fprintf(w, "\n# HELP mpquic_session_xor_active XOR FEC adaptive gate per session (1=ON, 0=OFF).\n")
		fmt.Fprintf(w, "# TYPE mpquic_session_xor_active gauge\n")
		for _, s := range gs.Sessions {
			fmt.Fprintf(w, "mpquic_session_xor_active{session=\"%s\",peer=\"%s\"} %d\n", s.SessionID, s.PeerIP, s.XorActive)
		}

		fmt.Fprintf(w, "\n# HELP mpquic_session_rlc_active RLC FEC adaptive gate per session (1=ON, 0=OFF).\n")
		fmt.Fprintf(w, "# TYPE mpquic_session_rlc_active gauge\n")
		for _, s := range gs.Sessions {
			fmt.Fprintf(w, "mpquic_session_rlc_active{session=\"%s\",peer=\"%s\"} %d\n", s.SessionID, s.PeerIP, s.RLCActive)
		}

		fmt.Fprintf(w, "\n# HELP mpquic_session_txtime_gap_ns Current kernel pacing gap in nanoseconds per session.\n")
		fmt.Fprintf(w, "# TYPE mpquic_session_txtime_gap_ns gauge\n")
		for _, s := range gs.Sessions {
			fmt.Fprintf(w, "mpquic_session_txtime_gap_ns{session=\"%s\",peer=\"%s\"} %d\n", s.SessionID, s.PeerIP, s.TxtimeGapNs)
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

		fmt.Fprintf(w, "\n# HELP mpquic_session_xor_effectiveness_pct XOR recovery effectiveness (recovered/emitted*100).\n")
		fmt.Fprintf(w, "# TYPE mpquic_session_xor_effectiveness_pct gauge\n")
		for _, s := range gs.Sessions {
			fmt.Fprintf(w, "mpquic_session_xor_effectiveness_pct{session=\"%s\",peer=\"%s\"} %.6f\n", s.SessionID, s.PeerIP, s.XorEffectivenessPct)
		}

		fmt.Fprintf(w, "\n# HELP mpquic_session_xor_window Current XOR protection window per session.\n")
		fmt.Fprintf(w, "# TYPE mpquic_session_xor_window gauge\n")
		for _, s := range gs.Sessions {
			fmt.Fprintf(w, "mpquic_session_xor_window{session=\"%s\",peer=\"%s\"} %d\n", s.SessionID, s.PeerIP, s.XorWindow)
		}

		fmt.Fprintf(w, "\n# HELP mpquic_session_xor_stride Current XOR repair stride per session.\n")
		fmt.Fprintf(w, "# TYPE mpquic_session_xor_stride gauge\n")
		for _, s := range gs.Sessions {
			fmt.Fprintf(w, "mpquic_session_xor_stride{session=\"%s\",peer=\"%s\"} %d\n", s.SessionID, s.PeerIP, s.XorStride)
		}

		fmt.Fprintf(w, "\n# HELP mpquic_session_xor_rx_capacity Current XOR receiver history capacity per session.\n")
		fmt.Fprintf(w, "# TYPE mpquic_session_xor_rx_capacity gauge\n")
		for _, s := range gs.Sessions {
			fmt.Fprintf(w, "mpquic_session_xor_rx_capacity{session=\"%s\",peer=\"%s\"} %d\n", s.SessionID, s.PeerIP, s.XorRxCapacity)
		}

		fmt.Fprintf(w, "\n# HELP mpquic_session_rlc_emitted RLC repair packets emitted per session.\n")
		fmt.Fprintf(w, "# TYPE mpquic_session_rlc_emitted counter\n")
		for _, s := range gs.Sessions {
			fmt.Fprintf(w, "mpquic_session_rlc_emitted{session=\"%s\",peer=\"%s\"} %d\n", s.SessionID, s.PeerIP, s.RLCEmitted)
		}

		fmt.Fprintf(w, "\n# HELP mpquic_session_rlc_recovered Packets recovered via RLC per session.\n")
		fmt.Fprintf(w, "# TYPE mpquic_session_rlc_recovered counter\n")
		for _, s := range gs.Sessions {
			fmt.Fprintf(w, "mpquic_session_rlc_recovered{session=\"%s\",peer=\"%s\"} %d\n", s.SessionID, s.PeerIP, s.RLCRecovered)
		}

		fmt.Fprintf(w, "\n# HELP mpquic_session_rlc_decode_failures RLC decode failures per session.\n")
		fmt.Fprintf(w, "# TYPE mpquic_session_rlc_decode_failures counter\n")
		for _, s := range gs.Sessions {
			fmt.Fprintf(w, "mpquic_session_rlc_decode_failures{session=\"%s\",peer=\"%s\"} %d\n", s.SessionID, s.PeerIP, s.RLCDecodeFailures)
		}

		fmt.Fprintf(w, "\n# HELP mpquic_session_rlc_effectiveness_pct RLC recovery effectiveness (recovered/emitted*100).\n")
		fmt.Fprintf(w, "# TYPE mpquic_session_rlc_effectiveness_pct gauge\n")
		for _, s := range gs.Sessions {
			fmt.Fprintf(w, "mpquic_session_rlc_effectiveness_pct{session=\"%s\",peer=\"%s\"} %.6f\n", s.SessionID, s.PeerIP, s.RLCEffectivenessPct)
		}

		fmt.Fprintf(w, "\n# HELP mpquic_session_rlc_window Current RLC protection window per session.\n")
		fmt.Fprintf(w, "# TYPE mpquic_session_rlc_window gauge\n")
		for _, s := range gs.Sessions {
			fmt.Fprintf(w, "mpquic_session_rlc_window{session=\"%s\",peer=\"%s\"} %d\n", s.SessionID, s.PeerIP, s.RLCWindow)
		}

		fmt.Fprintf(w, "\n# HELP mpquic_session_rlc_stride Current RLC repair stride per session.\n")
		fmt.Fprintf(w, "# TYPE mpquic_session_rlc_stride gauge\n")
		for _, s := range gs.Sessions {
			fmt.Fprintf(w, "mpquic_session_rlc_stride{session=\"%s\",peer=\"%s\"} %d\n", s.SessionID, s.PeerIP, s.RLCStride)
		}

		fmt.Fprintf(w, "\n# HELP mpquic_session_rlc_rx_capacity Current RLC receiver history capacity per session.\n")
		fmt.Fprintf(w, "# TYPE mpquic_session_rlc_rx_capacity gauge\n")
		for _, s := range gs.Sessions {
			fmt.Fprintf(w, "mpquic_session_rlc_rx_capacity{session=\"%s\",peer=\"%s\"} %d\n", s.SessionID, s.PeerIP, s.RLCRxCapacity)
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

		fmt.Fprintf(w, "\n# HELP mpquic_session_arq_nack_thresh Current adaptive ARQ NACK threshold per session.\n")
		fmt.Fprintf(w, "# TYPE mpquic_session_arq_nack_thresh gauge\n")
		for _, s := range gs.Sessions {
			fmt.Fprintf(w, "mpquic_session_arq_nack_thresh{session=\"%s\",peer=\"%s\"} %d\n", s.SessionID, s.PeerIP, s.ARQNackThresh)
		}

		fmt.Fprintf(w, "\n# HELP mpquic_session_arq_max_ooo Current max observed out-of-order distance per session.\n")
		fmt.Fprintf(w, "# TYPE mpquic_session_arq_max_ooo gauge\n")
		for _, s := range gs.Sessions {
			fmt.Fprintf(w, "mpquic_session_arq_max_ooo{session=\"%s\",peer=\"%s\"} %d\n", s.SessionID, s.PeerIP, s.ARQMaxOOO)
		}

		fmt.Fprintf(w, "\n# HELP mpquic_session_arq_pending_span Current receive gap span tracked by ARQ (highest-base).\n")
		fmt.Fprintf(w, "# TYPE mpquic_session_arq_pending_span gauge\n")
		for _, s := range gs.Sessions {
			fmt.Fprintf(w, "mpquic_session_arq_pending_span{session=\"%s\",peer=\"%s\"} %d\n", s.SessionID, s.PeerIP, s.ARQPendingSpan)
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

		fmt.Fprintf(w, "\n# HELP mpquic_session_decrypt_fail_total Decryption failures per session.\n")
		fmt.Fprintf(w, "# TYPE mpquic_session_decrypt_fail_total counter\n")
		for _, s := range gs.Sessions {
			fmt.Fprintf(w, "mpquic_session_decrypt_fail_total{session=\"%s\",peer=\"%s\"} %d\n", s.SessionID, s.PeerIP, s.DecryptFail)
		}
		fmt.Fprintln(w)
	}

	// Dispatch scheduler metrics (server side, per-path)
	if len(gs.Dispatch) > 0 {
		fmt.Fprintf(w, "# HELP mpquic_dispatch_hit_total Packets successfully queued to a path via dispatch.\n")
		fmt.Fprintf(w, "# TYPE mpquic_dispatch_hit_total counter\n")
		for _, d := range gs.Dispatch {
			fmt.Fprintf(w, "mpquic_dispatch_hit_total{remote=\"%s\"} %d\n", d.RemoteAddr, d.DispatchHit)
		}

		fmt.Fprintf(w, "\n# HELP mpquic_dispatch_drop_total Packets dropped (sendCh full) via dispatch.\n")
		fmt.Fprintf(w, "# TYPE mpquic_dispatch_drop_total counter\n")
		for _, d := range gs.Dispatch {
			fmt.Fprintf(w, "mpquic_dispatch_drop_total{remote=\"%s\"} %d\n", d.RemoteAddr, d.DispatchDrop)
		}

		fmt.Fprintf(w, "\n# HELP mpquic_dispatch_queue_len Current sendCh queue depth.\n")
		fmt.Fprintf(w, "# TYPE mpquic_dispatch_queue_len gauge\n")
		for _, d := range gs.Dispatch {
			fmt.Fprintf(w, "mpquic_dispatch_queue_len{remote=\"%s\"} %d\n", d.RemoteAddr, d.SendQueueLen)
		}

		fmt.Fprintf(w, "\n# HELP mpquic_dispatch_flow_count Active flows assigned to this path.\n")
		fmt.Fprintf(w, "# TYPE mpquic_dispatch_flow_count gauge\n")
		for _, d := range gs.Dispatch {
			fmt.Fprintf(w, "mpquic_dispatch_flow_count{remote=\"%s\"} %d\n", d.RemoteAddr, d.FlowCount)
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

		fmt.Fprintf(w, "\n# HELP mpquic_path_stripe_adaptive_m Current adaptive parity M per client stripe path.\n")
		fmt.Fprintf(w, "# TYPE mpquic_path_stripe_adaptive_m gauge\n")
		for _, p := range gs.Paths {
			fmt.Fprintf(w, "mpquic_path_stripe_adaptive_m{path=\"%s\",bind=\"%s\"} %d\n", p.Name, p.BindIP, p.StripeAdaptiveM)
		}

		fmt.Fprintf(w, "\n# HELP mpquic_path_stripe_peer_loss_rate_pct Peer-reported loss rate for the client stripe path.\n")
		fmt.Fprintf(w, "# TYPE mpquic_path_stripe_peer_loss_rate_pct gauge\n")
		for _, p := range gs.Paths {
			fmt.Fprintf(w, "mpquic_path_stripe_peer_loss_rate_pct{path=\"%s\",bind=\"%s\"} %d\n", p.Name, p.BindIP, p.StripePeerLossRate)
		}

		fmt.Fprintf(w, "\n# HELP mpquic_path_stripe_txtime_gap_ns Current kernel pacing gap in nanoseconds per client stripe path.\n")
		fmt.Fprintf(w, "# TYPE mpquic_path_stripe_txtime_gap_ns gauge\n")
		for _, p := range gs.Paths {
			fmt.Fprintf(w, "mpquic_path_stripe_txtime_gap_ns{path=\"%s\",bind=\"%s\"} %d\n", p.Name, p.BindIP, p.StripeTxtimeGapNs)
		}

		fmt.Fprintf(w, "\n# HELP mpquic_path_stripe_xor_active XOR FEC adaptive gate per path (1=ON, 0=OFF).\n")
		fmt.Fprintf(w, "# TYPE mpquic_path_stripe_xor_active gauge\n")
		for _, p := range gs.Paths {
			fmt.Fprintf(w, "mpquic_path_stripe_xor_active{path=\"%s\",bind=\"%s\"} %d\n", p.Name, p.BindIP, p.StripeXorActive)
		}

		fmt.Fprintf(w, "\n# HELP mpquic_path_stripe_rlc_active RLC FEC adaptive gate per path (1=ON, 0=OFF).\n")
		fmt.Fprintf(w, "# TYPE mpquic_path_stripe_rlc_active gauge\n")
		for _, p := range gs.Paths {
			fmt.Fprintf(w, "mpquic_path_stripe_rlc_active{path=\"%s\",bind=\"%s\"} %d\n", p.Name, p.BindIP, p.StripeRLCActive)
		}

		fmt.Fprintf(w, "\n# HELP mpquic_path_stripe_xor_emitted XOR FEC repair packets emitted per path.\n")
		fmt.Fprintf(w, "# TYPE mpquic_path_stripe_xor_emitted counter\n")
		for _, p := range gs.Paths {
			fmt.Fprintf(w, "mpquic_path_stripe_xor_emitted{path=\"%s\",bind=\"%s\"} %d\n", p.Name, p.BindIP, p.StripeXorEmitted)
		}

		fmt.Fprintf(w, "\n# HELP mpquic_path_stripe_xor_recovered Packets recovered via XOR FEC per path.\n")
		fmt.Fprintf(w, "# TYPE mpquic_path_stripe_xor_recovered counter\n")
		for _, p := range gs.Paths {
			fmt.Fprintf(w, "mpquic_path_stripe_xor_recovered{path=\"%s\",bind=\"%s\"} %d\n", p.Name, p.BindIP, p.StripeXorRecovered)
		}

		fmt.Fprintf(w, "\n# HELP mpquic_path_stripe_xor_unrecoverable XOR FEC multi-loss windows (ARQ fallback) per path.\n")
		fmt.Fprintf(w, "# TYPE mpquic_path_stripe_xor_unrecoverable counter\n")
		for _, p := range gs.Paths {
			fmt.Fprintf(w, "mpquic_path_stripe_xor_unrecoverable{path=\"%s\",bind=\"%s\"} %d\n", p.Name, p.BindIP, p.StripeXorUnrecoverable)
		}

		fmt.Fprintf(w, "\n# HELP mpquic_path_stripe_xor_effectiveness_pct XOR recovery effectiveness per client stripe path (recovered/emitted*100).\n")
		fmt.Fprintf(w, "# TYPE mpquic_path_stripe_xor_effectiveness_pct gauge\n")
		for _, p := range gs.Paths {
			fmt.Fprintf(w, "mpquic_path_stripe_xor_effectiveness_pct{path=\"%s\",bind=\"%s\"} %.6f\n", p.Name, p.BindIP, p.StripeXorEffectivenessPct)
		}

		fmt.Fprintf(w, "\n# HELP mpquic_path_stripe_xor_window Current XOR protection window per client stripe path.\n")
		fmt.Fprintf(w, "# TYPE mpquic_path_stripe_xor_window gauge\n")
		for _, p := range gs.Paths {
			fmt.Fprintf(w, "mpquic_path_stripe_xor_window{path=\"%s\",bind=\"%s\"} %d\n", p.Name, p.BindIP, p.StripeXorWindow)
		}

		fmt.Fprintf(w, "\n# HELP mpquic_path_stripe_xor_stride Current XOR repair stride per client stripe path.\n")
		fmt.Fprintf(w, "# TYPE mpquic_path_stripe_xor_stride gauge\n")
		for _, p := range gs.Paths {
			fmt.Fprintf(w, "mpquic_path_stripe_xor_stride{path=\"%s\",bind=\"%s\"} %d\n", p.Name, p.BindIP, p.StripeXorStride)
		}

		fmt.Fprintf(w, "\n# HELP mpquic_path_stripe_xor_rx_capacity Current XOR receiver history capacity per client stripe path.\n")
		fmt.Fprintf(w, "# TYPE mpquic_path_stripe_xor_rx_capacity gauge\n")
		for _, p := range gs.Paths {
			fmt.Fprintf(w, "mpquic_path_stripe_xor_rx_capacity{path=\"%s\",bind=\"%s\"} %d\n", p.Name, p.BindIP, p.StripeXorRxCapacity)
		}

		fmt.Fprintf(w, "\n# HELP mpquic_path_stripe_rlc_emitted RLC repair packets emitted per client stripe path.\n")
		fmt.Fprintf(w, "# TYPE mpquic_path_stripe_rlc_emitted counter\n")
		for _, p := range gs.Paths {
			fmt.Fprintf(w, "mpquic_path_stripe_rlc_emitted{path=\"%s\",bind=\"%s\"} %d\n", p.Name, p.BindIP, p.StripeRLCEmitted)
		}

		fmt.Fprintf(w, "\n# HELP mpquic_path_stripe_rlc_recovered Packets recovered via RLC per client stripe path.\n")
		fmt.Fprintf(w, "# TYPE mpquic_path_stripe_rlc_recovered counter\n")
		for _, p := range gs.Paths {
			fmt.Fprintf(w, "mpquic_path_stripe_rlc_recovered{path=\"%s\",bind=\"%s\"} %d\n", p.Name, p.BindIP, p.StripeRLCRecovered)
		}

		fmt.Fprintf(w, "\n# HELP mpquic_path_stripe_rlc_decode_failures RLC decode failures per client stripe path.\n")
		fmt.Fprintf(w, "# TYPE mpquic_path_stripe_rlc_decode_failures counter\n")
		for _, p := range gs.Paths {
			fmt.Fprintf(w, "mpquic_path_stripe_rlc_decode_failures{path=\"%s\",bind=\"%s\"} %d\n", p.Name, p.BindIP, p.StripeRLCDecodeFailures)
		}

		fmt.Fprintf(w, "\n# HELP mpquic_path_stripe_rlc_effectiveness_pct RLC recovery effectiveness per client stripe path (recovered/emitted*100).\n")
		fmt.Fprintf(w, "# TYPE mpquic_path_stripe_rlc_effectiveness_pct gauge\n")
		for _, p := range gs.Paths {
			fmt.Fprintf(w, "mpquic_path_stripe_rlc_effectiveness_pct{path=\"%s\",bind=\"%s\"} %.6f\n", p.Name, p.BindIP, p.StripeRLCEffectivenessPct)
		}

		fmt.Fprintf(w, "\n# HELP mpquic_path_stripe_rlc_window Current RLC protection window per client stripe path.\n")
		fmt.Fprintf(w, "# TYPE mpquic_path_stripe_rlc_window gauge\n")
		for _, p := range gs.Paths {
			fmt.Fprintf(w, "mpquic_path_stripe_rlc_window{path=\"%s\",bind=\"%s\"} %d\n", p.Name, p.BindIP, p.StripeRLCWindow)
		}

		fmt.Fprintf(w, "\n# HELP mpquic_path_stripe_rlc_stride Current RLC repair stride per client stripe path.\n")
		fmt.Fprintf(w, "# TYPE mpquic_path_stripe_rlc_stride gauge\n")
		for _, p := range gs.Paths {
			fmt.Fprintf(w, "mpquic_path_stripe_rlc_stride{path=\"%s\",bind=\"%s\"} %d\n", p.Name, p.BindIP, p.StripeRLCStride)
		}

		fmt.Fprintf(w, "\n# HELP mpquic_path_stripe_rlc_rx_capacity Current RLC receiver history capacity per client stripe path.\n")
		fmt.Fprintf(w, "# TYPE mpquic_path_stripe_rlc_rx_capacity gauge\n")
		for _, p := range gs.Paths {
			fmt.Fprintf(w, "mpquic_path_stripe_rlc_rx_capacity{path=\"%s\",bind=\"%s\"} %d\n", p.Name, p.BindIP, p.StripeRLCRxCapacity)
		}

		fmt.Fprintf(w, "\n# HELP mpquic_path_stripe_arq_nack_sent ARQ NACKs sent per client stripe path.\n")
		fmt.Fprintf(w, "# TYPE mpquic_path_stripe_arq_nack_sent counter\n")
		for _, p := range gs.Paths {
			fmt.Fprintf(w, "mpquic_path_stripe_arq_nack_sent{path=\"%s\",bind=\"%s\"} %d\n", p.Name, p.BindIP, p.StripeARQNackSent)
		}

		fmt.Fprintf(w, "\n# HELP mpquic_path_stripe_arq_retx_recv ARQ retransmissions received per client stripe path.\n")
		fmt.Fprintf(w, "# TYPE mpquic_path_stripe_arq_retx_recv counter\n")
		for _, p := range gs.Paths {
			fmt.Fprintf(w, "mpquic_path_stripe_arq_retx_recv{path=\"%s\",bind=\"%s\"} %d\n", p.Name, p.BindIP, p.StripeARQRetxRecv)
		}

		fmt.Fprintf(w, "\n# HELP mpquic_path_stripe_arq_dup_filtered Duplicate packets filtered per client stripe path.\n")
		fmt.Fprintf(w, "# TYPE mpquic_path_stripe_arq_dup_filtered counter\n")
		for _, p := range gs.Paths {
			fmt.Fprintf(w, "mpquic_path_stripe_arq_dup_filtered{path=\"%s\",bind=\"%s\"} %d\n", p.Name, p.BindIP, p.StripeARQDupFiltered)
		}

		fmt.Fprintf(w, "\n# HELP mpquic_path_stripe_arq_nack_thresh Current adaptive ARQ NACK threshold per client stripe path.\n")
		fmt.Fprintf(w, "# TYPE mpquic_path_stripe_arq_nack_thresh gauge\n")
		for _, p := range gs.Paths {
			fmt.Fprintf(w, "mpquic_path_stripe_arq_nack_thresh{path=\"%s\",bind=\"%s\"} %d\n", p.Name, p.BindIP, p.StripeARQNackThresh)
		}

		fmt.Fprintf(w, "\n# HELP mpquic_path_stripe_arq_max_ooo Current max observed out-of-order distance per client stripe path.\n")
		fmt.Fprintf(w, "# TYPE mpquic_path_stripe_arq_max_ooo gauge\n")
		for _, p := range gs.Paths {
			fmt.Fprintf(w, "mpquic_path_stripe_arq_max_ooo{path=\"%s\",bind=\"%s\"} %d\n", p.Name, p.BindIP, p.StripeARQMaxOOO)
		}

		fmt.Fprintf(w, "\n# HELP mpquic_path_stripe_arq_pending_span Current receive gap span tracked by ARQ per client stripe path.\n")
		fmt.Fprintf(w, "# TYPE mpquic_path_stripe_arq_pending_span gauge\n")
		for _, p := range gs.Paths {
			fmt.Fprintf(w, "mpquic_path_stripe_arq_pending_span{path=\"%s\",bind=\"%s\"} %d\n", p.Name, p.BindIP, p.StripeARQPendingSpan)
		}
		fmt.Fprintln(w)
	}
}
