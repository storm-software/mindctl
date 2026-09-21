// Package contentcrypto encrypts captured content before it crosses a storage boundary.
package contentcrypto

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"io"
)

// Envelope stores an authenticated ciphertext and the information needed to decrypt it.
type Envelope struct {
	Version    uint8
	KeyID      string
	Nonce      []byte
	Ciphertext []byte
}

// UnknownKeyError indicates that an envelope's key is absent from the keyring.
// It deliberately contains no caller-controlled envelope data.
type UnknownKeyError struct{}

func (*UnknownKeyError) Error() string {
	return "contentcrypto: unknown key ID"
}

// Keyring is immutable after construction and safe for concurrent use.
// Keep old keys in the configuration until their envelopes no longer need reading.
type Keyring struct {
	active string
	keys   map[string]cipher.AEAD
}

// New builds a keyring using only AES-256 keys. Caller maps and key bytes are not retained.
func New(active string, keys map[string][]byte) (*Keyring, error) {
	if active == "" {
		return nil, errors.New("contentcrypto: active key ID is required")
	}
	if _, ok := keys[active]; !ok {
		return nil, errors.New("contentcrypto: active key is missing")
	}
	decryptors := make(map[string]cipher.AEAD, len(keys))
	for id, key := range keys {
		if len(key) != 32 {
			return nil, errors.New("contentcrypto: keys must be 32 bytes")
		}
		keyCopy := bytes.Clone(key)
		block, err := aes.NewCipher(keyCopy)
		clear(keyCopy)
		if err != nil {
			return nil, errors.New("contentcrypto: cannot initialize AES")
		}
		aead, err := cipher.NewGCM(block)
		if err != nil {
			return nil, errors.New("contentcrypto: cannot initialize GCM")
		}
		decryptors[id] = aead
	}
	return &Keyring{active: active, keys: decryptors}, nil
}

// Encrypt creates a version 1 envelope using the active key and a fresh random nonce.
// The caller must rotate keys before encrypting 2^32 values with any one key.
func (k *Keyring) Encrypt(plaintext []byte) (Envelope, error) {
	aead := k.keys[k.active]
	nonce := make([]byte, aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return Envelope{}, errors.New("contentcrypto: cannot generate nonce")
	}
	return Envelope{
		Version:    1,
		KeyID:      k.active,
		Nonce:      nonce,
		Ciphertext: aead.Seal(nil, nonce, plaintext, []byte(k.active)),
	}, nil
}

// Decrypt authenticates the envelope, including its key ID, before returning content.
// The returned plaintext is independent of the envelope. Every failure returns nil.
func (k *Keyring) Decrypt(envelope Envelope) ([]byte, error) {
	if envelope.Version != 1 {
		return nil, errors.New("contentcrypto: unsupported envelope version")
	}
	if envelope.KeyID == "" {
		return nil, errors.New("contentcrypto: envelope key ID is required")
	}
	aead, ok := k.keys[envelope.KeyID]
	if !ok {
		return nil, &UnknownKeyError{}
	}
	if len(envelope.Nonce) != aead.NonceSize() {
		return nil, errors.New("contentcrypto: invalid nonce size")
	}
	plaintext, err := aead.Open(nil, envelope.Nonce, envelope.Ciphertext, []byte(envelope.KeyID))
	if err != nil {
		return nil, errors.New("contentcrypto: authentication failed")
	}
	return plaintext, nil
}
