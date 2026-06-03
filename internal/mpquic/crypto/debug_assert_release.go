//go:build !mpquicdebug

package crypto

// debugPanicf è una no-op in produzione.
// In sviluppo (build tag mpquicdebug) provoca panic per segnalare violazioni
// di precondizioni — senza overhead in release.
func debugPanicf(format string, args ...any) {}
