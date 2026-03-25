package main

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

// ─── Tunnel Config (mirrors cmd/mpquic/config.go, YAML-compatible) ────────

// TunnelConfig represents a tunnel YAML configuration file.
// Fields match the main mpquic binary's Config struct.
type TunnelConfig struct {
	Role                  string              `yaml:"role" json:"role"`
	BindIP                string              `yaml:"bind_ip,omitempty" json:"bind_ip,omitempty"`
	RemoteAddr            string              `yaml:"remote_addr,omitempty" json:"remote_addr,omitempty"`
	RemotePort            int                 `yaml:"remote_port,omitempty" json:"remote_port,omitempty"`
	MultiConnEnabled      bool                `yaml:"multi_conn_enabled,omitempty" json:"multi_conn_enabled,omitempty"`
	MultipathEnabled      bool                `yaml:"multipath_enabled,omitempty" json:"multipath_enabled,omitempty"`
	MultipathPolicy       string              `yaml:"multipath_policy,omitempty" json:"multipath_policy,omitempty"`
	DataplaneConfigFile   string              `yaml:"dataplane_config_file,omitempty" json:"dataplane_config_file,omitempty"`
	MultipathPaths        []MultipathPathConf `yaml:"multipath_paths,omitempty" json:"multipath_paths,omitempty"`
	TunName               string              `yaml:"tun_name" json:"tun_name"`
	TunCIDR               string              `yaml:"tun_cidr" json:"tun_cidr"`
	TunMTU                int                 `yaml:"tun_mtu,omitempty" json:"tun_mtu,omitempty"`
	LogLevel              string              `yaml:"log_level,omitempty" json:"log_level,omitempty"`
	TLSCertFile           string              `yaml:"tls_cert_file,omitempty" json:"tls_cert_file,omitempty"`
	TLSKeyFile            string              `yaml:"tls_key_file,omitempty" json:"tls_key_file,omitempty"`
	TLSCAFile             string              `yaml:"tls_ca_file,omitempty" json:"tls_ca_file,omitempty"`
	TLSServerName         string              `yaml:"tls_server_name,omitempty" json:"tls_server_name,omitempty"`
	TLSInsecureSkipVerify bool                `yaml:"tls_insecure_skip_verify,omitempty" json:"tls_insecure_skip_verify,omitempty"`
	ControlAPIListen      string              `yaml:"control_api_listen,omitempty" json:"control_api_listen,omitempty"`
	ControlAPIAuthToken   string              `yaml:"control_api_auth_token,omitempty" json:"-"` // never exposed via API
	CongestionAlgorithm   string              `yaml:"congestion_algorithm,omitempty" json:"congestion_algorithm,omitempty"`
	TransportMode         string              `yaml:"transport_mode,omitempty" json:"transport_mode,omitempty"`
	DetectStarlink        bool                `yaml:"detect_starlink,omitempty" json:"detect_starlink,omitempty"`
	StarlinkDefaultPipes  int                 `yaml:"starlink_default_pipes,omitempty" json:"starlink_default_pipes,omitempty"`
	StarlinkTransport     string              `yaml:"starlink_transport,omitempty" json:"starlink_transport,omitempty"`
	StripePort            int                 `yaml:"stripe_port,omitempty" json:"stripe_port,omitempty"`
	StripeDataShards      int                 `yaml:"stripe_data_shards,omitempty" json:"stripe_data_shards,omitempty"`
	StripeParityShards    int                 `yaml:"stripe_parity_shards,omitempty" json:"stripe_parity_shards,omitempty"`
	StripeFECMode         string              `yaml:"stripe_fec_mode,omitempty" json:"stripe_fec_mode,omitempty"`
	StripePacingRate      int                 `yaml:"stripe_pacing_rate,omitempty" json:"stripe_pacing_rate,omitempty"`
	StripeARQ             *bool               `yaml:"stripe_arq,omitempty" json:"stripe_arq,omitempty"` // pointer to distinguish unset
	StripeDisableGSO      bool                `yaml:"stripe_disable_gso,omitempty" json:"stripe_disable_gso,omitempty"`
	StripeFECType         string              `yaml:"stripe_fec_type,omitempty" json:"stripe_fec_type,omitempty"`
	StripeFECWindow       int                 `yaml:"stripe_fec_window,omitempty" json:"stripe_fec_window,omitempty"`
	StripeFECInterleave   int                 `yaml:"stripe_fec_interleave,omitempty" json:"stripe_fec_interleave,omitempty"`
	StripeEnabled         bool                `yaml:"stripe_enabled,omitempty" json:"stripe_enabled,omitempty"`
	MetricsListen         string              `yaml:"metrics_listen,omitempty" json:"metrics_listen,omitempty"`
}

type MultipathPathConf struct {
	Name       string `yaml:"name" json:"name"`
	BindIP     string `yaml:"bind_ip" json:"bind_ip"`
	RemoteAddr string `yaml:"remote_addr" json:"remote_addr"`
	RemotePort int    `yaml:"remote_port" json:"remote_port"`
	Priority   int    `yaml:"priority,omitempty" json:"priority,omitempty"`
	Weight     int    `yaml:"weight,omitempty" json:"weight,omitempty"`
	Pipes      int    `yaml:"pipes,omitempty" json:"pipes,omitempty"`
	Transport  string `yaml:"transport,omitempty" json:"transport,omitempty"`
}

// ─── Parameter Classification ─────────────────────────────────────────────

type ParamCategory int

const (
	CatA_HotReload ParamCategory = iota // Runtime modifiable, no restart
	CatB_Restart                        // Client-only, requires tunnel restart
	CatC_Server                         // Server-coupled, NOT modifiable via API
)

// paramClassification maps YAML field names to their category.
var paramClassification = map[string]ParamCategory{
	// Category A — Hot-reload (runtime, no restart)
	"log_level":         CatA_HotReload,
	"stripe_pacing_rate": CatA_HotReload,
	"stripe_fec_mode":   CatA_HotReload,
	"multipath_policy":  CatA_HotReload,
	// dataplane is also Cat A but handled separately (complex object)

	// Category B — Client-only, requires restart
	"tun_mtu":               CatB_Restart,
	"congestion_algorithm":  CatB_Restart,
	"transport_mode":        CatB_Restart,
	"stripe_arq":            CatB_Restart,
	"stripe_fec_type":       CatB_Restart,
	"stripe_fec_window":     CatB_Restart,
	"stripe_fec_interleave": CatB_Restart,
	"stripe_disable_gso":    CatB_Restart,
	"detect_starlink":       CatB_Restart,
	"starlink_default_pipes": CatB_Restart,
	"starlink_transport":    CatB_Restart,
	"stripe_enabled":        CatB_Restart,

	// Category C — Server-coupled (blocked)
	"role":                    CatC_Server,
	"bind_ip":                 CatC_Server,
	"remote_addr":             CatC_Server,
	"remote_port":             CatC_Server,
	"tun_name":                CatC_Server,
	"tun_cidr":                CatC_Server,
	"stripe_port":             CatC_Server,
	"stripe_data_shards":      CatC_Server,
	"stripe_parity_shards":    CatC_Server,
	"tls_cert_file":           CatC_Server,
	"tls_key_file":            CatC_Server,
	"tls_ca_file":             CatC_Server,
	"tls_server_name":         CatC_Server,
	"tls_insecure_skip_verify": CatC_Server,
	"multi_conn_enabled":      CatC_Server,
	"multipath_enabled":       CatC_Server,
	"metrics_listen":          CatC_Server,
	"control_api_listen":      CatC_Server,
	"control_api_auth_token":  CatC_Server,
	"dataplane_config_file":   CatC_Server,
}

// Nested path fields that are server-coupled
var pathServerFields = map[string]bool{
	"bind_ip":     true,
	"remote_addr": true,
	"remote_port": true,
	"name":        true,
}

// ─── Instance Info ────────────────────────────────────────────────────────

// TunnelInstance represents a discovered tunnel instance with runtime state.
type TunnelInstance struct {
	Name       string       `json:"name"`
	ConfigFile string       `json:"config_file"`
	Config     TunnelConfig `json:"config"`
	Status     string       `json:"status"`      // "running", "stopped", "failed", "unknown"
	PID        int          `json:"pid"`          // 0 if not running
	Uptime     string       `json:"uptime"`       // human-readable
	UptimeSec  int64        `json:"uptime_sec"`   // seconds
	MetricsURL string       `json:"metrics_url"`  // e.g. "http://10.200.17.1:9090/api/v1/stats"
}

// TunnelSummary is a compact view for the list endpoint.
type TunnelSummary struct {
	Name      string `json:"name"`
	Role      string `json:"role"`
	TunName   string `json:"tun_name"`
	TunCIDR   string `json:"tun_cidr"`
	Status    string `json:"status"`
	PID       int    `json:"pid"`
	Uptime    string `json:"uptime"`
	UptimeSec int64  `json:"uptime_sec"`
	Transport string `json:"transport"` // "stripe", "quic", "multipath-stripe"
}

// ─── Instance Manager ─────────────────────────────────────────────────────

// InstanceManager handles discovery, lifecycle, and configuration of tunnel instances.
type InstanceManager struct {
	dir string
	mu  sync.RWMutex
}

func NewInstanceManager(dir string) *InstanceManager {
	return &InstanceManager{dir: dir}
}

// Discover scans the instance directory for client YAML configs.
// Skips server configs (mt4/mt5/mt6), dataplane configs, backups, and templates.
func (im *InstanceManager) Discover() ([]string, error) {
	entries, err := os.ReadDir(im.dir)
	if err != nil {
		return nil, fmt.Errorf("scan instance dir: %w", err)
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		// Only .yaml files (not .tpl, .bak, .env, .json)
		if !strings.HasSuffix(name, ".yaml") {
			continue
		}
		// Skip backup files
		if strings.Contains(name, ".bak") || strings.Contains(name, "_RS_") {
			continue
		}
		// Skip dataplane configs
		if strings.HasPrefix(name, "dataplane") {
			continue
		}
		// Skip multipath- prefixed configs (templates)
		if strings.HasPrefix(name, "multipath-") {
			continue
		}

		base := strings.TrimSuffix(name, ".yaml")

		// Quick-load to check role — skip server configs
		cfg, err := im.loadConfigFile(filepath.Join(im.dir, name))
		if err != nil {
			continue // skip unparseable configs
		}
		if cfg.Role == "server" {
			continue
		}

		names = append(names, base)
	}
	sort.Strings(names)
	return names, nil
}

// ListInstances returns summary info for all discovered client tunnel instances.
func (im *InstanceManager) ListInstances() ([]TunnelSummary, error) {
	names, err := im.Discover()
	if err != nil {
		return nil, err
	}

	summaries := make([]TunnelSummary, 0, len(names))
	for _, name := range names {
		cfg, err := im.loadConfigFile(im.configPath(name))
		if err != nil {
			continue
		}
		status, pid, uptimeSec := im.querySystemdStatus(name)
		transport := detectTransport(cfg)

		summaries = append(summaries, TunnelSummary{
			Name:      name,
			Role:      cfg.Role,
			TunName:   cfg.TunName,
			TunCIDR:   cfg.TunCIDR,
			Status:    status,
			PID:       pid,
			Uptime:    formatDuration(uptimeSec),
			UptimeSec: uptimeSec,
			Transport: transport,
		})
	}
	return summaries, nil
}

// GetInstance returns full details for a single tunnel instance.
func (im *InstanceManager) GetInstance(name string) (*TunnelInstance, error) {
	cfgPath := im.configPath(name)
	cfg, err := im.loadConfigFile(cfgPath)
	if err != nil {
		return nil, fmt.Errorf("instance %q: %w", name, err)
	}

	status, pid, uptimeSec := im.querySystemdStatus(name)
	metricsURL := resolveMetricsURL(cfg)

	return &TunnelInstance{
		Name:       name,
		ConfigFile: cfgPath,
		Config:     *cfg,
		Status:     status,
		PID:        pid,
		Uptime:     formatDuration(uptimeSec),
		UptimeSec:  uptimeSec,
		MetricsURL: metricsURL,
	}, nil
}

// ─── Lifecycle ────────────────────────────────────────────────────────────

func (im *InstanceManager) StartTunnel(name string) error {
	if !im.instanceExists(name) {
		return fmt.Errorf("instance %q not found", name)
	}
	return im.systemctl("start", name)
}

func (im *InstanceManager) StopTunnel(name string) error {
	if !im.instanceExists(name) {
		return fmt.Errorf("instance %q not found", name)
	}
	return im.systemctl("stop", name)
}

func (im *InstanceManager) RestartTunnel(name string) error {
	if !im.instanceExists(name) {
		return fmt.Errorf("instance %q not found", name)
	}
	return im.systemctl("restart", name)
}

// ─── Configuration CRUD ───────────────────────────────────────────────────

// GetConfig returns the parsed YAML config for a tunnel.
func (im *InstanceManager) GetConfig(name string) (*TunnelConfig, error) {
	return im.loadConfigFile(im.configPath(name))
}

// GetConfigRaw returns the raw YAML bytes.
func (im *InstanceManager) GetConfigRaw(name string) ([]byte, error) {
	return os.ReadFile(im.configPath(name))
}

// ValidateConfigPatch checks a partial config update for validity.
// Returns (needsRestart bool, blockedFields []string, err error).
func (im *InstanceManager) ValidateConfigPatch(name string, patch map[string]interface{}) (bool, []string, error) {
	if !im.instanceExists(name) {
		return false, nil, fmt.Errorf("instance %q not found", name)
	}

	var blocked []string
	needsRestart := false

	for key := range patch {
		cat, known := paramClassification[key]
		if !known {
			// Check if it's a nested multipath_paths field
			if key == "multipath_paths" {
				needsRestart = true
				// Validate nested path fields
				if paths, ok := patch[key].([]interface{}); ok {
					for _, p := range paths {
						if pm, ok := p.(map[string]interface{}); ok {
							for pk := range pm {
								if pathServerFields[pk] {
									blocked = append(blocked, fmt.Sprintf("multipath_paths[].%s", pk))
								}
							}
						}
					}
				}
				continue
			}
			blocked = append(blocked, key)
			continue
		}
		switch cat {
		case CatC_Server:
			blocked = append(blocked, key)
		case CatB_Restart:
			needsRestart = true
		case CatA_HotReload:
			// no restart needed
		}
	}

	if len(blocked) > 0 {
		return needsRestart, blocked, fmt.Errorf("server-coupled parameters cannot be modified: %v", blocked)
	}
	return needsRestart, nil, nil
}

// ApplyConfigPatch applies a partial config update to a tunnel's YAML file.
// Returns whether a restart is needed.
func (im *InstanceManager) ApplyConfigPatch(name string, patch map[string]interface{}) (bool, error) {
	im.mu.Lock()
	defer im.mu.Unlock()

	cfgPath := im.configPath(name)

	// Validate first
	needsRestart, blocked, err := im.ValidateConfigPatch(name, patch)
	if err != nil {
		return false, err
	}
	if len(blocked) > 0 {
		return false, fmt.Errorf("blocked fields: %v", blocked)
	}

	// Read current YAML as ordered map to preserve formatting
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		return false, fmt.Errorf("read config: %w", err)
	}

	// Backup before modification
	backupPath := cfgPath + fmt.Sprintf(".bak.%d", time.Now().Unix())
	if err := os.WriteFile(backupPath, raw, 0644); err != nil {
		return false, fmt.Errorf("backup config: %w", err)
	}

	// Parse into generic map for safe merging
	var current map[string]interface{}
	if err := yaml.Unmarshal(raw, &current); err != nil {
		return false, fmt.Errorf("parse current config: %w", err)
	}
	if current == nil {
		current = make(map[string]interface{})
	}

	// Apply patch
	for k, v := range patch {
		current[k] = v
	}

	// Marshal back
	out, err := yaml.Marshal(current)
	if err != nil {
		return false, fmt.Errorf("marshal config: %w", err)
	}

	if err := os.WriteFile(cfgPath, out, 0644); err != nil {
		return false, fmt.Errorf("write config: %w", err)
	}

	// Validate the result by trying to parse it
	if _, err := im.loadConfigFile(cfgPath); err != nil {
		// Rollback
		_ = os.WriteFile(cfgPath, raw, 0644)
		return false, fmt.Errorf("validation failed (rolled back): %w", err)
	}

	return needsRestart, nil
}

// ─── Metrics Proxy ────────────────────────────────────────────────────────

// FetchMetrics retrieves metrics from a running tunnel's HTTP endpoint.
func (im *InstanceManager) FetchMetrics(name string) (json.RawMessage, error) {
	inst, err := im.GetInstance(name)
	if err != nil {
		return nil, err
	}
	if inst.Status != "running" {
		return nil, fmt.Errorf("tunnel %q not running", name)
	}
	if inst.MetricsURL == "" {
		return nil, fmt.Errorf("tunnel %q has no metrics endpoint", name)
	}

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(inst.MetricsURL)
	if err != nil {
		return nil, fmt.Errorf("metrics fetch: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("metrics endpoint returned %d", resp.StatusCode)
	}

	var data json.RawMessage
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return nil, fmt.Errorf("metrics decode: %w", err)
	}
	return data, nil
}

// FetchAllMetrics retrieves metrics from all running tunnels.
func (im *InstanceManager) FetchAllMetrics() (map[string]json.RawMessage, error) {
	names, err := im.Discover()
	if err != nil {
		return nil, err
	}
	result := make(map[string]json.RawMessage, len(names))
	for _, name := range names {
		data, err := im.FetchMetrics(name)
		if err != nil {
			result[name] = json.RawMessage(fmt.Sprintf(`{"error":%q}`, err.Error()))
			continue
		}
		result[name] = data
	}
	return result, nil
}

// ─── Logs ──────────────────────────────────────────────────────────────────

// FetchLogs retrieves recent journal entries for a tunnel instance.
func (im *InstanceManager) FetchLogs(name string, lines int, level string) (string, error) {
	if !im.instanceExists(name) {
		return "", fmt.Errorf("instance %q not found", name)
	}
	unit := fmt.Sprintf("mpquic@%s", name)
	args := []string{"-u", unit, "--no-pager", "-n", fmt.Sprintf("%d", lines)}
	if level == "error" {
		args = append(args, "--grep=ERROR")
	}
	out, err := exec.Command("journalctl", args...).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("journalctl: %w: %s", err, string(out))
	}
	return string(out), nil
}

// ─── Internal helpers ─────────────────────────────────────────────────────

func (im *InstanceManager) configPath(name string) string {
	return filepath.Join(im.dir, name+".yaml")
}

func (im *InstanceManager) instanceExists(name string) bool {
	_, err := os.Stat(im.configPath(name))
	return err == nil
}

func (im *InstanceManager) loadConfigFile(path string) (*TunnelConfig, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	cfg := &TunnelConfig{}
	if err := yaml.Unmarshal(b, cfg); err != nil {
		return nil, fmt.Errorf("parse YAML: %w", err)
	}
	return cfg, nil
}

func (im *InstanceManager) querySystemdStatus(name string) (status string, pid int, uptimeSec int64) {
	unit := fmt.Sprintf("mpquic@%s.service", name)

	// Get ActiveState
	out, err := exec.Command("systemctl", "show", unit,
		"--property=ActiveState,MainPID,ActiveEnterTimestamp").CombinedOutput()
	if err != nil {
		return "unknown", 0, 0
	}

	props := parseSystemdProperties(string(out))

	switch props["ActiveState"] {
	case "active":
		status = "running"
	case "failed":
		status = "failed"
	case "inactive":
		status = "stopped"
	case "activating":
		status = "starting"
	default:
		status = props["ActiveState"]
		if status == "" {
			status = "unknown"
		}
	}

	fmt.Sscanf(props["MainPID"], "%d", &pid)

	if ts := props["ActiveEnterTimestamp"]; ts != "" && status == "running" {
		// Parse systemd timestamp: "Mon 2026-03-25 10:30:00 UTC"
		for _, layout := range []string{
			"Mon 2006-01-02 15:04:05 MST",
			"Mon 2006-01-02 15:04:05 UTC",
			time.RFC3339,
		} {
			if t, err := time.Parse(layout, ts); err == nil {
				uptimeSec = int64(time.Since(t).Seconds())
				break
			}
		}
	}

	return status, pid, uptimeSec
}

func (im *InstanceManager) systemctl(action, name string) error {
	unit := fmt.Sprintf("mpquic@%s", name)
	out, err := exec.Command("systemctl", action, unit).CombinedOutput()
	if err != nil {
		return fmt.Errorf("systemctl %s %s: %w: %s", action, unit, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// ─── Utility functions ────────────────────────────────────────────────────

func parseSystemdProperties(output string) map[string]string {
	props := make(map[string]string)
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if idx := strings.IndexByte(line, '='); idx > 0 {
			props[line[:idx]] = line[idx+1:]
		}
	}
	return props
}

func detectTransport(cfg *TunnelConfig) string {
	if cfg.MultipathEnabled {
		allStripe := true
		for _, p := range cfg.MultipathPaths {
			if p.Transport != "stripe" {
				allStripe = false
				break
			}
		}
		if allStripe && len(cfg.MultipathPaths) > 0 {
			return "multipath-stripe"
		}
		return "multipath-quic"
	}
	return "quic"
}

func resolveMetricsURL(cfg *TunnelConfig) string {
	listen := cfg.MetricsListen
	if listen == "" {
		return ""
	}
	if strings.EqualFold(listen, "auto") {
		p, err := netip.ParsePrefix(cfg.TunCIDR)
		if err != nil {
			return ""
		}
		listen = net.JoinHostPort(p.Addr().String(), "9090")
	}
	return "http://" + listen + "/api/v1/stats"
}

func formatDuration(sec int64) string {
	if sec <= 0 {
		return "-"
	}
	d := time.Duration(sec) * time.Second
	days := int(d.Hours()) / 24
	hours := int(d.Hours()) % 24
	minutes := int(d.Minutes()) % 60
	if days > 0 {
		return fmt.Sprintf("%dd %dh %dm", days, hours, minutes)
	}
	if hours > 0 {
		return fmt.Sprintf("%dh %dm", hours, minutes)
	}
	return fmt.Sprintf("%dm", minutes)
}
