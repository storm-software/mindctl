package contentcrypto

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"errors"
	"strings"
	"sync"
	"testing"
)

func TestKeyringEncryptsWithUniqueNoncesAndRotates(t *testing.T) {
	oldKey, newKey := bytes.Repeat([]byte{1}, 32), bytes.Repeat([]byte{2}, 32)
	oldRing := mustKeyring(t, "old", map[string][]byte{"old": oldKey})
	first := mustEncrypt(t, oldRing, []byte("secret prompt"))
	rotated := mustKeyring(t, "new", map[string][]byte{"old": oldKey, "new": newKey})
	second := mustEncrypt(t, rotated, []byte("secret prompt"))
	if first.Version != 1 || second.Version != 1 || first.KeyID != "old" || second.KeyID != "new" {
		t.Fatal("wrong envelope version or active key ID")
	}
	for _, envelope := range []Envelope{first, second} {
		got, err := rotated.Decrypt(envelope)
		if err != nil || !bytes.Equal(got, []byte("secret prompt")) {
			t.Fatal("rotation round trip failed")
		}
		if bytes.Contains(envelope.Ciphertext, []byte("secret prompt")) {
			t.Fatal("plaintext leaked")
		}
	}
	seen := map[string]bool{string(second.Nonce): true}
	for range 128 {
		envelope := mustEncrypt(t, rotated, []byte("secret prompt"))
		if len(envelope.Nonce) != 12 || seen[string(envelope.Nonce)] {
			t.Fatal("nonce is the wrong size or reused with the same key")
		}
		seen[string(envelope.Nonce)] = true
	}
}

func TestNewRejectsInvalidConfiguration(t *testing.T) {
	valid := bytes.Repeat([]byte{1}, 32)
	tests := []struct {
		name   string
		active string
		keys   map[string][]byte
	}{
		{"empty active", "", map[string][]byte{"": valid}},
		{"missing active", "active", map[string][]byte{"old": valid}},
		{"nil keys", "active", nil},
		{"nil key", "active", map[string][]byte{"active": nil}},
		{"AES-128 key", "active", map[string][]byte{"active": make([]byte, 16)}},
		{"AES-192 key", "active", map[string][]byte{"active": make([]byte, 24)}},
		{"oversize key", "active", map[string][]byte{"active": make([]byte, 33)}},
		{"invalid old key", "active", map[string][]byte{"active": valid, "old": make([]byte, 31)}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if ring, err := New(tt.active, tt.keys); err == nil || ring != nil {
				t.Fatal("invalid configuration produced a keyring")
			}
		})
	}
}

func TestKeyringCopiesCallerKeys(t *testing.T) {
	key := bytes.Repeat([]byte{7}, 32)
	keys := map[string][]byte{"active": key}
	ring := mustKeyring(t, "active", keys)
	independent := mustKeyring(t, "active", map[string][]byte{"active": bytes.Clone(key)})
	before := mustEncrypt(t, ring, []byte("secret prompt"))
	clear(key)
	delete(keys, "active")
	keys["replacement"] = bytes.Repeat([]byte{9}, 32)
	after := mustEncrypt(t, ring, []byte("secret prompt"))
	for _, envelope := range []Envelope{before, after} {
		for _, reader := range []*Keyring{ring, independent} {
			got, err := reader.Decrypt(envelope)
			if err != nil || !bytes.Equal(got, []byte("secret prompt")) {
				t.Fatal("caller mutation changed the keyring")
			}
		}
	}
}

func TestKeyringUsesAES256GCMWithKeyIDAAD(t *testing.T) {
	key := bytes.Repeat([]byte{3}, 32)
	ring := mustKeyring(t, "active", map[string][]byte{"active": key})
	envelope := mustEncrypt(t, ring, []byte("secret prompt"))
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatal("could not create independent AES cipher")
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal("could not create independent GCM cipher")
	}
	got, err := aead.Open(nil, envelope.Nonce, envelope.Ciphertext, []byte("active"))
	if err != nil || !bytes.Equal(got, []byte("secret prompt")) {
		t.Fatal("envelope is not AES-256-GCM with key ID AAD")
	}
	if len(envelope.Ciphertext) != len("secret prompt")+16 {
		t.Fatal("envelope does not contain a full authentication tag")
	}
}

func TestDecryptRejectsMalformedAndTamperedEnvelopes(t *testing.T) {
	key := bytes.Repeat([]byte{4}, 32)
	ring := mustKeyring(t, "active", map[string][]byte{"active": key, "alias": key})
	tests := []struct {
		name   string
		mutate func(*Envelope)
	}{
		{"zero version", func(e *Envelope) { e.Version = 0 }},
		{"future version", func(e *Envelope) { e.Version = 2 }},
		{"empty key ID", func(e *Envelope) { e.KeyID = "" }},
		{"unknown key ID", func(e *Envelope) { e.KeyID = "unknown" }},
		{"key ID authentication", func(e *Envelope) { e.KeyID = "alias" }},
		{"nil nonce", func(e *Envelope) { e.Nonce = nil }},
		{"short nonce", func(e *Envelope) { e.Nonce = e.Nonce[:11] }},
		{"long nonce", func(e *Envelope) { e.Nonce = append(e.Nonce, 0) }},
		{"tampered nonce", func(e *Envelope) { e.Nonce[0] ^= 1 }},
		{"nil ciphertext", func(e *Envelope) { e.Ciphertext = nil }},
		{"short tag", func(e *Envelope) { e.Ciphertext = e.Ciphertext[:15] }},
		{"tampered ciphertext", func(e *Envelope) { e.Ciphertext[0] ^= 1 }},
		{"tampered tag", func(e *Envelope) { e.Ciphertext[len(e.Ciphertext)-1] ^= 1 }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			envelope := mustEncrypt(t, ring, []byte("secret prompt"))
			tt.mutate(&envelope)
			if got, err := ring.Decrypt(envelope); err == nil || got != nil {
				t.Fatal("invalid envelope did not fail without plaintext")
			}
		})
	}
	wrongKey := mustKeyring(t, "active", map[string][]byte{"active": bytes.Repeat([]byte{5}, 32)})
	if got, err := wrongKey.Decrypt(mustEncrypt(t, ring, []byte("secret prompt"))); err == nil || got != nil {
		t.Fatal("wrong key did not fail without plaintext")
	}
}

func TestUnknownKeyErrorIsTypedAndDoesNotExposeEnvelope(t *testing.T) {
	ring := mustKeyring(t, "active", map[string][]byte{"active": bytes.Repeat([]byte("key material"), 3)[:32]})
	envelope := Envelope{Version: 1, KeyID: "secret prompt", Nonce: []byte("nonce bytes"), Ciphertext: []byte("ciphertext bytes")}
	got, err := ring.Decrypt(envelope)
	var unknown *UnknownKeyError
	if got != nil || !errors.As(err, &unknown) {
		t.Fatal("unknown key did not return a typed error without plaintext")
	}
	for _, secret := range []string{"secret prompt", "nonce bytes", "ciphertext bytes", "key material"} {
		if strings.Contains(err.Error(), secret) {
			t.Fatal("unknown-key error exposed caller data")
		}
	}
}

func TestKeyringDoesNotAliasPlaintextOrEnvelope(t *testing.T) {
	ring := mustKeyring(t, "active", map[string][]byte{"active": bytes.Repeat([]byte{6}, 32)})
	plaintext := []byte("secret prompt")
	envelope := mustEncrypt(t, ring, plaintext)
	clear(plaintext)
	got, err := ring.Decrypt(envelope)
	if err != nil || !bytes.Equal(got, []byte("secret prompt")) {
		t.Fatal("encrypt retained a plaintext alias")
	}
	clear(got)
	got, err = ring.Decrypt(envelope)
	if err != nil || !bytes.Equal(got, []byte("secret prompt")) {
		t.Fatal("decrypt returned an envelope alias")
	}
	clear(envelope.Ciphertext)
	clear(envelope.Nonce)
	if !bytes.Equal(got, []byte("secret prompt")) {
		t.Fatal("envelope mutation changed returned plaintext")
	}
}

func TestKeyringRoundTripsEmptyAndBinaryContent(t *testing.T) {
	ring := mustKeyring(t, "active", map[string][]byte{"active": bytes.Repeat([]byte{8}, 32)})
	for _, plaintext := range [][]byte{nil, {}, {0, 255, 128, 1, 0}} {
		envelope := mustEncrypt(t, ring, plaintext)
		got, err := ring.Decrypt(envelope)
		if err != nil || !bytes.Equal(got, plaintext) {
			t.Fatal("empty or binary content did not round trip")
		}
	}
}

func TestKeyringConcurrentEncryptDecrypt(t *testing.T) {
	key := bytes.Repeat([]byte{9}, 32)
	ring := mustKeyring(t, "active", map[string][]byte{"active": key})
	shared := mustEncrypt(t, ring, []byte("secret prompt"))
	var workers sync.WaitGroup
	for range 32 {
		workers.Go(func() {
			for range 32 {
				envelope, err := ring.Encrypt([]byte("secret prompt"))
				if err != nil {
					t.Error("concurrent encrypt failed")
					return
				}
				for _, input := range []Envelope{shared, envelope} {
					got, err := ring.Decrypt(input)
					if err != nil || !bytes.Equal(got, []byte("secret prompt")) {
						t.Error("concurrent decrypt failed")
						return
					}
				}
			}
		})
	}
	workers.Wait()
}

func mustKeyring(t *testing.T, active string, keys map[string][]byte) *Keyring {
	t.Helper()
	ring, err := New(active, keys)
	if err != nil {
		t.Fatal("valid keyring creation failed")
	}
	return ring
}

func mustEncrypt(t *testing.T, ring *Keyring, plaintext []byte) Envelope {
	t.Helper()
	envelope, err := ring.Encrypt(plaintext)
	if err != nil {
		t.Fatal("encrypt failed")
	}
	return envelope
}
