package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// ─── API Handler ──────────────────────────────────────────────────────────

type APIHandler struct {
	mgr       *InstanceManager
	authToken string
}

func NewAPIHandler(mgr *InstanceManager, authToken string) *APIHandler {
	return &APIHandler{mgr: mgr, authToken: authToken}
}

// ─── Auth Middleware ──────────────────────────────────────────────────────

func (h *APIHandler) authorize(w http.ResponseWriter, r *http.Request) bool {
	if h.authToken == "" {
		return true // no auth configured
	}
	expected := "Bearer " + h.authToken
	if r.Header.Get("Authorization") != expected {
		w.Header().Set("WWW-Authenticate", "Bearer")
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
	level := r.URL.Query().Get("level") // "" or "error"

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

	lines := 100
	if v := r.URL.Query().Get("lines"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 10000 {
			lines = n
		}
	}
	level := r.URL.Query().Get("level")

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

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PATCH, OPTIONS")
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
