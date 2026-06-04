package main

import (
	"mpquic/internal/mpquic/crypto"
)

type fakeProvider struct{}

func (f *fakeProvider) Init(configFile string) error { return nil }
func (f *fakeProvider) Name() string                 { return "FakeCryptoProvider" }
func (f *fakeProvider) Version() string              { return "0.1.0-test" }
func (f *fakeProvider) Close() error                 { return nil }

func (f *fakeProvider) KeyExchangeProvider() crypto.KeyExchangeProvider {
	return crypto.NewClassicalKEXProvider()
}

func (f *fakeProvider) AEADProvider() crypto.AEADProvider {
	return crypto.NewAESGCMProvider()
}

// CryptoProvider è il simbolo esportato richiesto da LoadExternalProvider.
// DEVE essere di tipo *ExternalCryptoAdapter (puntatore all'interfaccia).
var CryptoProvider crypto.ExternalCryptoAdapter = &fakeProvider{}
