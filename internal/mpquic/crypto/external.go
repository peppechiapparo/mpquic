package crypto

// ExternalCryptoAdapter è l'interfaccia che un plugin Go di terze parti
// deve implementare ed esportare come simbolo "CryptoProvider".
type ExternalCryptoAdapter interface {
	Init(configPath string) error
	Name() string
	Version() string
	// KeyExchangeProvider restituisce il KEX provider del plugin.
	// Può essere nil se il plugin fornisce solo AEAD (Livello A).
	KeyExchangeProvider() KeyExchangeProvider
	// AEADProvider restituisce l'AEAD provider del plugin.
	AEADProvider() AEADProvider
	Close() error
}
