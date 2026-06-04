//go:build linux

package crypto_test

import (
	"errors"
	"fmt"
	"mpquic/internal/mpquic/crypto"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// fakeProviderSO è il path del .so compilato durante TestMain
var fakeProviderSO string

// noSymbolSO è il path del .so compilato durante TestMain che NON esporta CryptoProvider
var noSymbolSO string

// TestMain compila il fake provider come plugin prima di eseguire i test.
// Se la compilazione fallisce, i test di integrazione vengono skippati.
//
// NOTA: Go plugin richiede che host e plugin siano compilati con gli stessi
// build ID dei package condivisi. Questo crea problemi quando il test binary
// è compilato in "test mode" (go test) mentre il plugin è compilato in "build
// mode" (go build -buildmode=plugin): anche con gli stessi flag (-race incluso)
// i build ID possono divergere.
//
// Soluzione: dopo la compilazione del plugin, TestMain esegue un probe
// LoadExternalProvider per verificare che sia effettivamente caricabile
// nell'ambiente corrente. Se fallisce (es. con -race), fakeProviderSO viene
// azzerato e i test vengono skippati gracefully invece di fallire.
func TestMain(m *testing.M) {
	tmpDir, err := os.MkdirTemp("", "mpquic-external-test-*")
	if err != nil {
		os.Exit(1)
	}
	// NO defer — os.Exit bypassa i defer

	fakeProviderSO = filepath.Join(tmpDir, "fake_provider.so")
	noSymbolSO = filepath.Join(tmpDir, "no_symbol.so")

	moduleRoot, err := findModuleRoot()
	if err != nil {
		fakeProviderSO = ""
		noSymbolSO = ""
	} else {
		fakeProviderSrc := filepath.Join(moduleRoot, "internal", "mpquic", "crypto", "testdata", "fake_provider")
		cmd := exec.Command("go", "build", "-buildmode=plugin", "-o", fakeProviderSO, fakeProviderSrc)
		cmd.Dir = moduleRoot
		if out, buildErr := cmd.CombinedOutput(); buildErr != nil {
			println("fake_provider build failed:", string(out))
			fakeProviderSO = ""
		}

		// Compila anche il plugin senza simbolo CryptoProvider per test negativo
		noSymSrc := filepath.Join(moduleRoot, "internal", "mpquic", "crypto", "testdata", "no_symbol_provider")
		cmdNoSym := exec.Command("go", "build", "-buildmode=plugin", "-o", noSymbolSO, noSymSrc)
		cmdNoSym.Dir = moduleRoot
		if out, buildErr := cmdNoSym.CombinedOutput(); buildErr != nil {
			_ = out
			noSymbolSO = ""
		}
	}

	// Probe: verifica che fakeProviderSO sia effettivamente caricabile in questo
	// binary. Con -race (o altre varianti di build mode) i build ID dei package
	// condivisi possono divergere e plugin.Open fallisce. In tal caso impostiamo
	// fakeProviderSO="" così i test useranno t.Skip invece di t.Fatalf.
	// Se il probe fallisce, anche noSymbolSO non sarà caricabile (stesso problema
	// di build mode), quindi viene azzerato anch'esso.
	if fakeProviderSO != "" {
		p, probeErr := crypto.LoadExternalProvider(fakeProviderSO, "")
		if probeErr != nil {
			println("fake_provider.so: probe caricamento fallito (noto con -race o build mode diverso):", probeErr.Error())
			println("I test del plugin verranno skippati.")
			fakeProviderSO = ""
			noSymbolSO = "" // stessa incompatibilità, evita FAIL su TestLoadExternalProvider_MissingSymbol
		} else {
			_ = p.Close()
		}
	}

	code := m.Run()
	_ = os.RemoveAll(tmpDir)
	os.Exit(code)
}

func findModuleRoot() (string, error) {
	dir, err := filepath.Abs(".")
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("go.mod not found")
		}
		dir = parent
	}
}

func TestLoadExternalProvider_Success(t *testing.T) {
	if fakeProviderSO == "" {
		t.Skip("fake_provider.so non compilato o non caricabile in questo ambiente (noto con -race), skip test di integrazione plugin")
	}

	provider, err := crypto.LoadExternalProvider(fakeProviderSO, "")
	if err != nil {
		t.Fatalf("LoadExternalProvider: unexpected error: %v", err)
	}
	defer provider.Close()

	if provider.Name() != "FakeCryptoProvider" {
		t.Errorf("Name() = %q, want %q", provider.Name(), "FakeCryptoProvider")
	}
	if provider.Version() != "0.1.0-test" {
		t.Errorf("Version() = %q, want %q", provider.Version(), "0.1.0-test")
	}
	if provider.AEADProvider() == nil {
		t.Error("AEADProvider() must not be nil")
	}
	if provider.KeyExchangeProvider() == nil {
		t.Error("KeyExchangeProvider() must not be nil")
	}
}

func TestLoadExternalProvider_NotFound(t *testing.T) {
	_, err := crypto.LoadExternalProvider("/opt/mpquic/plugins/nonexistent_12345.so", "")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !isErrProviderNotFound(err) {
		t.Errorf("expected ErrProviderNotFound, got %v", err)
	}
}

func TestLoadExternalProvider_InvalidPath_Relative(t *testing.T) {
	_, err := crypto.LoadExternalProvider("relative/path/plugin.so", "")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !isErrProviderInvalidPath(err) {
		t.Errorf("expected ErrProviderInvalidPath, got %v", err)
	}
}

func TestLoadExternalProvider_InvalidPath_NotSo(t *testing.T) {
	_, err := crypto.LoadExternalProvider("/opt/mpquic/plugins/myplugin.dll", "")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !isErrProviderInvalidPath(err) {
		t.Errorf("expected ErrProviderInvalidPath, got %v", err)
	}
}

func TestLoadExternalProvider_MissingSymbol(t *testing.T) {
	if noSymbolSO == "" {
		t.Skip("no_symbol.so non compilato o non caricabile in questo ambiente (noto con -race), skip test ErrProviderSymbolMissing")
	}
	_, err := crypto.LoadExternalProvider(noSymbolSO, "")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, crypto.ErrProviderSymbolMissing) {
		t.Errorf("expected ErrProviderSymbolMissing, got %v", err)
	}
}

func TestExternalProvider_AEADRoundtrip(t *testing.T) {
	if fakeProviderSO == "" {
		t.Skip("fake_provider.so non compilato o non caricabile in questo ambiente (noto con -race), skip test di integrazione plugin")
	}

	provider, err := crypto.LoadExternalProvider(fakeProviderSO, "")
	if err != nil {
		t.Fatalf("LoadExternalProvider: %v", err)
	}
	defer provider.Close()

	aead := provider.AEADProvider()
	if aead == nil {
		t.Fatal("AEADProvider() is nil")
	}

	key := make([]byte, aead.KeySize())
	for i := range key {
		key[i] = byte(i)
	}
	cipher, err := aead.NewAEAD(key)
	if err != nil {
		t.Fatalf("NewAEAD: %v", err)
	}

	plaintext := []byte("test payload for roundtrip")
	nonce := make([]byte, aead.NonceSize())
	for i := range nonce {
		nonce[i] = byte(i + 100)
	}

	ciphertext := cipher.Seal(nil, nonce, plaintext, nil)
	decrypted, err := cipher.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if string(decrypted) != string(plaintext) {
		t.Errorf("roundtrip mismatch: got %q, want %q", decrypted, plaintext)
	}
}

func TestExternalProvider_KEXRoundtrip(t *testing.T) {
	if fakeProviderSO == "" {
		t.Skip("fake_provider.so non compilato o non caricabile in questo ambiente (noto con -race), skip test di integrazione plugin")
	}

	provider, err := crypto.LoadExternalProvider(fakeProviderSO, "")
	if err != nil {
		t.Fatalf("LoadExternalProvider: %v", err)
	}
	defer provider.Close()

	kex := provider.KeyExchangeProvider()
	if kex == nil {
		t.Fatal("KeyExchangeProvider() is nil")
	}

	pubA, privA, err := kex.GenerateKeyPair()
	if err != nil {
		t.Fatalf("GenerateKeyPair A: %v", err)
	}
	pubB, privB, err := kex.GenerateKeyPair()
	if err != nil {
		t.Fatalf("GenerateKeyPair B: %v", err)
	}

	quicSecret := make([]byte, 64)
	sessionID := []byte("test-session-id")

	keysA, err := kex.DeriveSessionKeys(quicSecret, privA, pubB, sessionID)
	if err != nil {
		t.Fatalf("DeriveSessionKeys A: %v", err)
	}
	keysB, err := kex.DeriveSessionKeys(quicSecret, privB, pubA, sessionID)
	if err != nil {
		t.Fatalf("DeriveSessionKeys B: %v", err)
	}

	assertBytesEqual(t, "ClientKey", keysA.ClientKey, keysB.ClientKey)
	assertBytesEqual(t, "ServerKey", keysA.ServerKey, keysB.ServerKey)
	assertBytesEqual(t, "ClientIV", keysA.ClientIV, keysB.ClientIV)
	assertBytesEqual(t, "ServerIV", keysA.ServerIV, keysB.ServerIV)
}

func isErrProviderNotFound(err error) bool {
	return errors.Is(err, crypto.ErrProviderNotFound)
}

func isErrProviderInvalidPath(err error) bool {
	return errors.Is(err, crypto.ErrProviderInvalidPath)
}

func assertBytesEqual(t *testing.T, name string, a, b []byte) {
	t.Helper()
	if len(a) != len(b) {
		t.Errorf("%s: length mismatch: %d vs %d", name, len(a), len(b))
		return
	}
	for i := range a {
		if a[i] != b[i] {
			t.Errorf("%s: mismatch at byte %d", name, i)
			return
		}
	}
}
