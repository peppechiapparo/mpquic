// Package crypto implementa la Crypto Abstraction Layer per il protocollo STRIPES.
//
// Questo package separa il sottosistema crittografico dal data plane di STRIPES,
// permettendo la selezione a runtime di diversi profili crittografici tramite YAML:
//
//   - ProfilePerformance: AES-256-GCM + X25519 classico (massimo throughput)
//   - ProfileHybridSecurity: AES-256-GCM + X25519 + ML-KEM-768 (post-quantum ready)
//   - ProfileCustomProvider: provider crittografico esterno via plugin Go
//
// Principio architetturale: il codice STRIPES deve dipendere solo dalle interfacce
// definite in questo package, MAI direttamente da librerie crittografiche specifiche.
//
// Riferimenti:
//   - NIST SP 800-38D (AES-GCM)
//   - NIST FIPS 203 (ML-KEM)
//   - RFC 5869 (HKDF)
//   - draft-ietf-tls-hybrid-design
package crypto
