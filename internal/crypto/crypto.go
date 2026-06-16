// Package crypto provides AES-256-GCM sealing for credential fields.
// The nonce is randomly generated per call and prepended to the ciphertext.
package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"fmt"

	"backuprepo/internal/apperr"
)

// KeySize is the required master-key length in bytes (AES-256).
const KeySize = 32

// Seal encrypts plaintext with key, returning nonce||ciphertext.
func Seal(key, plaintext []byte) ([]byte, error) {
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("%w: nonce: %v", apperr.ErrCrypto, err)
	}
	return gcm.Seal(nonce, nonce, plaintext, nil), nil
}

// Open decrypts nonce||ciphertext produced by Seal.
func Open(key, data []byte) ([]byte, error) {
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	ns := gcm.NonceSize()
	if len(data) < ns {
		return nil, fmt.Errorf("%w: ciphertext too short", apperr.ErrCrypto)
	}
	plain, err := gcm.Open(nil, data[:ns], data[ns:], nil)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", apperr.ErrCrypto, err)
	}
	return plain, nil
}

// newGCM builds an AES-256-GCM AEAD from key, rejecting any key that isn't KeySize bytes.
func newGCM(key []byte) (cipher.AEAD, error) {
	if len(key) != KeySize {
		return nil, fmt.Errorf("%w: key must be %d bytes, got %d", apperr.ErrCrypto, KeySize, len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", apperr.ErrCrypto, err)
	}
	return cipher.NewGCM(block)
}
