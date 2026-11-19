// Package secretbox encrypts small secrets (courier API credentials) at rest
// with AES-256-GCM and builds keyed fingerprints for duplicate detection.
//
// Sealed format: "v1.<keyID>.<base64url(nonce || ciphertext)>". The key ID
// lets you rotate: new data is sealed with the active key, old data still
// opens with any key listed as previous.
//
// Every Seal/Open call takes "additional authenticated data" (AAD). Callers
// bind the seller, connection and provider into it, so a ciphertext copied
// from one document to another (or another seller's) fails to open.
package secretbox

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

const (
	version = "v1"

	EnvKey         = "COURIER_ENCRYPTION_KEY"
	EnvKeyID       = "COURIER_ENCRYPTION_KEY_ID"
	EnvKeysPrev    = "COURIER_ENCRYPTION_KEYS_PREVIOUS"
	EnvFingerprint = "COURIER_FINGERPRINT_KEY"
)

var (
	ErrMalformed = errors.New("secretbox: malformed ciphertext")
	ErrUnknownID = errors.New("secretbox: unknown key id")
	ErrOpen      = errors.New("secretbox: cannot decrypt")

	keyIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,16}$`)
)

type Box struct {
	activeID string
	keys     map[string][]byte
	fpKey    []byte
}

// New builds a Box. Every key must be exactly 32 bytes and fpKey at least 32.
func New(activeID string, keys map[string][]byte, fpKey []byte) (*Box, error) {
	if len(keys) == 0 {
		return nil, errors.New("secretbox: no keys")
	}
	for id, k := range keys {
		if !keyIDPattern.MatchString(id) {
			return nil, fmt.Errorf("secretbox: invalid key id %q", id)
		}
		if len(k) != 32 {
			return nil, fmt.Errorf("secretbox: key %q must be 32 bytes", id)
		}
	}
	if _, ok := keys[activeID]; !ok {
		return nil, fmt.Errorf("secretbox: active key id %q not found", activeID)
	}
	if len(fpKey) < 32 {
		return nil, errors.New("secretbox: fingerprint key must be at least 32 bytes")
	}
	cp := make(map[string][]byte, len(keys))
	for id, k := range keys {
		cp[id] = append([]byte(nil), k...)
	}
	return &Box{activeID: activeID, keys: cp, fpKey: append([]byte(nil), fpKey...)}, nil
}

// FromLookup reads the configuration from environment-style lookups.
// It returns (nil, nil) when nothing is configured, so the caller can leave
// the courier feature switched off. A partial or invalid setup is an error.
func FromLookup(lookup func(string) (string, bool)) (*Box, error) {
	get := func(k string) string {
		v, _ := lookup(k)
		return strings.TrimSpace(v)
	}
	raw, fp := get(EnvKey), get(EnvFingerprint)
	if raw == "" && fp == "" {
		return nil, nil
	}
	if raw == "" || fp == "" {
		return nil, fmt.Errorf("%s and %s must both be set", EnvKey, EnvFingerprint)
	}
	activeID := get(EnvKeyID)
	if activeID == "" {
		activeID = "k1"
	}
	key, err := decodeKey(raw)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", EnvKey, err)
	}
	keys := map[string][]byte{activeID: key}
	if prev := get(EnvKeysPrev); prev != "" {
		for _, part := range strings.Split(prev, ",") {
			id, val, ok := strings.Cut(strings.TrimSpace(part), "=")
			if !ok {
				return nil, fmt.Errorf("%s: expected id=base64key pairs", EnvKeysPrev)
			}
			if _, dup := keys[id]; dup {
				return nil, fmt.Errorf("%s: key id %q is used twice", EnvKeysPrev, id)
			}
			k, err := decodeKey(val)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", EnvKeysPrev, err)
			}
			keys[id] = k
		}
	}
	if len(fp) < 32 {
		return nil, fmt.Errorf("%s must be at least 32 characters", EnvFingerprint)
	}
	return New(activeID, keys, []byte(fp))
}

func decodeKey(s string) ([]byte, error) {
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if b, err := enc.DecodeString(s); err == nil {
			if len(b) != 32 {
				return nil, errors.New("must decode to exactly 32 bytes (use: openssl rand -base64 32)")
			}
			return b, nil
		}
	}
	return nil, errors.New("is not valid base64 (use: openssl rand -base64 32)")
}

func (b *Box) gcm(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// Seal encrypts plaintext with the active key.
func (b *Box) Seal(plaintext, aad []byte) (string, error) {
	g, err := b.gcm(b.keys[b.activeID])
	if err != nil {
		return "", err
	}
	nonce := make([]byte, g.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	out := g.Seal(nonce, nonce, plaintext, aad)
	return version + "." + b.activeID + "." + base64.RawURLEncoding.EncodeToString(out), nil
}

func parse(sealed string) (id string, raw []byte, err error) {
	parts := strings.Split(sealed, ".")
	if len(parts) != 3 || parts[0] != version {
		return "", nil, ErrMalformed
	}
	raw, err = base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return "", nil, ErrMalformed
	}
	return parts[1], raw, nil
}

// Open decrypts a value produced by Seal using the same aad.
func (b *Box) Open(sealed string, aad []byte) ([]byte, error) {
	id, raw, err := parse(sealed)
	if err != nil {
		return nil, err
	}
	key, ok := b.keys[id]
	if !ok {
		return nil, ErrUnknownID
	}
	g, err := b.gcm(key)
	if err != nil {
		return nil, err
	}
	if len(raw) < g.NonceSize()+g.Overhead() {
		return nil, ErrMalformed
	}
	pt, err := g.Open(nil, raw[:g.NonceSize()], raw[g.NonceSize():], aad)
	if err != nil {
		return nil, ErrOpen // never include details
	}
	return pt, nil
}

// NeedsRotation reports whether sealed was written with a non-active key.
func (b *Box) NeedsRotation(sealed string) bool {
	id, _, err := parse(sealed)
	return err == nil && id != b.activeID
}

// Fingerprint returns a keyed, one-way identifier for (scope, kind, value).
// It uses its own key, so rotating encryption keys never changes it.
func (b *Box) Fingerprint(scope, kind, value string) string {
	m := hmac.New(sha256.New, b.fpKey)
	for _, p := range []string{scope, kind, value} {
		m.Write([]byte(p))
		m.Write([]byte{0})
	}
	return hex.EncodeToString(m.Sum(nil))
}
