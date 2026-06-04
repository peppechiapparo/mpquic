package crypto

import "errors"

var (
	ErrAuthFailed            = errors.New("crypto: packet authentication failed")
	ErrNonceExhausted        = errors.New("crypto: nonce counter exhausted")
	ErrInvalidProfile        = errors.New("crypto: invalid or unsupported crypto profile")
	ErrProviderNotFound      = errors.New("crypto: external provider not found or not loadable")
	ErrProviderInvalidPath   = errors.New("crypto: invalid or unsafe plugin path")
	ErrProviderSymbolMissing = errors.New("crypto: plugin does not export CryptoProvider symbol")
	ErrProviderInitFailed    = errors.New("crypto: external provider Init() failed")
	ErrInvalidConfig         = errors.New("crypto: invalid crypto configuration")
	ErrSessionClosed         = errors.New("crypto: crypto session is closed")
	ErrKeyMaterial           = errors.New("crypto: invalid or insufficient key material")
	ErrInvalidKeySize        = errors.New("crypto: invalid key size")
	ErrEmptySessionID        = errors.New("crypto: sessionID must not be empty")
	ErrMissingProvider       = errors.New("crypto: required provider is nil")
)

func IsAuthFailure(err error) bool {
	return errors.Is(err, ErrAuthFailed)
}
