//go:build linux

package crypto

import (
	"fmt"
	"path/filepath"
	"plugin"
	"time"
)

// LoadExternalProvider carica un plugin Go (.so) dal percorso specificato,
// ne estrae il simbolo "CryptoProvider" (deve implementare ExternalCryptoAdapter),
// e chiama Init(configFile). Il provider caricato non può essere scaricato
// dalla memoria — il lifecycle è gestito tramite Close().
//
// Sicurezza: il path viene normalizzato e deve essere assoluto.
func LoadExternalProvider(soPath, configFile string) (ExternalCryptoAdapter, error) {
	// Normalizza e valida il path
	cleaned := filepath.Clean(soPath)
	if !filepath.IsAbs(cleaned) {
		return nil, fmt.Errorf("%w: path must be absolute, got %q", ErrProviderInvalidPath, soPath)
	}
	// filepath.Clean su path assoluto elimina ogni componente "..";
	// la protezione contro traversal è garantita da filepath.IsAbs sopra.
	if filepath.Ext(cleaned) != ".so" {
		return nil, fmt.Errorf("%w: plugin must be a .so file, got %q", ErrProviderInvalidPath, soPath)
	}

	// Apre il plugin
	plug, err := plugin.Open(cleaned)
	if err != nil {
		return nil, fmt.Errorf("%w: plugin.Open(%q): %v", ErrProviderNotFound, cleaned, err)
	}

	// Cerca il simbolo esportato "CryptoProvider"
	sym, err := plug.Lookup("CryptoProvider")
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrProviderSymbolMissing, err)
	}

	// Type assertion: il simbolo deve essere *ExternalCryptoAdapter
	provider, ok := sym.(*ExternalCryptoAdapter)
	if !ok {
		return nil, fmt.Errorf("%w: symbol CryptoProvider has unexpected type %T", ErrProviderSymbolMissing, sym)
	}

	// Chiama Init con il path della config del fornitore.
	// DT-E2: timeout 10s per evitare hang su plugin difettosi o malevoli.
	// Nota: la goroutine bloccata non è killabile (limite Go), ma il caller
	// riceve un errore deterministico e può terminare il processo.
	// DT-E3: recover da panic nel plugin per evitare crash del processo principale.
	type initResult struct {
		err error
	}
	initCh := make(chan initResult, 1)
	go func() {
		var res initResult
		defer func() {
			if r := recover(); r != nil {
				res.err = fmt.Errorf("%w: Init() panic: %v", ErrProviderInitFailed, r)
			}
			initCh <- res
		}()
		res.err = (*provider).Init(configFile)
	}()

	select {
	case res := <-initCh:
		if res.err != nil {
			return nil, fmt.Errorf("%w: %v", ErrProviderInitFailed, res.err)
		}
	case <-time.After(10 * time.Second):
		return nil, fmt.Errorf("%w: Init() timeout after 10s", ErrProviderInitFailed)
	}

	return *provider, nil
}
