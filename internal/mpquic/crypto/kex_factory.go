package crypto

import "fmt"

func NewKeyExchangeProvider(profile CryptoProfile) (KeyExchangeProvider, error) {
	switch profile {
	case ProfilePerformance:
		return NewClassicalKEXProvider(), nil
	case ProfileHybridSecurity:
		return NewHybridKEXProvider(), nil
	case ProfileCustomProvider:
		return nil, fmt.Errorf("%w: custom provider loading not yet implemented", ErrProviderNotFound)
	default:
		return nil, fmt.Errorf("%w: %q", ErrInvalidProfile, profile)
	}
}
