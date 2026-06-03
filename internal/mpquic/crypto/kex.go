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

// KemProvider estende KeyExchangeProvider con il supporto per il lato client
// nei Key Encapsulation Mechanism (KEM), necessario per i provider ibridi
// come X25519+ML-KEM-768.
//
// I provider basati su DH classico (es. ClassicalKEXProvider) implementano
// solo KeyExchangeProvider; i provider KEM implementano KemProvider.
// I consumer che richiedono ClientEncapsulate devono fare type assertion:
//
//	if kp, ok := provider.(KemProvider); ok {
//	    localPrivKey, peerKeyShare, err := kp.ClientEncapsulate(serverPubKey)
//	}
type KemProvider interface {
	KeyExchangeProvider
	// ClientEncapsulate prepara il materiale per il lato client del KEX KEM.
	// Deve essere chiamato prima di DeriveSessionKeys sul lato client.
	//
	// serverPubKey: chiave pubblica ibrida del server (X25519_pub || MLKEM_ek).
	// Returns:
	//   localPrivKey  = X25519_priv_client || mlkem_shared (64 bytes)
	//   peerKeyShare  = X25519_pub_client || MLKEM_ciphertext (1120 bytes)
	ClientEncapsulate(serverPubKey []byte) (localPrivKey, peerKeyShare []byte, err error)
}
