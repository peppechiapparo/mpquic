package crypto

import "fmt"

func NewKeyExchangeProvider(profile CryptoProfile) (KeyExchangeProvider, error) {
	switch profile {
	case ProfilePerformance:
		return NewClassicalKEXProvider(), nil
	case ProfileHybridSecurity:
		return NewHybridKEXProvider(), nil
	case ProfileCustomProvider:
		// Il provider custom viene caricato in NewCryptoSession tramite LoadExternalProvider.
		// Questa factory gestisce solo i provider built-in (performance, hybrid_security).
		return nil, fmt.Errorf("%w: use LoadExternalProvider + NewCryptoSession for custom_provider profile", ErrInvalidProfile)
	default:
		return nil, fmt.Errorf("%w: %q", ErrInvalidProfile, profile)
	}
}
