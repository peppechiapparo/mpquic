package crypto

import (
	"crypto/ecdh"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
)

const (
	classicalKEXName = "X25519-HKDF-SHA256"
	x25519KeySize    = 32
)

type ClassicalKEXProvider struct{}

func NewClassicalKEXProvider() *ClassicalKEXProvider { return &ClassicalKEXProvider{} }

func (*ClassicalKEXProvider) Name() string { return classicalKEXName }

func (*ClassicalKEXProvider) GenerateKeyPair() (publicKey, privateKey []byte, err error) {
	priv, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("classical kex: X25519 GenerateKey: %w", err)
	}

	pubBytes := priv.PublicKey().Bytes()
	privBytes := priv.Bytes()
	defer zeroize(privBytes)
	if len(pubBytes) != x25519KeySize || len(privBytes) != x25519KeySize {
		return nil, nil, ErrInvalidKeySize
	}

	publicKey = make([]byte, x25519KeySize)
	privateKey = make([]byte, x25519KeySize)
	copy(publicKey, pubBytes)
	copy(privateKey, privBytes)
	return publicKey, privateKey, nil
}

func (*ClassicalKEXProvider) DeriveSessionKeys(quicSecret, localPrivKey, remotePubKey, sessionID []byte) (*SessionKeys, error) {
	if len(sessionID) == 0 {
		return nil, ErrEmptySessionID
	}
	if len(localPrivKey) != x25519KeySize || len(remotePubKey) != x25519KeySize {
		return nil, ErrInvalidKeySize
	}

	privKey, err := ecdh.X25519().NewPrivateKey(localPrivKey)
	if err != nil {
		return nil, fmt.Errorf("classical kex: invalid local private key: %w", err)
	}
	pubKey, err := ecdh.X25519().NewPublicKey(remotePubKey)
	if err != nil {
		return nil, fmt.Errorf("classical kex: invalid remote public key: %w", err)
	}

	sharedSecret, err := privKey.ECDH(pubKey)
	if err != nil {
		return nil, fmt.Errorf("classical kex: X25519 ECDH: %w", err)
	}
	defer zeroize(sharedSecret)
	if len(sharedSecret) != x25519KeySize {
		return nil, ErrKeyMaterial
	}

	keyMat, err := hkdf.Key(sha256.New, sharedSecret, quicSecret, classicalKEXName+"|"+string(sessionID), 88)
	if err != nil {
		return nil, fmt.Errorf("classical kex: HKDF: %w", err)
	}
	defer zeroize(keyMat)
	if len(keyMat) != 88 {
		return nil, ErrKeyMaterial
	}

	keys := &SessionKeys{}
	keys.ClientKey = make([]byte, 32)
	keys.ServerKey = make([]byte, 32)
	keys.ClientIV = make([]byte, 12)
	keys.ServerIV = make([]byte, 12)
	copy(keys.ClientKey, keyMat[0:32])
	copy(keys.ServerKey, keyMat[32:64])
	copy(keys.ClientIV, keyMat[64:76])
	copy(keys.ServerIV, keyMat[76:88])
	return keys, nil
}

var _ KeyExchangeProvider = (*ClassicalKEXProvider)(nil)
