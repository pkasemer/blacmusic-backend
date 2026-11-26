// Package youtube connects an artist's YouTube channel (read-only) so their
// songs can be discovered and turned into releases. It only ever reads
// metadata and thumbnails through the YouTube Data API; it never fetches audio
// or video.
package youtube

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"errors"
)

// Vault encrypts secrets (OAuth refresh tokens) for storage with AES-256-GCM.
// The stored form is: version byte, 12 byte nonce, ciphertext+tag.
type Vault struct{ aead cipher.AEAD }

const vaultVersion byte = 1

// NewVault creates a vault from a 32 byte key.
func NewVault(key []byte) (*Vault, error) {
	if len(key) != 32 {
		return nil, errors.New("youtube: vault key must be 32 bytes")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Vault{aead: aead}, nil
}

// DevKey derives a key from the JWT secret so development works without extra
// setup. Production must set TOKEN_ENC_KEY.
func DevKey(jwtSecret string) []byte {
	sum := sha256.Sum256([]byte("youtube-token-vault:" + jwtSecret))
	return sum[:]
}

// Seal encrypts plaintext; context (for example the user id) is authenticated
// but not stored, so a ciphertext copied to another row will not open.
func (v *Vault) Seal(plaintext, context string) ([]byte, error) {
	nonce := make([]byte, v.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	out := append([]byte{vaultVersion}, nonce...)
	return v.aead.Seal(out, nonce, []byte(plaintext), []byte(context)), nil
}

// Open decrypts what Seal produced with the same context.
func (v *Vault) Open(sealed []byte, context string) (string, error) {
	ns := v.aead.NonceSize()
	if len(sealed) < 1+ns+v.aead.Overhead() || sealed[0] != vaultVersion {
		return "", errors.New("youtube: malformed sealed value")
	}
	pt, err := v.aead.Open(nil, sealed[1:1+ns], sealed[1+ns:], []byte(context))
	if err != nil {
		return "", errors.New("youtube: cannot decrypt (wrong key or context)")
	}
	return string(pt), nil
}
