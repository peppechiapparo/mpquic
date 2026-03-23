package main

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Role                  string                `yaml:"role"`
	BindIP                string                `yaml:"bind_ip"`
	RemoteAddr            string                `yaml:"remote_addr"`
	RemotePort            int                   `yaml:"remote_port"`
	MultiConnEnabled      bool                  `yaml:"multi_conn_enabled"`
	MultipathEnabled      bool                  `yaml:"multipath_enabled"`
	MultipathPolicy       string                `yaml:"multipath_policy"`
	DataplaneConfigFile   string                `yaml:"dataplane_config_file"`
	Dataplane             DataplaneConfig       `yaml:"dataplane"`
	MultipathPaths        []MultipathPathConfig `yaml:"multipath_paths"`
	TunName               string                `yaml:"tun_name"`
	TunCIDR               string                `yaml:"tun_cidr"`
	TunMTU                int                   `yaml:"tun_mtu"`
	LogLevel              string                `yaml:"log_level"`
	TLSCertFile           string                `yaml:"tls_cert_file"`
	TLSKeyFile            string                `yaml:"tls_key_file"`
	TLSCAFile             string                `yaml:"tls_ca_file"`
	TLSServerName         string                `yaml:"tls_server_name"`
	TLSInsecureSkipVerify bool                  `yaml:"tls_insecure_skip_verify"`
	ControlAPIListen      string                `yaml:"control_api_listen"`
	ControlAPIAuthToken   string                `yaml:"control_api_auth_token"`
	CongestionAlgorithm   string                `yaml:"congestion_algorithm"`
	TransportMode         string                `yaml:"transport_mode"`
	DetectStarlink        bool                  `yaml:"detect_starlink"`
	StarlinkDefaultPipes  int                   `yaml:"starlink_default_pipes"`
	StarlinkTransport     string                `yaml:"starlink_transport"`
	StripePort            int                   `yaml:"stripe_port"`
	StripeDataShards      int                   `yaml:"stripe_data_shards"`
	StripeParityShards    int                   `yaml:"stripe_parity_shards"`
	StripeFECMode         string                `yaml:"stripe_fec_mode"` // "always" (default), "adaptive", "off"
	StripePacingRate      int                   `yaml:"stripe_pacing_rate"` // Mbps per session (0 = disabled)
	StripeARQ             bool                  `yaml:"stripe_arq"`         // Hybrid ARQ with NACK retransmission
	StripeDisableGSO      bool                  `yaml:"stripe_disable_gso"` // Disable UDP GSO (for A/B testing)
	StripeFECType         string                `yaml:"stripe_fec_type"`    // "rs" (default), "xor" (legacy), "rlc"
	StripeFECWindow       int                   `yaml:"stripe_fec_window"`  // Sliding-window size W (default 10, used by xor/rlc)
	StripeFECInterleave   int                   `yaml:"stripe_fec_interleave"` // RS interleave depth (0=block RS, >0=interleaved, default 4)
	StripeEnabled         bool                  `yaml:"stripe_enabled"`
	MetricsListen         string                `yaml:"metrics_listen"` // e.g. "10.200.17.254:9090" — bind to tunnel IP only
}

type MultipathPathConfig struct {
	Name       string `yaml:"name"`
	BindIP     string `yaml:"bind_ip"`
	RemoteAddr string `yaml:"remote_addr"`
	RemotePort int    `yaml:"remote_port"`
	Priority   int    `yaml:"priority"`
	Weight     int    `yaml:"weight"`
	Pipes      int    `yaml:"pipes"`
	BasePath   string `yaml:"-"`        // original path name before pipe expansion
	Transport  string `yaml:"transport"` // "quic" (default), "stripe", or "auto"
}

type DataplaneConfig struct {
	DefaultClass string                          `yaml:"default_class"`
	Classes      map[string]DataplaneClassPolicy `yaml:"classes"`
	Classifiers  []DataplaneClassifierRule       `yaml:"classifiers"`
}

type DataplaneClassPolicy struct {
	SchedulerPolicy string   `yaml:"scheduler_policy"`
	PreferredPaths  []string `yaml:"preferred_paths"`
	ExcludedPaths   []string `yaml:"excluded_paths"`
	Duplicate       bool     `yaml:"duplicate"`
	DuplicateCopies int      `yaml:"duplicate_copies"`
}

type DataplaneClassifierRule struct {
	Name      string   `yaml:"name"`
	ClassName string   `yaml:"class"`
	Protocol  string   `yaml:"protocol"`
	SrcCIDRs  []string `yaml:"src_cidrs"`
	DstCIDRs  []string `yaml:"dst_cidrs"`
	SrcPorts  []string `yaml:"src_ports"`
	DstPorts  []string `yaml:"dst_ports"`
	DSCP      []int    `yaml:"dscp"`
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
	cfg.ControlAPIListen = strings.TrimSpace(cfg.ControlAPIListen)
	cfg.ControlAPIAuthToken = strings.TrimSpace(cfg.ControlAPIAuthToken)
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
		cfg.MultipathPolicy = strings.ToLower(strings.TrimSpace(cfg.MultipathPolicy))
		if cfg.MultipathPolicy == "" {
			cfg.MultipathPolicy = "priority"
		}
		if cfg.MultipathPolicy != "priority" && cfg.MultipathPolicy != "failover" && cfg.MultipathPolicy != "balanced" {
			return nil, fmt.Errorf("multipath_policy must be one of: priority, failover, balanced")
		}
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
		if err := loadAndValidateDataplaneConfig(path, cfg); err != nil {
			return nil, err
		}
	}
	if cfg.TunName == "" {
		return nil, fmt.Errorf("tun_name required")
	}
	if cfg.TunCIDR == "" {
		return nil, fmt.Errorf("tun_cidr required")
	}
	if cfg.TunMTU <= 0 {
		cfg.TunMTU = 1300
	}

	// Resolve metrics_listen: "auto" → derive from tun_cidr IP + port 9090
	cfg.MetricsListen = strings.TrimSpace(cfg.MetricsListen)
	if strings.EqualFold(cfg.MetricsListen, "auto") {
		tunPrefix, err := netip.ParsePrefix(cfg.TunCIDR)
		if err != nil {
			return nil, fmt.Errorf("cannot parse tun_cidr for metrics auto-bind: %w", err)
		}
		cfg.MetricsListen = net.JoinHostPort(tunPrefix.Addr().String(), "9090")
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

func loadAndValidateDataplaneConfig(configPath string, cfg *Config) error {
	dp := cfg.Dataplane

	if cfg.DataplaneConfigFile != "" {
		dpPath := cfg.DataplaneConfigFile
		if !filepath.IsAbs(dpPath) {
			dpPath = filepath.Join(filepath.Dir(configPath), dpPath)
		}
		cfg.DataplaneConfigFile = dpPath
		b, err := os.ReadFile(dpPath)
		if err != nil {
			return fmt.Errorf("dataplane_config_file read failed: %w", err)
		}
		fileDP := DataplaneConfig{}
		if err := yaml.Unmarshal(b, &fileDP); err != nil {
			return fmt.Errorf("dataplane_config_file parse failed: %w", err)
		}
		dp = mergeDataplaneConfig(dp, fileDP)
	}

	normalizeDataplaneConfig(&dp, cfg.MultipathPolicy)
	if err := validateDataplaneConfig(dp, cfg.MultipathPaths); err != nil {
		return err
	}
	cfg.Dataplane = dp
	return nil
}

func mergeDataplaneConfig(base DataplaneConfig, override DataplaneConfig) DataplaneConfig {
	out := base
	if strings.TrimSpace(override.DefaultClass) != "" {
		out.DefaultClass = override.DefaultClass
	}
	if len(override.Classes) > 0 {
		if out.Classes == nil {
			out.Classes = make(map[string]DataplaneClassPolicy, len(override.Classes))
		}
		for className, policy := range override.Classes {
			out.Classes[className] = policy
		}
	}
	if len(override.Classifiers) > 0 {
		out.Classifiers = override.Classifiers
	}
	return out
}

func normalizeDataplaneConfig(dp *DataplaneConfig, fallbackPolicy string) {
	if fallbackPolicy == "" {
		fallbackPolicy = "priority"
	}
	dp.DefaultClass = strings.ToLower(strings.TrimSpace(dp.DefaultClass))
	if dp.DefaultClass == "" {
		dp.DefaultClass = "default"
	}

	if dp.Classes == nil {
		dp.Classes = map[string]DataplaneClassPolicy{}
	}
	if len(dp.Classes) == 0 {
		dp.Classes[dp.DefaultClass] = DataplaneClassPolicy{SchedulerPolicy: fallbackPolicy}
	}

	normalizedClasses := make(map[string]DataplaneClassPolicy, len(dp.Classes))
	for className, policy := range dp.Classes {
		n := strings.ToLower(strings.TrimSpace(className))
		if n == "" {
			continue
		}
		policy.SchedulerPolicy = strings.ToLower(strings.TrimSpace(policy.SchedulerPolicy))
		if policy.SchedulerPolicy == "" {
			policy.SchedulerPolicy = fallbackPolicy
		}
		if policy.Duplicate && policy.DuplicateCopies < 2 {
			policy.DuplicateCopies = 2
		}
		if policy.DuplicateCopies > 3 {
			policy.DuplicateCopies = 3
		}
		normalizedClasses[n] = policy
	}
	dp.Classes = normalizedClasses

	for i := range dp.Classifiers {
		r := &dp.Classifiers[i]
		r.Name = strings.TrimSpace(r.Name)
		r.ClassName = strings.ToLower(strings.TrimSpace(r.ClassName))
		r.Protocol = strings.ToLower(strings.TrimSpace(r.Protocol))
	}
}

func validateDataplaneConfig(dp DataplaneConfig, paths []MultipathPathConfig) error {
	if len(dp.Classes) == 0 {
		return fmt.Errorf("dataplane.classes must not be empty")
	}
	if _, ok := dp.Classes[dp.DefaultClass]; !ok {
		return fmt.Errorf("dataplane.default_class=%q not found in dataplane.classes", dp.DefaultClass)
	}

	pathSet := make(map[string]struct{}, len(paths))
	for _, p := range paths {
		pathSet[p.Name] = struct{}{}
		// Also accept base path names (before pipe expansion)
		if p.BasePath != "" {
			pathSet[p.BasePath] = struct{}{}
		}
	}

	for className, policy := range dp.Classes {
		if policy.SchedulerPolicy != "priority" && policy.SchedulerPolicy != "failover" && policy.SchedulerPolicy != "balanced" {
			return fmt.Errorf("dataplane.classes[%s].scheduler_policy must be one of: priority, failover, balanced", className)
		}
		for _, name := range policy.PreferredPaths {
			if _, ok := pathSet[name]; !ok {
				return fmt.Errorf("dataplane.classes[%s].preferred_paths references unknown path: %s", className, name)
			}
		}
		for _, name := range policy.ExcludedPaths {
			if _, ok := pathSet[name]; !ok {
				return fmt.Errorf("dataplane.classes[%s].excluded_paths references unknown path: %s", className, name)
			}
		}
	}

	for i, rule := range dp.Classifiers {
		if rule.ClassName == "" {
			return fmt.Errorf("dataplane.classifiers[%d].class required", i)
		}
		if _, ok := dp.Classes[rule.ClassName]; !ok {
			return fmt.Errorf("dataplane.classifiers[%d].class references unknown class: %s", i, rule.ClassName)
		}
		if rule.Protocol != "" && rule.Protocol != "udp" && rule.Protocol != "tcp" && rule.Protocol != "icmp" && rule.Protocol != "icmpv6" {
			return fmt.Errorf("dataplane.classifiers[%d].protocol invalid: %s", i, rule.Protocol)
		}
		if _, err := parseCIDRs(rule.SrcCIDRs); err != nil {
			return fmt.Errorf("dataplane.classifiers[%d].src_cidrs invalid: %w", i, err)
		}
		if _, err := parseCIDRs(rule.DstCIDRs); err != nil {
			return fmt.Errorf("dataplane.classifiers[%d].dst_cidrs invalid: %w", i, err)
		}
		if _, err := parsePortRanges(rule.SrcPorts); err != nil {
			return fmt.Errorf("dataplane.classifiers[%d].src_ports invalid: %w", i, err)
		}
		if _, err := parsePortRanges(rule.DstPorts); err != nil {
			return fmt.Errorf("dataplane.classifiers[%d].dst_ports invalid: %w", i, err)
		}
		for _, dscp := range rule.DSCP {
			if dscp < 0 || dscp > 63 {
				return fmt.Errorf("dataplane.classifiers[%d].dscp value out of range: %d", i, dscp)
			}
		}
	}

	return nil
}

func cloneDataplaneConfig(in DataplaneConfig) DataplaneConfig {
	out := DataplaneConfig{
		DefaultClass: in.DefaultClass,
	}
	if in.Classes != nil {
		out.Classes = make(map[string]DataplaneClassPolicy, len(in.Classes))
		for name, policy := range in.Classes {
			copyPolicy := policy
			copyPolicy.PreferredPaths = append([]string(nil), policy.PreferredPaths...)
			copyPolicy.ExcludedPaths = append([]string(nil), policy.ExcludedPaths...)
			out.Classes[name] = copyPolicy
		}
	}
	if len(in.Classifiers) > 0 {
		out.Classifiers = make([]DataplaneClassifierRule, 0, len(in.Classifiers))
		for _, rule := range in.Classifiers {
			copyRule := rule
			copyRule.SrcCIDRs = append([]string(nil), rule.SrcCIDRs...)
			copyRule.DstCIDRs = append([]string(nil), rule.DstCIDRs...)
			copyRule.SrcPorts = append([]string(nil), rule.SrcPorts...)
			copyRule.DstPorts = append([]string(nil), rule.DstPorts...)
			copyRule.DSCP = append([]int(nil), rule.DSCP...)
			out.Classifiers = append(out.Classifiers, copyRule)
		}
	}
	return out
}

func loadServerTLSConfig(cfg *Config) (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(cfg.TLSCertFile, cfg.TLSKeyFile)
	if err != nil {
		return nil, err
	}
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		NextProtos:   []string{"mpquic-ip", stripeKXALPN},
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
