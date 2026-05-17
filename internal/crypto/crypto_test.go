package crypto

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func mustKey(t *testing.T) []byte {
	t.Helper()
	k := make([]byte, MasterKeySize)
	for i := range k {
		k[i] = byte(i + 1)
	}
	return k
}

func TestEncryptDecryptRoundTrip(t *testing.T) {
	key := mustKey(t)
	pt := []byte("the answer is 42")
	aad := ColumnAAD("api_keys.encrypted_key")

	blob, err := Encrypt(key, pt, aad)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	got, err := Decrypt(key, blob, aad)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if !bytes.Equal(pt, got) {
		t.Fatalf("plaintext mismatch: got %q want %q", got, pt)
	}
}

func TestDecryptRejectsWrongAAD(t *testing.T) {
	key := mustKey(t)
	pt := []byte("hello")
	aadA := ColumnAAD("api_keys.encrypted_key")
	aadB := ColumnAAD("upstream_proxies.encrypted_password")

	blob, err := Encrypt(key, pt, aadA)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	if _, err := Decrypt(key, blob, aadB); err == nil {
		t.Fatal("decrypt with swapped AAD must fail")
	}
}

func TestDecryptRejectsTamperedCiphertext(t *testing.T) {
	key := mustKey(t)
	aad := ColumnAAD("col")
	blob, err := Encrypt(key, []byte("payload"), aad)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	// Flip one bit in the body (past the nonce).
	tampered := append([]byte{}, blob...)
	tampered[len(tampered)-1] ^= 0x01
	if _, err := Decrypt(key, tampered, aad); err == nil {
		t.Fatal("decrypt of tampered ciphertext must fail")
	}
}

func TestDecryptRejectsTruncated(t *testing.T) {
	key := mustKey(t)
	if _, err := Decrypt(key, []byte("short"), ColumnAAD("col")); err == nil {
		t.Fatal("decrypt of too-short blob must fail")
	}
}

func TestEncryptRejectsBadKeyLen(t *testing.T) {
	if _, err := Encrypt(make([]byte, 16), []byte("x"), nil); !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("expected ErrInvalidKey, got %v", err)
	}
	if _, err := Decrypt(make([]byte, 16), make([]byte, 64), nil); !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("expected ErrInvalidKey, got %v", err)
	}
}

func TestNonceUniqueness(t *testing.T) {
	// Two encryptions of the same plaintext must differ (random nonce).
	key := mustKey(t)
	aad := ColumnAAD("col")
	a, err := Encrypt(key, []byte("same"), aad)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Encrypt(key, []byte("same"), aad)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(a, b) {
		t.Fatal("two encryptions of the same plaintext produced identical ciphertext (nonce reuse)")
	}
}

func TestSecretRedaction(t *testing.T) {
	const cleartext = "super-secret-api-key"
	s := Secret(cleartext)

	if got := s.String(); got != redacted {
		t.Fatalf("Secret.String() = %q want %q", got, redacted)
	}
	if got := s.GoString(); got != redacted {
		t.Fatalf("Secret.GoString() = %q want %q", got, redacted)
	}
	// json.Marshal HTML-escapes <,>,& after MarshalJSON returns, so the wire
	// form is "<redacted>". We verify by decoding back to a string
	// and matching the literal, plus checking the cleartext never appears.
	js, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded string
	if err := json.Unmarshal(js, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded != redacted {
		t.Fatalf("MarshalJSON decodes to %q want %q (raw bytes: %s)", decoded, redacted, js)
	}
	if strings.Contains(string(js), cleartext) {
		t.Fatalf("cleartext leaked through direct MarshalJSON: %s", js)
	}
	// Ensure the cleartext does not appear when marshaled inside a struct either.
	wrapped, err := json.Marshal(struct{ K Secret }{K: s})
	if err != nil {
		t.Fatalf("marshal struct: %v", err)
	}
	if strings.Contains(string(wrapped), cleartext) {
		t.Fatalf("cleartext leaked through JSON: %s", wrapped)
	}
	if s.Reveal() != cleartext {
		t.Fatalf("Reveal() lost cleartext: got %q", s.Reveal())
	}
}

func TestLoadOrCreateGeneratesFile(t *testing.T) {
	dir := t.TempDir()
	key, err := LoadOrCreate(dir)
	if err != nil {
		t.Fatalf("LoadOrCreate fresh: %v", err)
	}
	if len(key) != MasterKeySize {
		t.Fatalf("key len = %d want %d", len(key), MasterKeySize)
	}
	path := filepath.Join(dir, MasterKeyName)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if mode := info.Mode().Perm(); mode != MasterKeyMode {
		t.Fatalf("file mode = %o want %o", mode, MasterKeyMode)
	}
}

func TestLoadOrCreateReusesFile(t *testing.T) {
	dir := t.TempDir()
	first, err := LoadOrCreate(dir)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	second, err := LoadOrCreate(dir)
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if !bytes.Equal(first, second) {
		t.Fatal("LoadOrCreate regenerated the key on second call")
	}
}

func TestLoadOrCreateRejectsBadLength(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, MasterKeyName), []byte("short"), MasterKeyMode); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrCreate(dir); err == nil {
		t.Fatal("LoadOrCreate must reject a malformed master key file")
	}
}
