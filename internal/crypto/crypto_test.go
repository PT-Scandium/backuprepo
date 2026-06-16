package crypto

import (
	"bytes"
	"testing"
)

// key32 returns a deterministic 32-byte test key.
func key32() []byte {
	k := make([]byte, 32)
	for i := range k {
		k[i] = byte(i)
	}
	return k
}

// TestSealOpenRoundTrip verifies Open recovers the plaintext sealed by Seal and
// that the ciphertext does not leak the plaintext.
func TestSealOpenRoundTrip(t *testing.T) {
	k := key32()
	plain := []byte("hello-secret-key-id")
	ct, err := Seal(k, plain)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if bytes.Contains(ct, plain) {
		t.Fatal("ciphertext leaks plaintext")
	}
	got, err := Open(k, ct)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if !bytes.Equal(got, plain) {
		t.Fatalf("round-trip mismatch: got %q", got)
	}
}

// TestOpenWrongKeyFails verifies Open errors when given a different key than Seal used.
func TestOpenWrongKeyFails(t *testing.T) {
	ct, _ := Seal(key32(), []byte("data"))
	wrong := key32()
	wrong[0] ^= 0xFF
	if _, err := Open(wrong, ct); err == nil {
		t.Fatal("expected error with wrong key")
	}
}

// TestOpenTamperedFails verifies Open errors when the ciphertext has been modified.
func TestOpenTamperedFails(t *testing.T) {
	ct, _ := Seal(key32(), []byte("data"))
	ct[len(ct)-1] ^= 0xFF
	if _, err := Open(key32(), ct); err == nil {
		t.Fatal("expected error with tampered ciphertext")
	}
}

// TestSealRejectsBadKeyLen verifies Seal errors when the key isn't 32 bytes.
func TestSealRejectsBadKeyLen(t *testing.T) {
	if _, err := Seal([]byte("short"), []byte("x")); err == nil {
		t.Fatal("expected error for non-32-byte key")
	}
}
