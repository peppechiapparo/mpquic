package crypto

// KeyExchangeProvider astrae la logica di key exchange.
// Non sostituisce il handshake QUIC — deriva le chiavi STRIPES sopra
// il secret già negoziato da QUIC TLS Exporter.
type KeyExchangeProvider interface {
	Name() string
	GenerateKeyPair() (publicKey, privateKey []byte, err error)
	// DeriveSessionKeys deriva le SessionKeys operative dalla combinazione di:
	//   quicSecret: output QUIC TLS Exporter (64 byte)
	//   localPrivKey: chiave privata locale
	//   remotePubKey: chiave pubblica del peer
	//   sessionID: identificatore univoco sessione
	DeriveSessionKeys(quicSecret, localPrivKey, remotePubKey, sessionID []byte) (*SessionKeys, error)
}
