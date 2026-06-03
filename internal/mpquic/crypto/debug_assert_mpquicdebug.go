//go:build mpquicdebug

package crypto

import "fmt"

// debugPanicf panica con il messaggio formattato.
// Attivo solo con il build tag `mpquicdebug` per segnalare violazioni di
// precondizioni in sviluppo senza overhead in produzione.
func debugPanicf(format string, args ...any) {
	panic(fmt.Sprintf(format, args...))
}
