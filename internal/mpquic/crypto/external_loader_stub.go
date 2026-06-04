//go:build !linux

package crypto

import "fmt"

// LoadExternalProvider è disponibile solo su Linux.
// Su altre piattaforme restituisce sempre ErrProviderNotFound.
func LoadExternalProvider(soPath, configFile string) (ExternalCryptoAdapter, error) {
	return nil, fmt.Errorf("%w: plugin loading is only supported on Linux", ErrProviderNotFound)
}
