package crypto

// RegisterPrometheus registra i gauge e counter Prometheus per le metriche crypto.
// Deve essere chiamato una sola volta all'avvio, prima di creare sessioni.
// Se prometheus non è disponibile, la funzione è a no-op.
func RegisterPrometheus(reg any, m *CryptoMetrics) {
	// Implementazione da completare nella Fase G
	// Per ora: no-op
	_ = reg
	_ = m
}
