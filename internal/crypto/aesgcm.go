package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
)

// AADPrefix is bound into every encrypted column so that ciphertext from
// one column cannot be transplanted into another and decrypted there.
//
// The value is intentionally kept as the historical "webshare-proxy/v1/"
// (not renamed to PIO) so that data encrypted before the project rename
// stays decryptable in place — no re-encryption/migration of existing
// data.db files is required. It is an opaque crypto domain separator with
// no user-facing surface.
const AADPrefix = "webshare-proxy/v1/"

// ColumnAAD returns the canonical AAD for an encrypted column.
// Example: ColumnAAD("api_keys.encrypted_key") -> []byte("webshare-proxy/v1/api_keys.encrypted_key").
func ColumnAAD(columnName string) []byte {
	return []byte(AADPrefix + columnName)
}

// ErrInvalidKey is returned by Encrypt/Decrypt when the master key is not 32 bytes.
var ErrInvalidKey = errors.New("crypto: master key must be 32 bytes (AES-256)")

// Encrypt seals plaintext with AES-256-GCM using key (32 bytes). Output layout:
//
//	nonce(12) || ciphertext || tag(16)
//
// AAD is required and bound to the ciphertext; supplying a different AAD at
// Decrypt time produces an authentication failure.
func Encrypt(key, plaintext, aad []byte) ([]byte, error) {
	if len(key) != 32 {
		return nil, ErrInvalidKey
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("aes new cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("gcm new: %w", err)
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("nonce read: %w", err)
	}
	out := make([]byte, 0, len(nonce)+len(plaintext)+aead.Overhead())
	out = append(out, nonce...)
	out = aead.Seal(out, nonce, plaintext, aad)
	return out, nil
}

// Decrypt reverses Encrypt. It returns an error if the AAD does not match
// the value supplied at encryption time, if the ciphertext is truncated,
// or if any byte (including the tag) has been modified.
func Decrypt(key, blob, aad []byte) ([]byte, error) {
	if len(key) != 32 {
		return nil, ErrInvalidKey
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("aes new cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("gcm new: %w", err)
	}
	ns := aead.NonceSize()
	if len(blob) < ns+aead.Overhead() {
		return nil, errors.New("crypto: ciphertext too short")
	}
	nonce, ct := blob[:ns], blob[ns:]
	pt, err := aead.Open(nil, nonce, ct, aad)
	if err != nil {
		return nil, fmt.Errorf("gcm open: %w", err)
	}
	return pt, nil
}
