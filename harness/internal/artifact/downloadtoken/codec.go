// Package downloadtoken mints and redeems stateless, encrypted artifact
// download tokens. A token is an AES-256-GCM sealed blob carrying the object
// storage key plus an expiry, so it can be converted back to exactly one
// artifact without any server-side lookup table. The storage key only ever
// appears as ciphertext, so the token leaks no physical-storage markers.
//
// This package imports only the standard library so both the artifact store
// (redemption) and the object stores (issuance) can depend on it without an
// import cycle.
package downloadtoken

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"time"
)

const (
	tokenVersion   byte = 1
	timestampBytes      = 8
)

var tokenAAD = []byte("harness:artifact-download:v1")

// MaxEncodedTokenBytes bounds untrusted bearer-token decoding before base64
// allocates memory. Canonical Artifact storage keys fit comfortably below it.
const MaxEncodedTokenBytes = 4096

// ErrInvalidToken is returned when a token cannot be authenticated or decoded.
// It deliberately carries no detail so callers cannot distinguish tampering
// from truncation from a wrong key.
var ErrInvalidToken = errors.New("invalid download token")

// Codec seals and opens download tokens with a single AEAD key. It is safe for
// concurrent use.
type Codec struct {
	aead cipher.AEAD
}

// New derives a 256-bit key from secret (SHA-256) and returns a Codec backed by
// AES-256-GCM. Any non-empty secret is accepted; deriving the key means callers
// need not supply exactly 32 bytes.
func New(secret []byte) (*Codec, error) {
	if len(secret) == 0 {
		return nil, errors.New("download token secret is required")
	}
	key := sha256.Sum256(secret)
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Codec{aead: aead}, nil
}

// Seal encrypts payload together with expiresAt into an opaque, URL-safe token.
// A fresh random nonce is used every call, so the same payload yields a
// different token each time (one artifact -> many tokens).
func (c *Codec) Seal(payload string, expiresAt time.Time) (string, error) {
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	plaintext := make([]byte, 1+timestampBytes+len(payload))
	plaintext[0] = tokenVersion
	binary.BigEndian.PutUint64(plaintext[1:1+timestampBytes], uint64(expiresAt.UnixNano()))
	copy(plaintext[1+timestampBytes:], payload)
	sealed := c.aead.Seal(nonce, nonce, plaintext, tokenAAD)
	return base64.RawURLEncoding.EncodeToString(sealed), nil
}

// Open authenticates and decrypts a token, returning the sealed payload and its
// expiry. It returns ErrInvalidToken for any decode/authentication failure and
// does not itself enforce expiry — callers compare expiresAt to their own clock.
func (c *Codec) Open(token string) (payload string, expiresAt time.Time, err error) {
	if len(token) == 0 || len(token) > MaxEncodedTokenBytes {
		return "", time.Time{}, ErrInvalidToken
	}
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return "", time.Time{}, ErrInvalidToken
	}
	nonceSize := c.aead.NonceSize()
	if len(raw) < nonceSize {
		return "", time.Time{}, ErrInvalidToken
	}
	nonce, ciphertext := raw[:nonceSize], raw[nonceSize:]
	plaintext, err := c.aead.Open(nil, nonce, ciphertext, tokenAAD)
	if err != nil {
		return "", time.Time{}, ErrInvalidToken
	}
	if len(plaintext) < 1+timestampBytes || plaintext[0] != tokenVersion {
		return "", time.Time{}, ErrInvalidToken
	}
	expiresAt = time.Unix(0, int64(binary.BigEndian.Uint64(plaintext[1:1+timestampBytes])))
	return string(plaintext[1+timestampBytes:]), expiresAt, nil
}
