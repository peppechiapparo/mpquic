package crypto

import "fmt"

// CryptoConfig mappa la sezione `crypto:` del file YAML STRIPES.
type CryptoConfig struct {
	Enabled        bool                  `yaml:"enabled"`
	Profile        CryptoProfile         `yaml:"profile"`
	Rekey          RekeyConfig           `yaml:"rekey"`
	CustomProvider *CustomProviderConfig `yaml:"custom_provider,omitempty"`
	AADVersion     uint8                 `yaml:"aad_version"` // 1 = legacy (default), 2 = extended
}

type RekeyConfig struct {
	Enabled             bool   `yaml:"enabled"`
	IntervalSeconds     int    `yaml:"interval_seconds"`
	MaxPackets          uint64 `yaml:"max_packets"`
	MaxBytes            uint64 `yaml:"max_bytes"`
	OnPathRecovery      bool   `yaml:"on_path_recovery"`
	OnEpochChange       bool   `yaml:"on_epoch_change"`
	AntiFlappingSeconds int    `yaml:"anti_flapping_seconds"`
}

type CustomProviderConfig struct {
	Path       string `yaml:"path"`
	ConfigFile string `yaml:"config_file"`
}

func (c *CryptoConfig) Validate() error {
	if c == nil {
		return ErrInvalidConfig
	}
	if c.Profile == "" {
		return fmt.Errorf("%w: profile is empty", ErrInvalidProfile)
	}
	switch c.Profile {
	case ProfilePerformance, ProfileHybridSecurity, ProfileCustomProvider:
		// ok
	default:
		return fmt.Errorf("%w: %q", ErrInvalidProfile, c.Profile)
	}

	if c.Profile == ProfileCustomProvider {
		if c.CustomProvider == nil || c.CustomProvider.Path == "" {
			return fmt.Errorf("%w: custom_provider.path required", ErrInvalidConfig)
		}
	}

	if c.CustomProvider != nil && c.Profile != ProfileCustomProvider {
		return fmt.Errorf("%w: custom_provider section present but profile is %q (expected %q)",
			ErrInvalidConfig, c.Profile, ProfileCustomProvider)
	}

	if c.Rekey.AntiFlappingSeconds < 0 {
		return fmt.Errorf("%w: rekey.anti_flapping_seconds must be >= 0", ErrInvalidConfig)
	}

	if c.Rekey.Enabled && c.Rekey.MaxPackets == 0 && c.Rekey.MaxBytes == 0 && c.Rekey.IntervalSeconds == 0 {
		return fmt.Errorf("%w: rekey enabled but all thresholds are zero", ErrInvalidConfig)
	}

	if c.AADVersion == 0 {
		c.AADVersion = 1 // default silenzioso per retrocompatibilità
	}
	if c.AADVersion != 1 && c.AADVersion != 2 {
		return fmt.Errorf("%w: aad_version must be 1 or 2, got %d", ErrInvalidConfig, c.AADVersion)
	}

	return nil
}

func DefaultCryptoConfig() *CryptoConfig {
	return &CryptoConfig{
		Enabled:    true,
		Profile:    ProfilePerformance,
		AADVersion: 1,
		Rekey: RekeyConfig{
			Enabled:             true,
			IntervalSeconds:     3600,
			MaxPackets:          1_000_000_000,
			MaxBytes:            1_073_741_824,
			OnEpochChange:       true,
			AntiFlappingSeconds: 10,
		},
	}
}
