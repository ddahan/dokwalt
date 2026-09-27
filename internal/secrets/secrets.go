// Package secrets encrypts config values at rest with XChaCha20-Poly1305.
//
// The key lives in a 0600 file next to the database. This protects config
// values in database copies and backups; it does not protect against root on
// the server, which can read the key (and every container's environment).
package secrets

import (
	"crypto/rand"
	"errors"
	"fmt"
	"os"

	"golang.org/x/crypto/chacha20poly1305"
)

type Box struct {
	key []byte
}

// LoadOrCreate reads the key file, creating it on first use.
func LoadOrCreate(path string) (*Box, error) {
	key, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		key = make([]byte, chacha20poly1305.KeySize)
		if _, err := rand.Read(key); err != nil {
			return nil, err
		}
		if err := os.WriteFile(path, key, 0o600); err != nil {
			return nil, err
		}
		return &Box{key: key}, nil
	}
	if err != nil {
		return nil, err
	}
	if len(key) != chacha20poly1305.KeySize {
		return nil, fmt.Errorf("%s: invalid key size %d", path, len(key))
	}
	return &Box{key: key}, nil
}

// NewForTest returns a box with a random in-memory key.
func NewForTest() *Box {
	key := make([]byte, chacha20poly1305.KeySize)
	_, _ = rand.Read(key)
	return &Box{key: key}
}

func (b *Box) Seal(plain []byte) ([]byte, error) {
	aead, err := chacha20poly1305.NewX(b.key)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, aead.NonceSize(), aead.NonceSize()+len(plain)+aead.Overhead())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return aead.Seal(nonce, nonce, plain, nil), nil
}

func (b *Box) Open(sealed []byte) ([]byte, error) {
	aead, err := chacha20poly1305.NewX(b.key)
	if err != nil {
		return nil, err
	}
	if len(sealed) < aead.NonceSize() {
		return nil, errors.New("secrets: ciphertext too short")
	}
	nonce, ct := sealed[:aead.NonceSize()], sealed[aead.NonceSize():]
	return aead.Open(nil, nonce, ct, nil)
}
