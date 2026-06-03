package crypto

import (
	"crypto/ecdh"
	"crypto/hkdf"
	"crypto/mlkem"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
)

const (
	hybridKEXName = "X25519+ML-KEM-768-HKDF-SHA256"

	mlkem768SeedSize   = 64
	mlkem768EKSize     = 1184
	mlkem768SharedSize = 32
	mlkem768CtSize     = 1088

	hybridPubKeySize     = x25519KeySize + mlkem768EKSize     // 1216
	hybridPrivKeySize    = x25519KeySize + mlkem768SeedSize   // 96
	hybridPeerShareSize  = x25519KeySize + mlkem768CtSize     // 1120
	hybridClientPrivSize = x25519KeySize + mlkem768SharedSize // 64
)

type HybridKEXProvider struct{}

func NewHybridKEXProvider() *HybridKEXProvider { return &HybridKEXProvider{} }

func (*HybridKEXProvider) Name() string { return hybridKEXName }

func (*HybridKEXProvider) GenerateKeyPair() (publicKey, privateKey []byte, err error) {
	// X25519 server keypair
	x25519Priv, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("hybrid kex server: X25519 GenerateKey: %w", err)
	}
	xPubBytes := x25519Priv.PublicKey().Bytes()
	xPrivBytes := x25519Priv.Bytes()
	defer zeroize(xPrivBytes)
	if len(xPubBytes) != x25519KeySize || len(xPrivBytes) != x25519KeySize {
		return nil, nil, ErrInvalidKeySize
	}

	// ML-KEM-768 server keypair
	dk, err := mlkem.GenerateKey768()
	if err != nil {
		return nil, nil, fmt.Errorf("hybrid kex server: ML-KEM GenerateKey768: %w", err)
	}
	ekBytes := dk.EncapsulationKey().Bytes()
	dkSeed := dk.Bytes()
	defer zeroize(dkSeed)
	if len(ekBytes) != mlkem768EKSize || len(dkSeed) != mlkem768SeedSize {
		return nil, nil, ErrInvalidKeySize
	}

	publicKey = make([]byte, hybridPubKeySize)
	privateKey = make([]byte, hybridPrivKeySize)
	copy(publicKey[0:x25519KeySize], xPubBytes)
	copy(publicKey[x25519KeySize:hybridPubKeySize], ekBytes)
	copy(privateKey[0:x25519KeySize], xPrivBytes)
	copy(privateKey[x25519KeySize:hybridPrivKeySize], dkSeed)
	return publicKey, privateKey, nil
}

// ClientEncapsulate prepara il materiale per il lato client del KEX ibrido.
func (*HybridKEXProvider) ClientEncapsulate(serverPubKey []byte) (localPrivKey, peerKeyShare []byte, err error) {
	if len(serverPubKey) != hybridPubKeySize {
		return nil, nil, fmt.Errorf("%w: serverPubKey must be %d bytes", ErrInvalidKeySize, hybridPubKeySize)
	}

	// 1. X25519 client keypair
	x25519Priv, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("hybrid kex client: X25519 GenerateKey: %w", err)
	}
	xPubBytes := x25519Priv.PublicKey().Bytes()
	xPrivBytes := x25519Priv.Bytes()
	defer zeroize(xPrivBytes)
	if len(xPubBytes) != x25519KeySize || len(xPrivBytes) != x25519KeySize {
		return nil, nil, ErrInvalidKeySize
	}

	// 2. ML-KEM encapsulate
	serverMLKEMek := serverPubKey[x25519KeySize:hybridPubKeySize]
	ek, err := mlkem.NewEncapsulationKey768(serverMLKEMek)
	if err != nil {
		return nil, nil, fmt.Errorf("hybrid kex client: invalid server MLKEM ek: %w", err)
	}
	mlkemShared, mlkemCiphertext := ek.Encapsulate()
	defer zeroize(mlkemShared)
	if len(mlkemShared) != mlkem768SharedSize || len(mlkemCiphertext) != mlkem768CtSize {
		return nil, nil, ErrInvalidKeySize
	}

	// 3. localPrivKey = X25519_priv || mlkem_shared
	localPrivKey = make([]byte, hybridClientPrivSize)
	copy(localPrivKey[0:x25519KeySize], xPrivBytes)
	copy(localPrivKey[x25519KeySize:hybridClientPrivSize], mlkemShared)

	// 4. peerKeyShare = X25519_pub_client || MLKEM_ciphertext
	peerKeyShare = make([]byte, hybridPeerShareSize)
	copy(peerKeyShare[0:x25519KeySize], xPubBytes)
	copy(peerKeyShare[x25519KeySize:hybridPeerShareSize], mlkemCiphertext)

	return localPrivKey, peerKeyShare, nil
}

func (*HybridKEXProvider) DeriveSessionKeys(quicSecret, localPrivKey, remotePubKey, sessionID []byte) (*SessionKeys, error) {
	if len(sessionID) == 0 {
		return nil, ErrEmptySessionID
	}
	switch len(localPrivKey) {
	case hybridPrivKeySize:
		return hybridDeriveServerKeys(quicSecret, localPrivKey, remotePubKey, sessionID)
	case hybridClientPrivSize:
		return hybridDeriveClientKeys(quicSecret, localPrivKey, remotePubKey, sessionID)
	default:
		return nil, fmt.Errorf("%w: localPrivKey must be %d (server) or %d (client) bytes, got %d",
			ErrInvalidKeySize, hybridPrivKeySize, hybridClientPrivSize, len(localPrivKey))
	}
}

// hybridDeriveServerKeys implementa DeriveSessionKeys per il ruolo server (decapsulation).
// localPrivKey = X25519_priv (32) || MLKEM_dk_seed (64)
// remotePubKey = X25519_pub_client (32) || MLKEM_ciphertext (1088) = 1120 bytes
func hybridDeriveServerKeys(quicSecret, localPrivKey, remotePubKey, sessionID []byte) (*SessionKeys, error) {
	if len(remotePubKey) != hybridPeerShareSize {
		return nil, ErrInvalidKeySize
	}

	x25519PrivBytes := localPrivKey[0:x25519KeySize]
	mlkemSeed := localPrivKey[x25519KeySize:hybridPrivKeySize]

	x25519PeerPubBytes := remotePubKey[0:x25519KeySize]
	mlkemCiphertext := remotePubKey[x25519KeySize:hybridPeerShareSize]

	privKey, err := ecdh.X25519().NewPrivateKey(x25519PrivBytes)
	if err != nil {
		return nil, fmt.Errorf("hybrid kex server: invalid X25519 private key: %w", err)
	}
	pubKey, err := ecdh.X25519().NewPublicKey(x25519PeerPubBytes)
	if err != nil {
		return nil, fmt.Errorf("hybrid kex server: invalid peer X25519 public key: %w", err)
	}

	sharedX, err := privKey.ECDH(pubKey)
	if err != nil {
		return nil, fmt.Errorf("hybrid kex server: X25519 ECDH: %w", err)
	}
	defer zeroize(sharedX)
	if len(sharedX) != x25519KeySize {
		return nil, ErrKeyMaterial
	}

	dk, err := mlkem.NewDecapsulationKey768(mlkemSeed)
	if err != nil {
		return nil, fmt.Errorf("hybrid kex server: invalid MLKEM seed: %w", err)
	}
	mlkemShared, err := dk.Decapsulate(mlkemCiphertext)
	if err != nil {
		return nil, fmt.Errorf("hybrid kex server: MLKEM decapsulate: %w", err)
	}
	defer zeroize(mlkemShared)
	if len(mlkemShared) != mlkem768SharedSize {
		return nil, ErrKeyMaterial
	}

	ikm := make([]byte, 64)
	defer zeroize(ikm)
	copy(ikm[0:32], sharedX)
	copy(ikm[32:64], mlkemShared)

	keyMat, err := hkdf.Key(sha256.New, ikm, quicSecret, hybridKEXName+"|"+string(sessionID), 88)
	if err != nil {
		return nil, fmt.Errorf("hybrid kex server: HKDF: %w", err)
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

// hybridDeriveClientKeys implementa DeriveSessionKeys per il ruolo client (post-encapsulation).
// localPrivKey = X25519_priv (32) || mlkem_shared_precomputed (32) = 64 bytes
// remotePubKey = X25519_pub_server (esattamente 32 bytes)
func hybridDeriveClientKeys(quicSecret, localPrivKey, remotePubKey, sessionID []byte) (*SessionKeys, error) {
	if len(remotePubKey) != x25519KeySize {
		return nil, ErrInvalidKeySize
	}

	x25519PrivBytes := localPrivKey[0:x25519KeySize]
	mlkemShared := localPrivKey[x25519KeySize:hybridClientPrivSize]

	privKey, err := ecdh.X25519().NewPrivateKey(x25519PrivBytes)
	if err != nil {
		return nil, fmt.Errorf("hybrid kex client: invalid X25519 private key: %w", err)
	}
	pubKey, err := ecdh.X25519().NewPublicKey(remotePubKey[0:x25519KeySize])
	if err != nil {
		return nil, fmt.Errorf("hybrid kex client: invalid server X25519 public key: %w", err)
	}

	sharedX, err := privKey.ECDH(pubKey)
	if err != nil {
		return nil, fmt.Errorf("hybrid kex client: X25519 ECDH: %w", err)
	}
	defer zeroize(sharedX)
	if len(sharedX) != x25519KeySize {
		return nil, ErrKeyMaterial
	}

	ikm := make([]byte, 64)
	defer zeroize(ikm)
	copy(ikm[0:32], sharedX)
	copy(ikm[32:64], mlkemShared)

	keyMat, err := hkdf.Key(sha256.New, ikm, quicSecret, hybridKEXName+"|"+string(sessionID), 88)
	if err != nil {
		return nil, fmt.Errorf("hybrid kex client: HKDF: %w", err)
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

var _ KeyExchangeProvider = (*HybridKEXProvider)(nil)
var _ KemProvider = (*HybridKEXProvider)(nil)
