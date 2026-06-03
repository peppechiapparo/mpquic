package crypto

// NonceManager gestisce la generazione di nonce unici e non riutilizzati.
// Le implementazioni devono essere lock-free per il hot path.
type NonceManager interface {
	// NextNonce restituisce il prossimo nonce per il workerID specificato.
	// La slice restituita punta a un buffer interno — non deve essere modificata.
	NextNonce(workerID uint) ([]byte, error)
	// Reset azzera i contatori interni. Chiamato ESCLUSIVAMENTE dopo un rekey
	// che ha già cambiato la chiave — mai durante la cifratura.
	Reset()
	// NonceSize restituisce la dimensione del nonce in byte.
	NonceSize() int
}
