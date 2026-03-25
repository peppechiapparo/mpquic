package main

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ─── API Handler ──────────────────────────────────────────────────────────

// validName matches only safe tunnel instance names: alphanumeric, hyphen, underscore, dot.
var validName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,63}$`)

// rateLimiter tracks failed auth attempts per IP for brute-force protection.
type rateLimiter struct {
	mu       sync.Mutex
	failures map[string][]time.Time // IP -> timestamps of recent failures
}

func newRateLimiter() *rateLimiter {
	rl := &rateLimiter{failures: make(map[string][]time.Time)}
	// Background cleanup of stale entries every 5 minutes
	go func() {
		for {
			time.Sleep(5 * time.Minute)
			rl.cleanup()
		}
	}()
	return rl
}

const (
	rateLimitWindow  = 5 * time.Minute
	rateLimitMaxFail = 10 // max failures per IP per window
)

func (rl *rateLimiter) isBlocked(ip string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	cutoff := time.Now().Add(-rateLimitWindow)
	recent := rl.pruneOld(ip, cutoff)
	return len(recent) >= rateLimitMaxFail
}

func (rl *rateLimiter) recordFailure(ip string) {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	rl.failures[ip] = append(rl.failures[ip], time.Now())
}

func (rl *rateLimiter) pruneOld(ip string, cutoff time.Time) []time.Time {
	entries := rl.failures[ip]
	var recent []time.Time
	for _, t := range entries {
		if t.After(cutoff) {
			recent = append(recent, t)
		}
	}
	rl.failures[ip] = recent
	return recent
}

func (rl *rateLimiter) cleanup() {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	cutoff := time.Now().Add(-rateLimitWindow)
	for ip := range rl.failures {
		rl.pruneOld(ip, cutoff)
		if len(rl.failures[ip]) == 0 {
			delete(rl.failures, ip)
		}
	}
}

type APIHandler struct {
	mgr            *InstanceManager
	authTokenHash  []byte   // stored as bytes for constant-time compare
	allowedOrigins []string // empty = no CORS
	rl             *rateLimiter
}

func NewAPIHandler(mgr *InstanceManager, authToken string, corsOrigins string) *APIHandler {
	var origins []string
	if corsOrigins != "" {
		for _, o := range strings.Split(corsOrigins, ",") {
			o = strings.TrimSpace(o)
			if o != "" {
				origins = append(origins, o)
			}
		}
	}
	return &APIHandler{
		mgr:            mgr,
		authTokenHash:  []byte("Bearer " + authToken),
		allowedOrigins: origins,
		rl:             newRateLimiter(),
	}
}

// ─── Auth Middleware ──────────────────────────────────────────────────────

// clientIP extracts the remote IP (without port) from the request.
func clientIP(r *http.Request) string {
	ip := r.RemoteAddr
	if idx := strings.LastIndex(ip, ":"); idx != -1 {
		ip = ip[:idx]
	}
	return strings.Trim(ip, "[]")
}

func (h *APIHandler) authorize(w http.ResponseWriter, r *http.Request) bool {
	// Handle CORS preflight (OPTIONS) — allow without auth
	if r.Method == http.MethodOptions {
		h.setCORSHeaders(w, r)
		w.WriteHeader(http.StatusNoContent)
		return false // signal caller to stop (preflight handled)
	}

	ip := clientIP(r)

	// Rate limit check — block IPs with too many recent failures
	if h.rl.isBlocked(ip) {
		log.Printf("SECURITY: rate-limited ip=%s", ip)
		w.Header().Set("Retry-After", "300")
		writeJSON(w, http.StatusTooManyRequests, map[string]any{"error": "too many failed attempts, retry later"})
		return false
	}

	// Constant-time comparison to prevent timing attacks
	provided := []byte(r.Header.Get("Authorization"))
	if subtle.ConstantTimeCompare(provided, h.authTokenHash) != 1 {
		h.rl.recordFailure(ip)
		log.Printf("SECURITY: auth failure ip=%s ua=%s", ip, r.UserAgent())
		w.Header().Set("WWW-Authenticate", `Bearer realm="mpquic-mgmt"`)
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
		return false
	}

	return true
}

// ─── Health ───────────────────────────────────────────────────────────────

func (h *APIHandler) HandleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	if !h.authorize(w, r) {
		return
	}

	tunnels, err := h.mgr.ListInstances()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}

	running := 0
	stopped := 0
	failed := 0
	for _, t := range tunnels {
		switch t.Status {
		case "running":
			running++
		case "stopped":
			stopped++
		case "failed":
			failed++
		}
	}

	hostname, _ := os.Hostname()

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":             true,
		"version":        version,
		"hostname":       hostname,
		"os":             runtime.GOOS,
		"arch":           runtime.GOARCH,
		"tunnels_total":  len(tunnels),
		"tunnels_running": running,
		"tunnels_stopped": stopped,
		"tunnels_failed": failed,
		"timestamp":      time.Now().UTC().Format(time.RFC3339),
	})
}

// ─── Tunnels List ─────────────────────────────────────────────────────────

func (h *APIHandler) HandleTunnels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	if !h.authorize(w, r) {
		return
	}

	tunnels, err := h.mgr.ListInstances()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"tunnels": tunnels})
}

// ─── Tunnel Routes (/api/v1/tunnels/{name}[/action]) ──────────────────────

func (h *APIHandler) HandleTunnelRoutes(w http.ResponseWriter, r *http.Request) {
	if !h.authorize(w, r) {
		return
	}

	// Parse: /api/v1/tunnels/{name}[/{action}]
	path := strings.TrimPrefix(r.URL.Path, "/api/v1/tunnels/")
	parts := strings.SplitN(path, "/", 2)
	name := parts[0]
	action := ""
	if len(parts) > 1 {
		action = parts[1]
	}

	if name == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "tunnel name required"})
		return
	}

	switch action {
	case "":
		h.handleTunnelDetail(w, r, name)
	case "start":
		h.handleTunnelAction(w, r, name, "start")
	case "stop":
		h.handleTunnelAction(w, r, name, "stop")
	case "restart":
		h.handleTunnelAction(w, r, name, "restart")
	case "config":
		h.handleTunnelConfig(w, r, name)
	case "config/validate":
		h.handleTunnelConfigValidate(w, r, name)
	case "metrics":
		h.handleTunnelMetrics(w, r, name)
	case "logs":
		h.handleTunnelLogs(w, r, name)
	default:
		writeJSON(w, http.StatusNotFound, map[string]any{"error": fmt.Sprintf("unknown action: %s", action)})
	}
}

// GET /api/v1/tunnels/{name}
func (h *APIHandler) handleTunnelDetail(w http.ResponseWriter, r *http.Request, name string) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}

	inst, err := h.mgr.GetInstance(name)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": err.Error()})
		return
	}

	writeJSON(w, http.StatusOK, inst)
}

// POST /api/v1/tunnels/{name}/{start|stop|restart}
func (h *APIHandler) handleTunnelAction(w http.ResponseWriter, r *http.Request, name, action string) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}

	log.Printf("API: %s tunnel=%s remote=%s", action, name, r.RemoteAddr)

	var err error
	switch action {
	case "start":
		err = h.mgr.StartTunnel(name)
	case "stop":
		err = h.mgr.StopTunnel(name)
	case "restart":
		err = h.mgr.RestartTunnel(name)
	}

	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"error":  err.Error(),
			"action": action,
			"tunnel": name,
		})
		return
	}

	// Wait briefly for systemd to settle, then return new status
	time.Sleep(500 * time.Millisecond)

	inst, _ := h.mgr.GetInstance(name)
	status := "unknown"
	if inst != nil {
		status = inst.Status
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":     true,
		"action": action,
		"tunnel": name,
		"status": status,
	})
}

// GET/PATCH /api/v1/tunnels/{name}/config
func (h *APIHandler) handleTunnelConfig(w http.ResponseWriter, r *http.Request, name string) {
	switch r.Method {
	case http.MethodGet:
		cfg, err := h.mgr.GetConfig(name)
		if err != nil {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": err.Error()})
			return
		}
		// Annotate fields with their category for the UI
		writeJSON(w, http.StatusOK, map[string]any{
			"tunnel": name,
			"config": cfg,
			"param_categories": paramClassification,
		})

	case http.MethodPatch:
		body, err := io.ReadAll(io.LimitReader(r.Body, 512*1024))
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "read body: " + err.Error()})
			return
		}

		var patch map[string]interface{}
		if err := json.Unmarshal(body, &patch); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid JSON: " + err.Error()})
			return
		}

		if len(patch) == 0 {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "empty patch"})
			return
		}

		log.Printf("API: config patch tunnel=%s fields=%v remote=%s", name, keys(patch), r.RemoteAddr)

		needsRestart, err := h.mgr.ApplyConfigPatch(name, patch)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}

		result := map[string]any{
			"ok":              true,
			"tunnel":          name,
			"fields_applied":  keys(patch),
			"needs_restart":   needsRestart,
			"restart_applied": false,
		}

		// Auto-restart if needed and requested
		autoRestart := r.URL.Query().Get("auto_restart") == "true"
		if needsRestart && autoRestart {
			if err := h.mgr.RestartTunnel(name); err != nil {
				result["restart_error"] = err.Error()
			} else {
				result["restart_applied"] = true
				time.Sleep(500 * time.Millisecond)
			}
		}

		writeJSON(w, http.StatusOK, result)

	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
	}
}

// POST /api/v1/tunnels/{name}/config/validate
func (h *APIHandler) handleTunnelConfigValidate(w http.ResponseWriter, r *http.Request, name string) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 512*1024))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "read body: " + err.Error()})
		return
	}

	var patch map[string]interface{}
	if err := json.Unmarshal(body, &patch); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid JSON: " + err.Error()})
		return
	}

	needsRestart, blocked, err := h.mgr.ValidateConfigPatch(name, patch)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"ok":              false,
			"error":           err.Error(),
			"blocked_fields":  blocked,
			"needs_restart":   needsRestart,
		})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":            true,
		"needs_restart": needsRestart,
	})
}

// GET /api/v1/tunnels/{name}/metrics
func (h *APIHandler) handleTunnelMetrics(w http.ResponseWriter, r *http.Request, name string) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}

	data, err := h.mgr.FetchMetrics(name)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"error":  err.Error(),
			"tunnel": name,
		})
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write(data)
}

// GET /api/v1/tunnels/{name}/logs?lines=100&level=error
func (h *APIHandler) handleTunnelLogs(w http.ResponseWriter, r *http.Request, name string) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}

	lines := 100
	if v := r.URL.Query().Get("lines"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 10000 {
			lines = n
		}
	}
	// Sanitize level: only allow known safe values
	level := ""
	switch r.URL.Query().Get("level") {
	case "error", "ERROR":
		level = "error"
	case "warn", "WARN", "warning", "WARNING":
		level = "warning"
	}

	output, err := h.mgr.FetchLogs(name, lines, level)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"tunnel": name,
		"lines":  lines,
		"level":  level,
		"output": output,
	})
}

// ─── Aggregated Metrics ───────────────────────────────────────────────────

func (h *APIHandler) HandleMetricsAll(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	if !h.authorize(w, r) {
		return
	}

	data, err := h.mgr.FetchAllMetrics()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"tunnels": data})
}

// ─── System Info ──────────────────────────────────────────────────────────

func (h *APIHandler) HandleSystemInfo(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	if !h.authorize(w, r) {
		return
	}

	hostname, _ := os.Hostname()

	// Get mpquic binary version
	mpquicVersion := "unknown"
	if out, err := exec.Command("/usr/local/bin/mpquic", "--version").CombinedOutput(); err == nil {
		mpquicVersion = strings.TrimSpace(string(out))
	}

	// Git info
	gitCommit := "unknown"
	if out, err := exec.Command("git", "-C", "/opt/mpquic", "log", "--oneline", "-1").CombinedOutput(); err == nil {
		gitCommit = strings.TrimSpace(string(out))
	}

	// System uptime
	uptimeStr := "unknown"
	if out, err := exec.Command("uptime", "-p").CombinedOutput(); err == nil {
		uptimeStr = strings.TrimSpace(string(out))
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"mgmt_version":   version,
		"mpquic_version": mpquicVersion,
		"git_commit":     gitCommit,
		"go_version":     runtime.Version(),
		"hostname":       hostname,
		"os":             runtime.GOOS,
		"arch":           runtime.GOARCH,
		"num_cpu":        runtime.NumCPU(),
		"uptime":         uptimeStr,
		"timestamp":      time.Now().UTC().Format(time.RFC3339),
	})
}

// ─── System Logs ──────────────────────────────────────────────────────────

func (h *APIHandler) HandleSystemLogs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	if !h.authorize(w, r) {
		return
	}

	// Parse: /api/v1/system/logs/{name}
	name := strings.TrimPrefix(r.URL.Path, "/api/v1/system/logs/")
	if name == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "tunnel name required"})
		return
	}

	// Input validation: prevent command injection
	if !validName.MatchString(name) {
		log.Printf("SECURITY: invalid tunnel name=%q remote=%s", name, r.RemoteAddr)
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid tunnel name"})
		return
	}

	lines := 100
	if v := r.URL.Query().Get("lines"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 10000 {
			lines = n
		}
	}
	// Sanitize level: only allow known safe values
	level := ""
	switch r.URL.Query().Get("level") {
	case "error", "ERROR":
		level = "error"
	case "warn", "WARN", "warning", "WARNING":
		level = "warning"
	}

	output, err := h.mgr.FetchLogs(name, lines, level)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"tunnel": name,
		"lines":  lines,
		"level":  level,
		"output": output,
	})
}

// ─── Helpers ──────────────────────────────────────────────────────────────

func (h *APIHandler) setCORSHeaders(w http.ResponseWriter, r *http.Request) {
	origin := r.Header.Get("Origin")
	if origin == "" || len(h.allowedOrigins) == 0 {
		return
	}
	for _, allowed := range h.allowedOrigins {
		if origin == allowed {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PATCH, OPTIONS")
			w.Header().Set("Access-Control-Max-Age", "86400")
			w.Header().Set("Vary", "Origin")
			return
		}
	}
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	// Security headers
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Strict-Transport-Security", "max-age=63072000; includeSubDomains")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(payload)
}

func keys(m map[string]interface{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
